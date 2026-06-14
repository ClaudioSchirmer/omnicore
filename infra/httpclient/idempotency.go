package httpclient

import (
	"context"
	"fmt"
	"net/http"

	"github.com/google/uuid"
)

// AppContextIdempotencyKey is the key used to store the per-call
// idempotency value on the AppContext metadata map. Consumers can read it
// via AppContext.Get for logging or for emitting the same value to a
// downstream pipeline.
const AppContextIdempotencyKey = "httpclient.idempotency-key"

// idempotencyMiddleware injects the per-call idempotency key on the
// outbound request. Runs once per call before retry begins; the header on
// the *http.Request persists across retry attempts, so retried requests
// carry the same key as the original — which is precisely what makes
// POST/PATCH retry safe under an upstream that dedupes on the key.
//
// Source semantics:
//
//   - ctx → generate a UUIDv7 (sortable by timestamp for upstream dedup logs)
//   - explicit → require obs.idempotencyKey (set by CallConfig.IdempotencyKey).
//     Missing key returns *HttpError{Cause: errMissingIdempotencyKey}
//     before delegating.
func idempotencyMiddleware(policy idempotencyPolicy) roundTripper {
	return rtFunc(func(ctx context.Context, req *http.Request, obs *observation, next roundTripper) (*http.Response, error) {
		if !policy.enabled {
			return next.RoundTrip(ctx, req, obs, nil)
		}
		key := obs.idempotencyKey
		if key == "" {
			if policy.source == idempotencyExplicit {
				return nil, &HttpError{
					Method: req.Method,
					URL:    req.URL.String(),
					Cause:  fmt.Errorf("httpclient: %s requires CallConfig.IdempotencyKey (source=explicit)", policy.header),
				}
			}
			generated, err := newIdempotencyKey()
			if err != nil {
				return nil, &HttpError{Method: req.Method, URL: req.URL.String(), Cause: err}
			}
			key = generated
		}
		req.Header.Set(policy.header, key)
		obs.IdempotencyKey = key
		// Propagate the key onto the AppContext metadata so downstream
		// callers (audit, custom middleware) can observe the same value.
		if setter, ok := ctx.(appContextSetter); ok {
			setter.Set(AppContextIdempotencyKey, key)
		}
		return next.RoundTrip(ctx, req, obs, nil)
	})
}

// newIdempotencyKey returns a UUIDv7 string suitable as an idempotency
// header. v7 is preferred because it is sortable by creation time, which
// makes upstream dedup logs ordered and helps with debugging.
func newIdempotencyKey() (string, error) {
	v, err := uuid.NewV7()
	if err != nil {
		return "", fmt.Errorf("httpclient: generate idempotency key: %w", err)
	}
	return v.String(), nil
}

// appContextSetter is the minimal interface the idempotency middleware
// uses to write onto AppContext without taking a hard dependency on the
// application/configuration package. AppContext satisfies it natively.
type appContextSetter interface {
	Set(key string, value any)
}
