package web

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/exception"
	"github.com/ClaudioSchirmer/omnicore/application/notifications"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/MicahParks/keyfunc/v3"
	"github.com/gofiber/fiber/v3"
	"github.com/golang-jwt/jwt/v5"
)

// AuthOptions describes the local JWT pre-validation knobs the middleware
// enforces per request. It mirrors the relevant subset of bootstrap.AuthConfig
// in primitive types so the web package stays independent of bootstrap (the
// public-surface dependency would otherwise be cyclic).
//
// The middleware itself is constructed only when authentication is enabled —
// bootstrap.Run skips registration entirely when auth.mode=disabled.
type AuthOptions struct {
	// Algorithms is the allowlist of accepted `alg` header values. Asymmetric
	// algorithms only — RS256, ES256, EdDSA. Empty falls back to all three.
	Algorithms []string

	// Issuer and Audience pin the expected `iss` and `aud` claims; tokens
	// minted for a different IdP or relying party are rejected.
	Issuer   string
	Audience string

	// LeewaySeconds is the clock-skew tolerance applied to `exp`/`nbf` checks.
	LeewaySeconds int

	// JWKSURL points at the IdP's JWKS endpoint (preferred — keyfunc fetches
	// and caches keys, refreshing on `kid` cache miss so rotated keys work
	// without redeploying the service). Mutually exclusive with PublicKeyPEM.
	JWKSURL string

	// PublicKeyPEM is the verification key inline (any of RSA / ECDSA / Ed25519
	// in SubjectPublicKeyInfo / "PUBLIC KEY" form). Mutually exclusive with
	// JWKSURL.
	PublicKeyPEM string

	// PublicRoutes are exact "METHOD /path" entries that bypass the middleware
	// entirely (health probes, login endpoints, etc.).
	PublicRoutes []string

	// ExternalValidator, when non-nil, makes the middleware also call the IdP
	// (token introspection, RFC 7662 or compatible) after local JWT validation
	// passes — so revoked tokens are caught immediately. Optional; absent
	// means local validation is the only check.
	ExternalValidator *ExternalValidatorOptions

	// TenantRequired makes the middleware reject any non-public request whose
	// Identity carries no tenant claim — uniform across the service, no
	// per-route declaration. Drives a 403 TenantMissingNotification right
	// after JWT validation succeeds. Off by default; opt in via
	// auth.authorization.tenant.required: true in the yaml.
	TenantRequired bool

	// TenantClaim mirrors auth.authorization.tenant.claim from the yaml so
	// authOptionsFromConfig has a single destination per knob. The middleware
	// itself reads the tenant value via Identity.TenantID(), which consults
	// the package-level configuration set by bootstrap from the SAME yaml
	// entry — so this field is structural mirror, not an independent source
	// of truth. Empty falls back to the default ("tenant_id").
	TenantClaim string
}

type publicRoute struct {
	method string
	path   string
}

// AuthMiddleware returns a Fiber middleware that validates the bearer JWT on
// every request whose method+path is not in opts.PublicRoutes. On success it
// populates AppContext.Identity with the standard claims (sub/iss/exp) plus
// the raw claim map. On failure it short-circuits the request with the
// canonical envelope carrying one of:
//
//   - MissingAuthorization / InvalidToken / ExpiredToken — 401 Unauthorized
//   - TenantMissingNotification — 403 Forbidden, only when opts.TenantRequired
//     is set AND the validated Identity carries no tenant claim
//
// respondAuthFailure dispatches the HTTP status from each notification's
// own Semantic(), so adding a new failure mode is a notification change,
// not a middleware change.
//
// The function returns an error only at construction time (invalid PEM,
// unreachable JWKS at first fetch, malformed PublicRoutes). Per-request
// errors are rendered to the response, never propagated.
func AuthMiddleware(opts AuthOptions, pipe *pipeline.Pipeline) (fiber.Handler, error) {
	if pipe == nil {
		return nil, errors.New("web: AuthMiddleware requires a non-nil Pipeline (for translation of unauthorized responses)")
	}
	keyfn, err := buildKeyfunc(opts)
	if err != nil {
		return nil, fmt.Errorf("web: AuthMiddleware key source: %w", err)
	}
	routes, err := parsePublicRoutes(opts.PublicRoutes)
	if err != nil {
		return nil, fmt.Errorf("web: AuthMiddleware publicRoutes: %w", err)
	}
	var externalV *externalValidator
	if opts.ExternalValidator != nil {
		externalV, err = newExternalValidator(*opts.ExternalValidator)
		if err != nil {
			return nil, fmt.Errorf("web: AuthMiddleware externalValidator: %w", err)
		}
	}
	algos := opts.Algorithms
	if len(algos) == 0 {
		algos = []string{"RS256", "ES256", "EdDSA"}
	}
	parserOpts := []jwt.ParserOption{
		jwt.WithValidMethods(algos),
		jwt.WithIssuer(opts.Issuer),
		jwt.WithAudience(opts.Audience),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(time.Duration(opts.LeewaySeconds) * time.Second),
	}

	return func(c fiber.Ctx) error {
		if matchPublic(c.Method(), c.Path(), routes) {
			return c.Next()
		}
		token, ok := extractBearerToken(c.Get("Authorization"))
		if !ok {
			return respondAuthFailure(c, pipe, notifications.MissingAuthorizationNotification{})
		}
		claims := jwt.MapClaims{}
		parsed, err := jwt.ParseWithClaims(token, claims, keyfn, parserOpts...)
		if err != nil {
			if errors.Is(err, jwt.ErrTokenExpired) {
				return respondAuthFailure(c, pipe, notifications.ExpiredTokenNotification{})
			}
			return respondAuthFailure(c, pipe, notifications.InvalidTokenNotification{})
		}
		if !parsed.Valid {
			return respondAuthFailure(c, pipe, notifications.InvalidTokenNotification{})
		}
		if externalV != nil {
			if err := externalV.Validate(c.Context(), token); err != nil {
				return respondAuthFailure(c, pipe, notifications.InvalidTokenNotification{})
			}
		}
		appCtx := AppContext(c)
		appCtx.SetBearerToken(token)
		identity := buildIdentity(claims)
		appCtx.SetIdentity(identity)
		if opts.TenantRequired && identity.TenantID() == "" {
			// Reject before the request reaches any handler — the tenant claim
			// is a service-wide property of an authenticated principal. The
			// notification carries SemanticForbidden, so respondAuthFailure
			// emits 403 instead of 401 (it dispatches on Semantic).
			return respondAuthFailure(c, pipe, notifications.TenantMissingNotification{})
		}
		return c.Next()
	}, nil
}

func buildKeyfunc(opts AuthOptions) (jwt.Keyfunc, error) {
	hasJWKS := opts.JWKSURL != ""
	hasPEM := opts.PublicKeyPEM != ""
	if hasJWKS == hasPEM {
		return nil, fmt.Errorf("exactly one of JWKSURL or PublicKeyPEM is required (got jwks=%t, pem=%t)", hasJWKS, hasPEM)
	}
	if hasPEM {
		key, err := parsePublicKeyPEM([]byte(opts.PublicKeyPEM))
		if err != nil {
			return nil, err
		}
		return func(*jwt.Token) (any, error) { return key, nil }, nil
	}
	k, err := keyfunc.NewDefaultCtx(context.Background(), []string{opts.JWKSURL})
	if err != nil {
		return nil, err
	}
	return k.Keyfunc, nil
}

func parsePublicKeyPEM(b []byte) (any, error) {
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, errors.New("no PEM block found")
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse public key: %w", err)
	}
	return key, nil
}

func parsePublicRoutes(items []string) ([]publicRoute, error) {
	out := make([]publicRoute, 0, len(items))
	for _, raw := range items {
		parts := strings.Fields(raw)
		if len(parts) != 2 {
			return nil, fmt.Errorf("publicRoute %q must be \"METHOD /path\"", raw)
		}
		out = append(out, publicRoute{method: strings.ToUpper(parts[0]), path: parts[1]})
	}
	return out, nil
}

func matchPublic(method, path string, routes []publicRoute) bool {
	for _, r := range routes {
		if r.method == method && r.path == path {
			return true
		}
	}
	return false
}

// extractBearerToken parses an "Authorization: Bearer <token>" header.
// Returns (token, true) when the header is well-formed and the token is
// non-empty; (_, false) otherwise.
func extractBearerToken(header string) (string, bool) {
	if header == "" {
		return "", false
	}
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(header[len(prefix):])
	if token == "" {
		return "", false
	}
	return token, true
}

func buildIdentity(claims jwt.MapClaims) *configuration.Identity {
	sub, _ := claims.GetSubject()
	iss, _ := claims.GetIssuer()
	var exp time.Time
	if e, err := claims.GetExpirationTime(); err == nil && e != nil {
		exp = e.Time
	}
	raw := make(map[string]any, len(claims))
	for k, v := range claims {
		raw[k] = v
	}
	return &configuration.Identity{
		Subject:   sub,
		Issuer:    iss,
		ExpiresAt: exp,
		Claims:    raw,
	}
}

// respondAuthFailure renders an unauthorized response with the same shape as
// other framework rejections: NotificationContext "Authorization" carrying a
// single notification, translated against the request's Accept-Language via
// the Pipeline, with Semantic = Unauthorized → 401.
func respondAuthFailure(c fiber.Ctx, pipe *pipeline.Pipeline, n domain.Notification) error {
	ctx := domain.NewNotificationContext("Authorization")
	ctx.AddNotificationMessage(domain.NotificationMessage{Notification: n})
	err := exception.NewApplicationError([]*domain.NotificationContext{ctx})
	result := pipeline.Run(pipe, AppContext(c), func() (any, error) {
		return nil, err
	})
	return RespondFromResult(c, result, fiber.StatusOK)
}
