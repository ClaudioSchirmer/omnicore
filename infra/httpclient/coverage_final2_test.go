package httpclient

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

// HttpError.Error renders each of its four branches.
func TestHttpError_Error_Branches(t *testing.T) {
	var nilErr *HttpError
	if got := nilErr.Error(); got != "<nil HttpError>" {
		t.Errorf("nil = %q", got)
	}
	causeOnly := &HttpError{Service: "s", Endpoint: "e", Method: "GET", URL: "u", Status: 0, Cause: errors.New("boom")}
	if got := causeOnly.Error(); !contains2(got, "boom") {
		t.Errorf("cause branch = %q", got)
	}
	acceptable := &HttpError{Service: "s", Endpoint: "e", Method: "GET", URL: "u", Status: 404, Acceptable: true}
	if got := acceptable.Error(); !contains2(got, "acceptable status 404") {
		t.Errorf("acceptable branch = %q", got)
	}
	plain := &HttpError{Service: "s", Endpoint: "e", Method: "GET", URL: "u", Status: 500}
	if got := plain.Error(); !contains2(got, "status 500") {
		t.Errorf("plain branch = %q", got)
	}
}

// IsRetriable: nil, cancellation, retriable statuses, status-0-with-cause, and
// a non-HttpError transport error.
func TestIsRetriable_Branches(t *testing.T) {
	if IsRetriable(nil) {
		t.Error("nil is not retriable")
	}
	if IsRetriable(context.Canceled) {
		t.Error("cancellation is never retriable")
	}
	for _, st := range []int{http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout} {
		if !IsRetriable(&HttpError{Status: st}) {
			t.Errorf("status %d should be retriable", st)
		}
	}
	if IsRetriable(&HttpError{Status: 400}) {
		t.Error("400 is not retriable")
	}
	// status 0 + cause delegates to causeIsRetriable
	if !IsRetriable(&HttpError{Status: 0, Cause: context.DeadlineExceeded}) {
		t.Error("status-0 with deadline cause should be retriable")
	}
	// raw transport error path
	if !IsRetriable(context.DeadlineExceeded) {
		t.Error("raw deadline error should be retriable")
	}
}

// shouldRetry's sentinel short-circuits and DNS/network branches.
func TestShouldRetry_SentinelsAndNetwork(t *testing.T) {
	p := retryPolicy{retryOnTimeout: true, retryOnNetwork: true, retryOnDNS: true}
	if shouldRetry(nil, ErrCircuitOpen, p) {
		t.Error("ErrCircuitOpen must not retry")
	}
	if shouldRetry(nil, ErrTokenAcquire, p) {
		t.Error("ErrTokenAcquire must not retry")
	}
	// an unrecognized error with no matching policy returns false
	if shouldRetry(nil, errors.New("random"), p) {
		t.Error("unknown error with no network match should not retry")
	}
	// nil resp + nil err
	if shouldRetry(nil, nil, p) {
		t.Error("nil resp + nil err should not retry")
	}
}

// computeBackoff edge cases: zero initial delay, attempt<1 clamp, and the
// default (unknown curve) branch.
func TestComputeBackoff_Edges(t *testing.T) {
	if d := computeBackoff(retryPolicy{initialDelay: 0}, 3); d != 0 {
		t.Errorf("zero initialDelay must yield 0, got %v", d)
	}
	// attempt < 1 clamps to 1; constant curve → initialDelay
	p := retryPolicy{backoff: backoffConstant, initialDelay: 10 * time.Millisecond, maxDelay: time.Second}
	if d := computeBackoff(p, 0); d != 10*time.Millisecond {
		t.Errorf("attempt<1 clamp failed, got %v", d)
	}
	// unknown curve hits the default branch (→ initialDelay)
	def := retryPolicy{backoff: backoffStrategy(99), initialDelay: 7 * time.Millisecond, maxDelay: time.Second}
	if d := computeBackoff(def, 2); d != 7*time.Millisecond {
		t.Errorf("default curve should fall back to initialDelay, got %v", d)
	}
}

// randJitter clamps non-positive bounds to 0 and stays within range.
func TestRandJitter_Bounds(t *testing.T) {
	if randJitter(0) != 0 {
		t.Error("randJitter(0) must be 0")
	}
	if randJitter(-5) != 0 {
		t.Error("randJitter(negative) must be 0")
	}
	for i := 0; i < 50; i++ {
		if v := randJitter(100); v < 0 || v >= 100 {
			t.Fatalf("randJitter(100)=%d out of [0,100)", v)
		}
	}
}

// redactBody / redactPath branches.
func TestRedactBody_Branches(t *testing.T) {
	// empty body / empty paths → verbatim
	if got := redactBody(nil, []string{"$.a"}); got != nil {
		t.Error("empty body should pass through")
	}
	if got := redactBody([]byte(`{"a":1}`), nil); string(got) != `{"a":1}` {
		t.Error("empty paths should pass through")
	}
	// non-JSON → verbatim
	if got := redactBody([]byte("not-json"), []string{"$.a"}); string(got) != "not-json" {
		t.Error("non-JSON should pass through")
	}
	// no path matched → unchanged (mutated=false)
	if got := redactBody([]byte(`{"a":1}`), []string{"$.missing"}); string(got) != `{"a":1}` {
		t.Errorf("unmatched path should pass through, got %s", got)
	}
	// matched leaf → redacted
	out := redactBody([]byte(`{"user":{"password":"secret"}}`), []string{"$.user.password"})
	if !contains2(string(out), redactedPlaceholder) {
		t.Errorf("expected redaction, got %s", out)
	}
}

func TestRedactPath_Branches(t *testing.T) {
	root := map[string]any{"user": map[string]any{"name": "x"}}
	cases := []struct {
		path string
		want bool
	}{
		{"$", false},                  // empty after trim
		{"$.user.name", true},         // hit
		{"$.user.missing", false},     // leaf absent
		{"$.missing.deep", false},     // intermediate absent
		{"$.user.name.deeper", false}, // traverse through a non-map
	}
	for _, tc := range cases {
		// fresh copy so a successful redact doesn't pollute later cases
		r := map[string]any{"user": map[string]any{"name": "x"}}
		if got := redactPath(r, tc.path); got != tc.want {
			t.Errorf("redactPath(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
	_ = root
}

// maxAgeFromCacheControl branches.
func TestMaxAgeFromCacheControl_Branches(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"", 0, false},
		{"no-cache", 0, false},                 // no max-age directive
		{"max-age=60", 60 * time.Second, true}, // hit
		{"public, max-age=30", 30 * time.Second, true},
		{"max-age=abc", 0, false}, // not an int
		{"max-age=-5", 0, false},  // negative
	}
	for _, tc := range cases {
		got, ok := maxAgeFromCacheControl(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("maxAge(%q) = (%v,%v), want (%v,%v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// cloneHeader: empty yields a fresh empty header; populated deep-copies.
func TestCloneHeader_Branches(t *testing.T) {
	if got := cloneHeader(nil); got == nil || len(got) != 0 {
		t.Errorf("empty clone should be a fresh empty header, got %v", got)
	}
	src := http.Header{"X-A": {"1", "2"}}
	out := cloneHeader(src)
	out["X-A"][0] = "mutated"
	if src["X-A"][0] != "1" {
		t.Error("cloneHeader must deep-copy value slices")
	}
}

func contains2(s, sub string) bool {
	return joinHas([]string{s}, sub)
}
