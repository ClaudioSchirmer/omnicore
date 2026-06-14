package httpclient

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"
)

// HttpError carries the outcome of an outbound call that didn't succeed with
// a clean 2xx. It is also returned when the response status is in the
// endpoint's acceptableStatus set; Acceptable distinguishes the two cases so
// the caller can branch without inspecting Status directly.
//
// Fields reserved for later phases (Attempt, future cache/breaker state) are
// present today so consumer switch statements survive phase additions
// unchanged. Attempt is always 1 in the current phase (no retry yet).
type HttpError struct {
	Service    string
	Endpoint   string
	Method     string
	URL        string
	Status     int
	Headers    http.Header
	Body       []byte
	Duration   time.Duration
	Cause      error
	Acceptable bool
	Attempt    int
}

// Error renders a concise summary the caller can log without further
// processing. The full URL is included so the failure points at the right
// upstream without consulting external state.
func (e *HttpError) Error() string {
	if e == nil {
		return "<nil HttpError>"
	}
	if e.Cause != nil && e.Status == 0 {
		return fmt.Sprintf("httpclient %s/%s %s %s: %v", e.Service, e.Endpoint, e.Method, e.URL, e.Cause)
	}
	if e.Acceptable {
		return fmt.Sprintf("httpclient %s/%s %s %s: acceptable status %d", e.Service, e.Endpoint, e.Method, e.URL, e.Status)
	}
	return fmt.Sprintf("httpclient %s/%s %s %s: status %d", e.Service, e.Endpoint, e.Method, e.URL, e.Status)
}

// Unwrap exposes the underlying cause for errors.Is / errors.As traversal.
func (e *HttpError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// Sentinel errors. Phase-future ones are declared today so consumer code can
// pattern-match against them ahead of time without compile breaks when those
// phases land.
var (
	// ErrRequestBuild wraps a failure to assemble the *http.Request from
	// the typed request struct (bad tag binding, missing path placeholder
	// value, codec error before dialing).
	ErrRequestBuild = errors.New("httpclient: request build failed")

	// ErrResponseDecode wraps a failure to decode the response body or
	// headers into the typed Resp.
	ErrResponseDecode = errors.New("httpclient: response decode failed")

	// ErrTokenAcquire wraps an auth provider failure that prevented the
	// call from dialing. Phase-future.
	ErrTokenAcquire = errors.New("httpclient: auth token acquisition failed")

	// ErrCircuitOpen is returned when the per-(service, endpoint) breaker
	// is open and rejects the call without dialing. Phase-future.
	ErrCircuitOpen = errors.New("httpclient: circuit open")

	// ErrFakeUnstubbed is returned by the testing fake when a call does not
	// match any registered stub. Wrapped inside *HttpError{Status: 0} so
	// the consumer's normal error-handling path catches it.
	ErrFakeUnstubbed = errors.New("httpclient: fake call did not match any registered stub")
)

// IsAcceptableStatus reports whether err is an HttpError marked Acceptable
// and the response status matches one of the codes. The codes argument lets
// the caller branch on the exact status without inspecting err directly.
//
// Returns false for a nil error or any error that is not *HttpError.
func IsAcceptableStatus(err error, codes ...int) bool {
	var he *HttpError
	if !errors.As(err, &he) {
		return false
	}
	if !he.Acceptable {
		return false
	}
	if len(codes) == 0 {
		return true
	}
	for _, c := range codes {
		if he.Status == c {
			return true
		}
	}
	return false
}

// IsRetriable reports whether err is retriable per the framework's default
// policy: 502/503/504 statuses, network errors (DNS, dial, refused), and
// timeouts. Used by the call surface today only to set the slog observation
// hint; auto-retry arrives in the dedicated phase.
func IsRetriable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		// Caller cancellation is never retriable.
		return false
	}
	var he *HttpError
	if errors.As(err, &he) {
		switch he.Status {
		case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return true
		}
		if he.Status == 0 && he.Cause != nil {
			return causeIsRetriable(he.Cause)
		}
		return false
	}
	return causeIsRetriable(err)
}

// IsCircuitOpen reports whether err carries the ErrCircuitOpen sentinel.
// Always false in the current phase; the helper is here so consumer code can
// branch on it ahead of the breaker phase.
func IsCircuitOpen(err error) bool {
	return errors.Is(err, ErrCircuitOpen)
}

// causeIsRetriable inspects a raw transport error for retriable shapes:
// DNS failures, dial timeouts, connection resets. Tries the common stdlib
// types directly rather than string-matching so the check survives stdlib
// error refactors.
func causeIsRetriable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		if ue.Timeout() {
			return true
		}
		err = ue.Unwrap()
	}
	var ne net.Error
	if errors.As(err, &ne) {
		if ne.Timeout() {
			return true
		}
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	return false
}
