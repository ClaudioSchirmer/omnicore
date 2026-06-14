package httpclient

import (
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/ClaudioSchirmer/omnicore/infra/httpclient/auth"
)

// authMiddleware applies the resolved auth provider to the outbound
// request. Sits at position 3 of the chain — after logging (so the slog
// observation sees the credential headers post-attach) and before signing
// (so an HMAC signature, when it ships, signs the request including the
// auth header).
//
// When provider is nil the middleware short-circuits — services without
// an auth: block simply delegate. Per Q5 of the design's open questions,
// Apply failures (token acquisition errors) return *HttpError{Cause:
// ErrTokenAcquire} so shouldRetry can bail out of the retry loop instead
// of burning attempts on a stuck IdP.
//
// When the provider implements RevocableProvider and revocationOnUnauthorized
// is true, the middleware reacts to a 401 by invalidating the provider's
// cached credential, re-applying, and dispatching one more time. This
// handles the case where the IdP rotated keys or revoked the token while
// the cache still holds the stale value.
func authMiddleware(provider auth.AuthProvider, revocationOnUnauthorized bool) roundTripper {
	return rtFunc(func(ctx context.Context, req *http.Request, obs *observation, next roundTripper) (*http.Response, error) {
		if provider == nil {
			return next.RoundTrip(ctx, req, obs, nil)
		}
		obs.AuthProvider = provider.Name()
		if err := provider.Apply(req); err != nil {
			return nil, &HttpError{
				Method: req.Method,
				URL:    req.URL.String(),
				Cause:  fmt.Errorf("%w: %v", ErrTokenAcquire, err),
			}
		}
		resp, err := next.RoundTrip(ctx, req, obs, nil)
		if err != nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
			return resp, err
		}
		revocable, ok := provider.(auth.RevocableProvider)
		if !ok || !revocationOnUnauthorized {
			return resp, err
		}
		// 401 with revocation enabled — invalidate the cached credential,
		// drain the failing response so the connection returns to the
		// pool, re-apply the provider, and dispatch once more.
		revocable.Invalidate()
		_ = drainAndClose(resp)
		if err := provider.Apply(req); err != nil {
			return nil, &HttpError{
				Method: req.Method,
				URL:    req.URL.String(),
				Cause:  fmt.Errorf("%w: %v", ErrTokenAcquire, err),
			}
		}
		return next.RoundTrip(ctx, req, obs, nil)
	})
}

// drainAndClose consumes the response body and closes it so the connection
// returns to the underlying pool for reuse on the retry attempt.
func drainAndClose(resp *http.Response) error {
	if resp == nil || resp.Body == nil {
		return nil
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.Body.Close()
}
