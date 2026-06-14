package httpclient

import (
	"context"
	"net/http"
)

// breakerMiddleware enforces the circuit breaker for a single
// (service, endpoint) pair. The state is shared across all calls to that
// pair (managed by HttpClient.breakerStore); the middleware itself is
// stateless — it consults state.allow() before delegating and records the
// outcome on the way back up.
//
// Position in the chain: per Q7 in the design open questions, each
// upstream attempt counts as a breaker observation. To honor that,
// breakerMiddleware sits INSIDE retry (later in the layer slice, closer
// to the terminal transport), so the retry loop's repeated dispatch goes
// through the breaker once per attempt.
//
// On rejection the middleware returns *HttpError{Cause: ErrCircuitOpen}
// without delegating; shouldRetry recognizes the sentinel and the retry
// loop terminates immediately rather than burning attempts on an open
// breaker.
func breakerMiddleware(state *breakerState) roundTripper {
	return rtFunc(func(ctx context.Context, req *http.Request, obs *observation, next roundTripper) (*http.Response, error) {
		if state == nil || !state.policy.enabled {
			obs.BreakerState = "closed"
			return next.RoundTrip(ctx, req, obs, nil)
		}
		ok, label := state.allow()
		obs.BreakerState = label
		if !ok {
			return nil, &HttpError{
				Method: req.Method,
				URL:    req.URL.String(),
				Cause:  ErrCircuitOpen,
			}
		}
		resp, err := next.RoundTrip(ctx, req, obs, nil)
		if err != nil {
			state.recordFailure()
			obs.BreakerState = state.snapshotState()
			return resp, err
		}
		if resp != nil && resp.StatusCode >= 500 {
			state.recordFailure()
		} else {
			state.recordSuccess()
		}
		obs.BreakerState = state.snapshotState()
		return resp, err
	})
}
