package httpclient

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// backoffStrategy is the enum of supported delay curves between retry
// attempts. The strings come from the YAML's `backoff:` field.
type backoffStrategy int

const (
	backoffConstant backoffStrategy = iota + 1
	backoffLinear
	backoffExponential
	backoffExponentialJitter
)

// retryPolicy is the resolved runtime shape consumed by retryMiddleware. It
// is built from RetryConfig at boot, so the request path performs no map
// lookups or string parsing.
type retryPolicy struct {
	maxAttempts       int
	backoff           backoffStrategy
	initialDelay      time.Duration
	maxDelay          time.Duration
	retryOnStatus     map[int]struct{}
	retryOnNetwork    bool
	retryOnTimeout    bool
	retryOnDNS        bool
	respectRetryAfter bool
}

// disabled reports whether the policy is a single-attempt no-op (no retry).
// retryMiddleware short-circuits for disabled policies so the chain skips
// even the body-buffering work below.
func (p retryPolicy) disabled() bool {
	return p.maxAttempts <= 1
}

// retryRand is the shared random source for jitter. Seeded once with the
// process start time and protected by a mutex because math/rand.Source is
// not safe for concurrent use.
var (
	retryRandOnce sync.Once
	retryRandMu   sync.Mutex
	retryRand     *rand.Rand
)

func initRetryRand() {
	retryRandOnce.Do(func() {
		retryRand = rand.New(rand.NewSource(time.Now().UnixNano()))
	})
}

func randJitter(maxNS int64) int64 {
	initRetryRand()
	if maxNS <= 0 {
		return 0
	}
	retryRandMu.Lock()
	defer retryRandMu.Unlock()
	return retryRand.Int63n(maxNS)
}

// retryMiddleware loops next.RoundTrip up to policy.maxAttempts, replaying
// the body from obs.RequestBody on each retry. Sleep between attempts is
// context-aware: a ctx.Done() during backoff aborts retries immediately.
//
// The middleware reads obs.RequestBody (buffered by loggingMiddleware at
// position 2 — guaranteed to have run by the time retry executes at
// position 8). When the request has no body, obs.RequestBody is nil and the
// replay step is a no-op.
func retryMiddleware(policy retryPolicy) roundTripper {
	return rtFunc(func(ctx context.Context, req *http.Request, obs *observation, next roundTripper) (*http.Response, error) {
		// Streaming uploads cannot be retried: the io.Reader / Multipart
		// pipe is one-shot. The first attempt consumes the body; a
		// second attempt would replay an empty body and silently send a
		// broken request. Short-circuit to a single attempt regardless
		// of policy.
		if obs.streamingRequest || policy.disabled() {
			obs.Attempt = 1
			return next.RoundTrip(ctx, req, obs, nil)
		}

		var resp *http.Response
		var err error
		for attempt := 1; attempt <= policy.maxAttempts; attempt++ {
			obs.Attempt = attempt
			if attempt > 1 && len(obs.RequestBody) > 0 {
				req.Body = io.NopCloser(bytes.NewReader(obs.RequestBody))
				req.ContentLength = int64(len(obs.RequestBody))
			}
			resp, err = next.RoundTrip(ctx, req, obs, nil)

			if !shouldRetry(resp, err, policy) {
				return resp, err
			}
			if attempt == policy.maxAttempts {
				return resp, err
			}
			// Drain and close the failing response before sleeping so the
			// connection returns to the pool. Best-effort — the body has
			// already been read by the logging middleware on the way up if
			// it ran; otherwise we discard here.
			if resp != nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}
			wait := computeWait(policy, attempt, resp)
			if !sleepCtx(ctx, wait) {
				return resp, err
			}
		}
		return resp, err
	})
}

// shouldRetry decides whether the (resp, err) outcome qualifies for another
// attempt. Order of precedence:
//
//  1. Caller cancellation → no retry.
//  2. Transport-layer error → consult policy.retryOnNetwork / Timeout / DNS.
//  3. HTTP status → consult policy.retryOnStatus.
func shouldRetry(resp *http.Response, err error, policy retryPolicy) bool {
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return false
		}
		if errors.Is(err, ErrCircuitOpen) {
			// Breaker rejection — burning retry attempts on an open breaker
			// only adds pressure to a recovering upstream once it transitions
			// to half-open. Stop the loop and return the rejection straight
			// to the caller.
			return false
		}
		if errors.Is(err, ErrTokenAcquire) {
			// Auth pre-call failure (Q5 short-circuit) — token acquisition
			// errors don't benefit from retry semantics; the IdP is stuck
			// or credentials are wrong, both of which retry cannot fix.
			return false
		}
		if policy.retryOnTimeout && (errors.Is(err, context.DeadlineExceeded) || isTimeout(err)) {
			return true
		}
		if policy.retryOnDNS && isDNSError(err) {
			return true
		}
		if policy.retryOnNetwork && isNetworkError(err) {
			return true
		}
		return false
	}
	if resp == nil {
		return false
	}
	if _, ok := policy.retryOnStatus[resp.StatusCode]; ok {
		return true
	}
	return false
}

// computeWait applies the backoff curve and the Retry-After hint (when
// honored). The result is always at most policy.maxDelay.
func computeWait(policy retryPolicy, attempt int, resp *http.Response) time.Duration {
	if policy.respectRetryAfter && resp != nil {
		if d, ok := parseRetryAfter(resp.Header.Get("Retry-After")); ok {
			if d > policy.maxDelay {
				d = policy.maxDelay
			}
			return d
		}
	}
	return computeBackoff(policy, attempt)
}

// computeBackoff applies the curve corresponding to policy.backoff for the
// given attempt (1-indexed; sleep happens AFTER the attempt that just
// failed). All curves are capped at policy.maxDelay.
func computeBackoff(policy retryPolicy, attempt int) time.Duration {
	if policy.initialDelay <= 0 {
		return 0
	}
	if attempt < 1 {
		attempt = 1
	}
	var d time.Duration
	switch policy.backoff {
	case backoffConstant:
		d = policy.initialDelay
	case backoffLinear:
		d = policy.initialDelay * time.Duration(attempt)
	case backoffExponential:
		d = policy.initialDelay << uint(attempt-1)
	case backoffExponentialJitter:
		ceiling := policy.initialDelay << uint(attempt-1)
		if ceiling > policy.maxDelay {
			ceiling = policy.maxDelay
		}
		d = time.Duration(randJitter(int64(ceiling)))
	default:
		d = policy.initialDelay
	}
	if d > policy.maxDelay {
		d = policy.maxDelay
	}
	if d < 0 {
		d = policy.maxDelay
	}
	return d
}

// parseRetryAfter accepts the RFC 7231 forms: a positive integer in seconds
// or an HTTP-date. Returns the wait duration and a boolean indicating
// success.
func parseRetryAfter(v string) (time.Duration, bool) {
	if v == "" {
		return 0, false
	}
	if n, err := strconv.Atoi(v); err == nil {
		if n < 0 {
			return 0, false
		}
		return time.Duration(n) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		d := time.Until(t)
		if d < 0 {
			return 0, false
		}
		return d, true
	}
	return 0, false
}

// sleepCtx waits for d unless ctx is canceled. Returns true when the sleep
// completed, false when the context fired (the caller should abort).
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		select {
		case <-ctx.Done():
			return false
		default:
			return true
		}
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// isTimeout matches transport-level timeouts, regardless of how they were
// wrapped (url.Error, net.OpError, plain net.Error).
func isTimeout(err error) bool {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Timeout() {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return false
}

// isDNSError matches DNS lookup failures regardless of wrapper.
func isDNSError(err error) bool {
	var d *net.DNSError
	return errors.As(err, &d)
}

// isNetworkError matches the broad transport failure surface: DNS, OpError,
// any net.Error that is not a timeout. Returns true even for unwrappable
// raw transport errors with no recognized type — once we've ruled out
// timeout and DNS, anything is reasonable to label "network".
func isNetworkError(err error) bool {
	if err == nil {
		return false
	}
	if isDNSError(err) {
		return true
	}
	var oe *net.OpError
	if errors.As(err, &oe) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	return false
}
