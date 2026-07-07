// Package authcore is the transport-neutral JWT validation core shared by
// the framework's inbound surfaces: the Fiber HTTP middleware
// (web.AuthMiddleware) and the gRPC surface's auth interceptor are both thin
// shells over this package. It owns key sourcing (JWKS endpoint or inline
// PEM), token parsing and claim pinning (alg allowlist, iss/aud, exp/nbf
// with leeway), the optional post-validation revocation check
// (TokenChecker), and Identity construction. It deliberately does NOT own
// transport policy: public-route bypass, tenant enforcement and failure
// rendering stay in each transport shell.
package authcore

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"
)

// Options is the transport-neutral subset of the auth knobs — the mirror of
// web.AuthOptions minus the transport policy fields (PublicRoutes,
// TenantRequired/TenantClaim), which belong to the shells.
type Options struct {
	// Algorithms is the allowlist of accepted `alg` header values.
	// Asymmetric algorithms only — RS256, ES256, EdDSA. Empty falls back to
	// all three.
	Algorithms []string

	// Issuer and Audience pin the expected `iss` and `aud` claims.
	Issuer   string
	Audience string

	// LeewaySeconds is the clock-skew tolerance applied to `exp`/`nbf`.
	LeewaySeconds int

	// JWKSURL points at the IdP's JWKS endpoint (preferred). Mutually
	// exclusive with PublicKeyPEM.
	JWKSURL string

	// PublicKeyPEM is the verification key inline (RSA / ECDSA / Ed25519 in
	// SubjectPublicKeyInfo form). Mutually exclusive with JWKSURL.
	PublicKeyPEM string

	// External, when non-nil, is called after local JWT validation passes
	// (token introspection, RFC 7662 or compatible) so revoked tokens are
	// caught immediately. web.AuthMiddleware plugs its externalValidator in
	// through this seam.
	External TokenChecker
}

// TokenChecker is the post-validation revocation seam.
type TokenChecker interface {
	Validate(ctx context.Context, token string) error
}

// Failure classifies a validation rejection so each transport shell can map
// it to its own notification/status without re-inspecting causes.
type Failure int

const (
	// FailureMissingToken — no usable bearer token in the credential.
	FailureMissingToken Failure = iota
	// FailureExpiredToken — the token parsed but `exp` is in the past.
	FailureExpiredToken
	// FailureInvalidToken — anything else: bad signature, wrong iss/aud,
	// disallowed alg, malformed token, revoked by the external check.
	FailureInvalidToken
)

// Error is the classified validation failure.
type Error struct {
	Failure Failure
	Cause   error
}

func (e *Error) Error() string {
	switch e.Failure {
	case FailureMissingToken:
		return "authcore: missing bearer token"
	case FailureExpiredToken:
		return "authcore: token expired"
	default:
		return "authcore: invalid token"
	}
}

func (e *Error) Unwrap() error { return e.Cause }

// Validator is the reusable validation core. Construct once at boot via New;
// every inbound request calls ValidateAuthorization or ValidateToken.
type Validator struct {
	keyfn      jwt.Keyfunc
	parserOpts []jwt.ParserOption
	external   TokenChecker
}

// New builds the Validator: key source (exactly one of JWKS / PEM), parser
// pinning and the optional external checker. Errors are construction-time
// only (invalid PEM, unreachable JWKS on first fetch).
func New(opts Options) (*Validator, error) {
	keyfn, err := BuildKeyfunc(opts)
	if err != nil {
		return nil, err
	}
	algos := opts.Algorithms
	if len(algos) == 0 {
		algos = []string{"RS256", "ES256", "EdDSA"}
	}
	return &Validator{
		keyfn: keyfn,
		parserOpts: []jwt.ParserOption{
			jwt.WithValidMethods(algos),
			jwt.WithIssuer(opts.Issuer),
			jwt.WithAudience(opts.Audience),
			jwt.WithExpirationRequired(),
			jwt.WithLeeway(time.Duration(opts.LeewaySeconds) * time.Second),
		},
		external: opts.External,
	}, nil
}

// ValidateAuthorization validates a full "Authorization: Bearer <token>"
// header value: extraction, local JWT validation, external revocation check.
// On success returns the Identity and the raw token (for
// AppContext.SetBearerToken).
func (v *Validator) ValidateAuthorization(ctx context.Context, header string) (*configuration.Identity, string, *Error) {
	token, ok := ExtractBearerToken(header)
	if !ok {
		return nil, "", &Error{Failure: FailureMissingToken}
	}
	identity, verr := v.ValidateToken(ctx, token)
	if verr != nil {
		return nil, "", verr
	}
	return identity, token, nil
}

// ValidateToken validates a raw token (no header parsing): local JWT
// validation with the pinned parser options, then the external check.
func (v *Validator) ValidateToken(ctx context.Context, token string) (*configuration.Identity, *Error) {
	claims := jwt.MapClaims{}
	parsed, err := jwt.ParseWithClaims(token, claims, v.keyfn, v.parserOpts...)
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, &Error{Failure: FailureExpiredToken, Cause: err}
		}
		return nil, &Error{Failure: FailureInvalidToken, Cause: err}
	}
	if !parsed.Valid {
		return nil, &Error{Failure: FailureInvalidToken}
	}
	if v.external != nil {
		if err := v.external.Validate(ctx, token); err != nil {
			return nil, &Error{Failure: FailureInvalidToken, Cause: err}
		}
	}
	return BuildIdentity(claims), nil
}

// BuildKeyfunc resolves the verification key source: exactly one of JWKSURL
// (fetched and cached, refreshing on `kid` cache miss) or PublicKeyPEM.
func BuildKeyfunc(opts Options) (jwt.Keyfunc, error) {
	hasJWKS := opts.JWKSURL != ""
	hasPEM := opts.PublicKeyPEM != ""
	if hasJWKS == hasPEM {
		return nil, fmt.Errorf("exactly one of JWKSURL or PublicKeyPEM is required (got jwks=%t, pem=%t)", hasJWKS, hasPEM)
	}
	if hasPEM {
		key, err := ParsePublicKeyPEM([]byte(opts.PublicKeyPEM))
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

// ParsePublicKeyPEM parses a SubjectPublicKeyInfo / "PUBLIC KEY" PEM block
// into the verification key (RSA / ECDSA / Ed25519).
func ParsePublicKeyPEM(b []byte) (any, error) {
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

// ExtractBearerToken parses an "Authorization: Bearer <token>" header value.
// Returns (token, true) when well-formed and non-empty; (_, false) otherwise.
func ExtractBearerToken(header string) (string, bool) {
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

// BuildIdentity materializes the standard claims (sub/iss/exp) plus the raw
// claim map into the application-layer Identity.
func BuildIdentity(claims jwt.MapClaims) *configuration.Identity {
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

// AttributionResult is the outcome of ValidateAttribution: the Identity of
// an AUTHENTIC token (signature/iss/aud verified) and whether it was
// expired at validation time (Stale) — the internal-plane semantics where
// a forwarded bearer is an attribution artifact, not an entry credential.
type AttributionResult struct {
	Identity *configuration.Identity
	Stale    bool
}

// ValidateAttribution validates a bearer for ATTRIBUTION on the trusted
// internal plane: signature, issuer and audience are enforced exactly like
// ValidateToken, but an expired-yet-authentic token still yields the
// Identity (Stale=true) — the user was charged full validation at the main
// door; inside the plane the token answers "who caused this?", not "may
// you enter?". A forged/garbage token is rejected: lying attribution is
// worse than none.
//
// This method NEVER consults the External checker by design — revocation
// introspection belongs to the edge; attribution is local cached-JWKS
// crypto (microseconds, IdP-independent). Construct the internal-plane
// Validator without External for clarity; this method ignores it either
// way.
func (v *Validator) ValidateAttribution(_ context.Context, token string) (AttributionResult, *Error) {
	claims := jwt.MapClaims{}
	parsed, err := jwt.ParseWithClaims(token, claims, v.keyfn, v.parserOpts...)
	if err != nil {
		// jwt/v5 verifies the signature BEFORE claims validation, so an
		// ErrTokenExpired (possibly joined with other claim errors) implies
		// an authentic token. Reject when expiry is accompanied by a
		// signature or iss/aud violation.
		if errors.Is(err, jwt.ErrTokenExpired) &&
			!errors.Is(err, jwt.ErrTokenSignatureInvalid) &&
			!errors.Is(err, jwt.ErrTokenInvalidIssuer) &&
			!errors.Is(err, jwt.ErrTokenInvalidAudience) {
			return AttributionResult{Identity: BuildIdentity(claims), Stale: true}, nil
		}
		return AttributionResult{}, &Error{Failure: FailureInvalidToken, Cause: err}
	}
	if !parsed.Valid {
		return AttributionResult{}, &Error{Failure: FailureInvalidToken}
	}
	return AttributionResult{Identity: BuildIdentity(claims)}, nil
}
