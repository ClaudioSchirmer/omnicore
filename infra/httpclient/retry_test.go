package httpclient

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"gopkg.in/yaml.v3"
)

// --- backoff math ---------------------------------------------------------

func TestBackoff_Constant(t *testing.T) {
	p := retryPolicy{backoff: backoffConstant, initialDelay: 100 * time.Millisecond, maxDelay: time.Second}
	for n := 1; n <= 5; n++ {
		if got := computeBackoff(p, n); got != 100*time.Millisecond {
			t.Errorf("attempt %d: got %v, want 100ms", n, got)
		}
	}
}

func TestBackoff_Linear(t *testing.T) {
	p := retryPolicy{backoff: backoffLinear, initialDelay: 50 * time.Millisecond, maxDelay: time.Second}
	want := []time.Duration{50 * time.Millisecond, 100 * time.Millisecond, 150 * time.Millisecond, 200 * time.Millisecond}
	for i, w := range want {
		if got := computeBackoff(p, i+1); got != w {
			t.Errorf("attempt %d: got %v, want %v", i+1, got, w)
		}
	}
}

func TestBackoff_Exponential(t *testing.T) {
	p := retryPolicy{backoff: backoffExponential, initialDelay: 50 * time.Millisecond, maxDelay: time.Second}
	want := []time.Duration{50 * time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond}
	for i, w := range want {
		if got := computeBackoff(p, i+1); got != w {
			t.Errorf("attempt %d: got %v, want %v", i+1, got, w)
		}
	}
}

func TestBackoff_CappedAtMaxDelay(t *testing.T) {
	p := retryPolicy{backoff: backoffExponential, initialDelay: 100 * time.Millisecond, maxDelay: 250 * time.Millisecond}
	for n := 5; n <= 10; n++ {
		if got := computeBackoff(p, n); got != 250*time.Millisecond {
			t.Errorf("attempt %d: got %v, want capped at 250ms", n, got)
		}
	}
}

func TestBackoff_JitterStaysWithinBounds(t *testing.T) {
	p := retryPolicy{backoff: backoffExponentialJitter, initialDelay: 100 * time.Millisecond, maxDelay: 1 * time.Second}
	for i := 0; i < 50; i++ {
		got := computeBackoff(p, 3)
		ceiling := 400 * time.Millisecond
		if got < 0 || got > ceiling {
			t.Errorf("jitter attempt 3: got %v, want in [0, %v]", got, ceiling)
		}
	}
}

// --- parseRetryAfter ------------------------------------------------------

func TestParseRetryAfter_Seconds(t *testing.T) {
	d, ok := parseRetryAfter("5")
	if !ok || d != 5*time.Second {
		t.Errorf("got (%v, %v), want (5s, true)", d, ok)
	}
}

func TestParseRetryAfter_HTTPDate(t *testing.T) {
	future := time.Now().Add(2 * time.Second).UTC().Format(http.TimeFormat)
	d, ok := parseRetryAfter(future)
	if !ok || d <= 0 || d > 3*time.Second {
		t.Errorf("got (%v, %v), want positive duration <= 3s", d, ok)
	}
}

func TestParseRetryAfter_Invalid(t *testing.T) {
	for _, in := range []string{"", "abc", "-5"} {
		if _, ok := parseRetryAfter(in); ok {
			t.Errorf("parseRetryAfter(%q) should fail", in)
		}
	}
}

// --- shouldRetry ----------------------------------------------------------

func TestShouldRetry_StatusInRetryOn(t *testing.T) {
	p := retryPolicy{retryOnStatus: map[int]struct{}{503: {}}}
	resp := &http.Response{StatusCode: 503}
	if !shouldRetry(resp, nil, p) {
		t.Error("503 should retry")
	}
	if shouldRetry(&http.Response{StatusCode: 401}, nil, p) {
		t.Error("401 should not retry")
	}
}

func TestShouldRetry_CtxCanceled(t *testing.T) {
	p := retryPolicy{retryOnNetwork: true, retryOnTimeout: true}
	if shouldRetry(nil, context.Canceled, p) {
		t.Error("context.Canceled should not retry")
	}
}

func TestShouldRetry_DeadlineExceededTimeout(t *testing.T) {
	p := retryPolicy{retryOnTimeout: true}
	if !shouldRetry(nil, context.DeadlineExceeded, p) {
		t.Error("DeadlineExceeded should retry when retryOnTimeout")
	}
	pNo := retryPolicy{retryOnTimeout: false}
	if shouldRetry(nil, context.DeadlineExceeded, pNo) {
		t.Error("DeadlineExceeded should not retry when retryOnTimeout=false")
	}
}

// --- validateRetryConfig --------------------------------------------------

func TestValidateRetryConfig_BadBackoff(t *testing.T) {
	cfg := &RetryConfig{Backoff: "random"}
	errs := validateRetryConfig("x", cfg)
	if len(errs) == 0 || !strings.Contains(errs[0], "constant|linear|exponential|exponential-jitter") {
		t.Errorf("expected backoff error; got %v", errs)
	}
}

func TestValidateRetryConfig_NegativeDelays(t *testing.T) {
	cfg := &RetryConfig{InitialDelay: Duration(-1), MaxDelay: Duration(-1), MaxAttempts: -1}
	errs := validateRetryConfig("x", cfg)
	if len(errs) < 3 {
		t.Errorf("expected 3 errors for negative fields; got %v", errs)
	}
}

func TestValidateRetryConfig_RetryOnBadStatus(t *testing.T) {
	cfg := &RetryConfig{RetryOn: []string{"42", "abc"}}
	errs := validateRetryConfig("x", cfg)
	if len(errs) < 2 {
		t.Errorf("expected 2 errors for bad retryOn entries; got %v", errs)
	}
}

func TestValidateRetryConfig_AcceptsSentinels(t *testing.T) {
	cfg := &RetryConfig{RetryOn: []string{"network", "timeout", "dns"}}
	if errs := validateRetryConfig("x", cfg); len(errs) != 0 {
		t.Errorf("sentinels should be accepted; got %v", errs)
	}
}

// --- POST/PATCH gate ------------------------------------------------------

func TestConfig_Validate_POSTRetryRejected(t *testing.T) {
	yml := `
services:
  s:
    baseURL: https://s.example.com
    endpoints:
      e:
        method: POST
        path: /x
        retry:
          maxAttempts: 3
`
	var c Config
	if err := yaml.Unmarshal([]byte(yml), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	c.applyDefaults()
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "POST requires idempotency") {
		t.Errorf("expected POST gate error; got %v", err)
	}
}

func TestConfig_Validate_PATCHRetryRejected(t *testing.T) {
	yml := `
services:
  s:
    baseURL: https://s.example.com
    endpoints:
      e:
        method: PATCH
        path: /x
        retry:
          maxAttempts: 2
`
	var c Config
	if err := yaml.Unmarshal([]byte(yml), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	c.applyDefaults()
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "PATCH requires idempotency") {
		t.Errorf("expected PATCH gate error; got %v", err)
	}
}

func TestConfig_Validate_GETRetryAccepted(t *testing.T) {
	yml := `
services:
  s:
    baseURL: https://s.example.com
    endpoints:
      e:
        method: GET
        path: /x
        retry:
          maxAttempts: 5
          backoff: exponential
`
	var c Config
	if err := yaml.Unmarshal([]byte(yml), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		t.Errorf("GET retry should be accepted; got %v", err)
	}
}

// --- resolveRetryPolicy cascade -------------------------------------------

func TestResolveRetryPolicy_DefaultsOnly(t *testing.T) {
	d := &RetryConfig{MaxAttempts: 3, Backoff: "exponential", InitialDelay: Duration(50 * time.Millisecond)}
	p := resolveRetryPolicy("GET", d, nil, false)
	if p.maxAttempts != 3 || p.backoff != backoffExponential || p.initialDelay != 50*time.Millisecond {
		t.Errorf("got %+v", p)
	}
}

func TestResolveRetryPolicy_EndpointOverrides(t *testing.T) {
	d := &RetryConfig{MaxAttempts: 3, Backoff: "exponential"}
	e := &RetryConfig{MaxAttempts: 1, Backoff: "constant"}
	p := resolveRetryPolicy("GET", d, e, false)
	if p.maxAttempts != 1 || p.backoff != backoffConstant {
		t.Errorf("endpoint should override; got %+v", p)
	}
}

func TestResolveRetryPolicy_POSTForcedTo1(t *testing.T) {
	d := &RetryConfig{MaxAttempts: 5}
	p := resolveRetryPolicy("POST", d, nil, false)
	if p.maxAttempts != 1 {
		t.Errorf("POST should be forced to 1; got %d", p.maxAttempts)
	}
}

func TestResolveRetryPolicy_PATCHForcedTo1(t *testing.T) {
	d := &RetryConfig{MaxAttempts: 5}
	p := resolveRetryPolicy("PATCH", d, nil, false)
	if p.maxAttempts != 1 {
		t.Errorf("PATCH should be forced to 1; got %d", p.maxAttempts)
	}
}

func TestResolveRetryPolicy_POSTWithIdempotency_Allowed(t *testing.T) {
	d := &RetryConfig{MaxAttempts: 5}
	p := resolveRetryPolicy("POST", d, nil, true)
	if p.maxAttempts != 5 {
		t.Errorf("POST with idempotency should keep maxAttempts; got %d", p.maxAttempts)
	}
}

func TestResolveRetryPolicy_NoBlock_Disabled(t *testing.T) {
	p := resolveRetryPolicy("GET", nil, nil, false)
	if p.maxAttempts != 1 {
		t.Errorf("absent block should yield maxAttempts=1; got %d", p.maxAttempts)
	}
}

// --- E2E: retry middleware against httptest -------------------------------

func newRetryClient(t *testing.T, server *httptest.Server, ep EndpointConfig) *HttpClient {
	t.Helper()
	cfg := &Config{
		Services: map[string]ServiceConfig{
			"svc": {BaseURL: server.URL, Endpoints: map[string]EndpointConfig{"call": ep}},
		},
	}
	c, err := New(cfg, WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestRetry_503Retried_RecoversOnSecond(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n < 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, `{"hello":"recovered"}`)
	}))
	defer srv.Close()
	c := newRetryClient(t, srv, EndpointConfig{
		Method: "GET", Path: "/x",
		Retry: &RetryConfig{MaxAttempts: 3, Backoff: "constant", InitialDelay: Duration(1 * time.Millisecond), MaxDelay: Duration(10 * time.Millisecond)},
	})
	type req struct{}
	type resp struct {
		Hello string `json:"hello"`
	}
	got, err := Call[req, resp](configuration.NewAppContextWithRandomID(configuration.LangPTBR), c, "svc", "call", req{})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got.Hello != "recovered" {
		t.Errorf("got %+v", got)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Errorf("server saw %d calls, want 2", calls)
	}
}

func TestRetry_AlwaysFails_ExhaustsAttempts(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c := newRetryClient(t, srv, EndpointConfig{
		Method: "GET", Path: "/x",
		Retry: &RetryConfig{MaxAttempts: 3, Backoff: "constant", InitialDelay: Duration(1 * time.Millisecond)},
	})
	type req struct{}
	_, err := Call[req, struct{}](configuration.NewAppContextWithRandomID(configuration.LangPTBR), c, "svc", "call", req{})
	if err == nil {
		t.Fatal("expected error after exhausted attempts")
	}
	if !IsRetriable(err) {
		t.Errorf("final error should still be retriable: %v", err)
	}
	if atomic.LoadInt32(&calls) != 3 {
		t.Errorf("server saw %d calls, want 3", calls)
	}
}

func TestRetry_404NotRetried(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c := newRetryClient(t, srv, EndpointConfig{
		Method: "GET", Path: "/x",
		Retry: &RetryConfig{MaxAttempts: 3, Backoff: "constant", InitialDelay: Duration(1 * time.Millisecond)},
	})
	type req struct{}
	_, _ = Call[req, struct{}](configuration.NewAppContextWithRandomID(configuration.LangPTBR), c, "svc", "call", req{})
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("server saw %d calls, want 1 (404 not in retryOn)", calls)
	}
}

func TestRetry_BodyReplayedOnEachAttempt(t *testing.T) {
	var received []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		received = append(received, string(b))
		if len(received) < 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	// PUT is retriable; allows body replay assertion.
	c := newRetryClient(t, srv, EndpointConfig{
		Method: "PUT", Path: "/x",
		Retry: &RetryConfig{MaxAttempts: 3, Backoff: "constant", InitialDelay: Duration(1 * time.Millisecond)},
	})
	type body struct {
		N int `json:"n"`
	}
	type req struct {
		Body body `http:"body,json"`
	}
	_, _ = Call[req, struct{}](configuration.NewAppContextWithRandomID(configuration.LangPTBR), c, "svc", "call", req{Body: body{N: 7}})
	if len(received) != 2 {
		t.Fatalf("got %d server hits, want 2", len(received))
	}
	for i, b := range received {
		if b != `{"n":7}` {
			t.Errorf("attempt %d body = %q, want {\"n\":7}", i+1, b)
		}
	}
}

func TestRetry_CtxCanceledDuringSleep(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c := newRetryClient(t, srv, EndpointConfig{
		Method: "GET", Path: "/x",
		Retry: &RetryConfig{MaxAttempts: 5, Backoff: "constant", InitialDelay: Duration(50 * time.Millisecond)},
	})
	parent, cancel := context.WithCancel(context.Background())
	ctx := configuration.NewAppContextWithRandomID(configuration.LangPTBR)
	ctx.SetParent(parent)
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	type req struct{}
	_, err := Call[req, struct{}](ctx, c, "svc", "call", req{})
	if err == nil {
		t.Fatal("expected error after cancellation")
	}
	got := atomic.LoadInt32(&calls)
	if got >= 5 {
		t.Errorf("expected partial attempts (cancellation), got %d", got)
	}
}

func TestRetry_RetryAfter_Honored(t *testing.T) {
	var calls int32
	var firstTime time.Time
	var elapsed time.Duration
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			firstTime = time.Now()
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		elapsed = time.Since(firstTime)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	c := newRetryClient(t, srv, EndpointConfig{
		Method: "GET", Path: "/x",
		Retry: &RetryConfig{MaxAttempts: 2, Backoff: "constant", InitialDelay: Duration(1 * time.Millisecond), MaxDelay: Duration(2 * time.Second), RetryOn: []string{"503"}},
	})
	type req struct{}
	_, _ = Call[req, struct{}](configuration.NewAppContextWithRandomID(configuration.LangPTBR), c, "svc", "call", req{})
	if elapsed < 900*time.Millisecond {
		t.Errorf("Retry-After=1s not honored: elapsed=%v", elapsed)
	}
}

// --- HttpError.Attempt propagation (Bug 20 regression) -------------------

// Retry exhaustion must surface the actual attempt count on the returned
// *HttpError so consumers can log "exhausted N tries" without reaching into
// the observation. Pre-fix the value was hardcoded to 1 regardless of how
// many times the retry middleware ran.
func TestHttpError_Attempt_PropagatesFromRetry(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c := newRetryClient(t, srv, EndpointConfig{
		Method: "GET", Path: "/x",
		Retry: &RetryConfig{MaxAttempts: 3, Backoff: "constant", InitialDelay: Duration(1 * time.Millisecond)},
	})
	type req struct{}
	_, err := Call[req, struct{}](configuration.NewAppContextWithRandomID(configuration.LangPTBR), c, "svc", "call", req{})
	if err == nil {
		t.Fatal("expected error after exhausted retries")
	}
	var he *HttpError
	if !errors.As(err, &he) {
		t.Fatalf("expected *HttpError; got %T: %v", err, err)
	}
	if he.Attempt != 3 {
		t.Errorf("HttpError.Attempt = %d, want 3 (maxAttempts)", he.Attempt)
	}
	if atomic.LoadInt32(&calls) != 3 {
		t.Errorf("server saw %d calls, want 3", calls)
	}
}

// Single-attempt policy (no retry block) must still carry Attempt=1 — the
// retry middleware short-circuits without iterating but obs.Attempt is set
// to 1 before dispatch and the error path must honor it.
func TestHttpError_Attempt_NoRetryPolicy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := newRetryClient(t, srv, EndpointConfig{Method: "GET", Path: "/x"})
	type req struct{}
	_, err := Call[req, struct{}](configuration.NewAppContextWithRandomID(configuration.LangPTBR), c, "svc", "call", req{})
	if err == nil {
		t.Fatal("expected error")
	}
	var he *HttpError
	if !errors.As(err, &he) {
		t.Fatalf("expected *HttpError; got %T: %v", err, err)
	}
	if he.Attempt != 1 {
		t.Errorf("HttpError.Attempt = %d, want 1 (no retry policy)", he.Attempt)
	}
}

// keep used imports
var _ = errors.Is
