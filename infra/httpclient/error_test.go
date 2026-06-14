package httpclient

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestHttpError_Error_NoCause(t *testing.T) {
	e := &HttpError{Service: "kc", Endpoint: "getUser", Method: "GET", URL: "https://x/u/1", Status: 404}
	s := e.Error()
	if !strings.Contains(s, "kc/getUser") || !strings.Contains(s, "GET") || !strings.Contains(s, "404") {
		t.Errorf("Error() = %q", s)
	}
}

func TestHttpError_Error_AcceptableLabel(t *testing.T) {
	e := &HttpError{Service: "kc", Endpoint: "getUser", Method: "GET", URL: "https://x/u/1", Status: 404, Acceptable: true}
	s := e.Error()
	if !strings.Contains(s, "acceptable") {
		t.Errorf("expected acceptable label in %q", s)
	}
}

func TestHttpError_Error_TransportFailure(t *testing.T) {
	e := &HttpError{Service: "kc", Endpoint: "getUser", Method: "GET", URL: "https://x/u/1", Cause: errors.New("dial failure")}
	s := e.Error()
	if !strings.Contains(s, "dial failure") {
		t.Errorf("expected cause in %q", s)
	}
}

func TestHttpError_Unwrap(t *testing.T) {
	cause := errors.New("inner")
	e := &HttpError{Cause: cause}
	if !errors.Is(e, cause) {
		t.Errorf("errors.Is should match wrapped cause")
	}
}

func TestIsAcceptableStatus_NilErr(t *testing.T) {
	if IsAcceptableStatus(nil, 404) {
		t.Errorf("nil should not be acceptable")
	}
}

func TestIsAcceptableStatus_NotHttpError(t *testing.T) {
	if IsAcceptableStatus(errors.New("nope"), 404) {
		t.Errorf("plain error should not be acceptable")
	}
}

func TestIsAcceptableStatus_NotMarkedAcceptable(t *testing.T) {
	e := &HttpError{Status: 404, Acceptable: false}
	if IsAcceptableStatus(e, 404) {
		t.Errorf("unmarked HttpError should not be acceptable")
	}
}

func TestIsAcceptableStatus_StatusMismatch(t *testing.T) {
	e := &HttpError{Status: 404, Acceptable: true}
	if IsAcceptableStatus(e, 401) {
		t.Errorf("status 404 should not match 401")
	}
}

func TestIsAcceptableStatus_StatusMatch(t *testing.T) {
	e := &HttpError{Status: 404, Acceptable: true}
	if !IsAcceptableStatus(e, 404) {
		t.Errorf("status 404 should match 404")
	}
}

func TestIsAcceptableStatus_NoCodes_DefaultsToAny(t *testing.T) {
	e := &HttpError{Status: 410, Acceptable: true}
	if !IsAcceptableStatus(e) {
		t.Errorf("no codes should accept any acceptable HttpError")
	}
}

func TestIsRetriable_StatusMatrix(t *testing.T) {
	cases := []struct {
		status int
		want   bool
	}{
		{http.StatusBadGateway, true},
		{http.StatusServiceUnavailable, true},
		{http.StatusGatewayTimeout, true},
		{http.StatusInternalServerError, false},
		{http.StatusNotFound, false},
		{http.StatusUnauthorized, false},
	}
	for _, tc := range cases {
		got := IsRetriable(&HttpError{Status: tc.status})
		if got != tc.want {
			t.Errorf("status %d: IsRetriable = %t, want %t", tc.status, got, tc.want)
		}
	}
}

func TestIsRetriable_Nil(t *testing.T) {
	if IsRetriable(nil) {
		t.Error("nil should not be retriable")
	}
}

func TestIsRetriable_ContextCanceled(t *testing.T) {
	if IsRetriable(context.Canceled) {
		t.Error("user cancellation should not be retriable")
	}
}

func TestIsRetriable_TimeoutCause(t *testing.T) {
	e := &HttpError{Cause: context.DeadlineExceeded}
	if !IsRetriable(e) {
		t.Error("deadline exceeded should be retriable")
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestIsRetriable_NetTimeoutErr(t *testing.T) {
	e := &HttpError{Cause: &url.Error{Op: "Get", URL: "x", Err: timeoutErr{}}}
	if !IsRetriable(e) {
		t.Error("url.Error wrapping timeout should be retriable")
	}
}

func TestIsRetriable_DNSError(t *testing.T) {
	e := &HttpError{Cause: &net.DNSError{Err: "no such host"}}
	if !IsRetriable(e) {
		t.Error("DNS error should be retriable")
	}
}

func TestIsCircuitOpen(t *testing.T) {
	if !IsCircuitOpen(ErrCircuitOpen) {
		t.Error("ErrCircuitOpen sentinel should be matched")
	}
	if IsCircuitOpen(errors.New("other")) {
		t.Error("unrelated error should not be circuit-open")
	}
}

// keep stdlib used in case-by-case future additions
var _ = time.Second
