package grpc

import (
	"context"
	"net/http"

	"connectrpc.com/connect"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/notifications"
	"github.com/ClaudioSchirmer/omnicore/web/authcore"
)

// AuthPosture selects the listener's security posture (yaml
// grpc.auth.mode). The gRPC plane exists for trusted internal
// communication — the synchronous sibling of the integration-events
// posture — so besides inheriting the edge rules it can declare the plane
// itself as the trust boundary.
type AuthPosture int

const (
	// PostureInherit — the global auth: block governs (jwt/disabled),
	// exactly like the HTTP surface. The default.
	PostureInherit AuthPosture = iota

	// PostureInternal — the trusted plane: a call WITHOUT a bearer passes
	// as an ANONYMOUS call (the framework's existing vocabulary:
	// persistence.AnonymousActor / audit actor "anonymous"); a bearer, when
	// present, is an ATTRIBUTION artifact — validated locally
	// (signature/iss/aud; expiry tolerated with a stale mark), never
	// against the external validator. Trust = the private network.
	PostureInternal

	// PostureMTLS — PostureInternal plus cryptographic caller proof: the
	// listener requires a client certificate from the internal CA
	// (bootstrap wires the TLS side), and an anonymous call carries a
	// synthetic Identity built from the certificate (Subject = SAN), so
	// the audit trail names the calling service.
	PostureMTLS
)

// attributionStaleClaim is the synthetic claim stamped on an Identity built
// from an expired-but-authentic forwarded token, so the audit trail
// distinguishes a live session from aged propagated context.
const attributionStaleClaim = "attribution"

const attributionStaleValue = "stale-token"

// attributionMTLSValue marks a synthetic Identity built from the client
// certificate (anonymous mTLS call).
const attributionMTLSValue = "mtls-certificate"

// SetAuthPosture arms the internal-plane posture. attribution is the
// LOCAL-ONLY validator for forwarded bearers (constructed by bootstrap
// WITHOUT the external checker — the two structural guarantees: the edge
// validator stays strict and separate; attribution never leaves the
// process). A nil attribution (global auth disabled — dev) makes every
// bearer-carrying call anonymous: nothing can be verified, nothing is
// attributed. Boot-time call, same single-writer contract as the other
// registry knobs.
func (r *Registry) SetAuthPosture(p AuthPosture, attribution *authcore.Validator) {
	r.posture = p
	r.attribution = attribution
}

// internalAuth is the internal/mtls-plane branch of the auth interceptor.
func (r *Registry) internalAuth(ctx context.Context, req connect.AnyRequest, next connect.UnaryFunc) (connect.AnyResponse, error) {
	appCtx := AppContextFrom(ctx)
	header := req.Header().Get("Authorization")
	if header == "" {
		// Anonymous internal call. Under mTLS the connection-level client
		// certificate names the calling service.
		if r.posture == PostureMTLS {
			if san := clientCertIdentityFrom(ctx); san != "" {
				appCtx.SetIdentity(syntheticServiceIdentity(san))
			}
		}
		return next(ctx, req)
	}
	token, ok := authcore.ExtractBearerToken(header)
	if !ok {
		return nil, r.authFailure(appCtx, notifications.InvalidTokenNotification{})
	}
	if r.attribution == nil {
		// No JWT material configured (auth disabled — dev): nothing can be
		// verified, so nothing is attributed; the call proceeds anonymous.
		return next(ctx, req)
	}
	res, verr := r.attribution.ValidateAttribution(ctx, token)
	if verr != nil {
		// Forged/garbage attribution is worse than none — reject loudly.
		return nil, r.authFailure(appCtx, notifications.InvalidTokenNotification{})
	}
	identity := res.Identity
	if res.Stale && identity != nil {
		if identity.Claims == nil {
			identity.Claims = map[string]any{}
		}
		identity.Claims[attributionStaleClaim] = attributionStaleValue
	}
	appCtx.SetBearerToken(token)
	appCtx.SetIdentity(identity)
	return next(ctx, req)
}

// syntheticServiceIdentity is the mTLS anonymous-call Identity: the calling
// service, named by its certificate.
func syntheticServiceIdentity(service string) *configuration.Identity {
	return &configuration.Identity{
		Subject: service,
		Claims:  map[string]any{attributionStaleClaim: attributionMTLSValue},
	}
}

type clientCertKey struct{}

// WithClientCertIdentity is the http-layer companion bootstrap wraps around
// the registry handler when grpc.auth.mode=mtls: it lifts the verified
// client certificate's identity (first DNS SAN, falling back to the
// CommonName) into the request context BEFORE connect takes over — the
// interceptor layer sees headers only, never the TLS state.
func WithClientCertIdentity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.TLS != nil && len(req.TLS.PeerCertificates) > 0 {
			cert := req.TLS.PeerCertificates[0]
			name := cert.Subject.CommonName
			if len(cert.DNSNames) > 0 {
				name = cert.DNSNames[0]
			}
			if name != "" {
				req = req.WithContext(context.WithValue(req.Context(), clientCertKey{}, name))
			}
		}
		next.ServeHTTP(w, req)
	})
}

func clientCertIdentityFrom(ctx context.Context) string {
	if v, ok := ctx.Value(clientCertKey{}).(string); ok {
		return v
	}
	return ""
}
