package httpclient

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
)

// --- state machine unit tests --------------------------------------------

func makePolicy() breakerPolicy {
	return breakerPolicy{enabled: true, failureThreshold: 3, successThreshold: 2, openFor: 50 * time.Millisecond}
}

func TestBreaker_ClosedAdmits(t *testing.T) {
	b := newBreakerState(makePolicy())
	ok, label := b.allow()
	if !ok || label != "closed" {
		t.Errorf("closed allow = (%t, %q), want (true, closed)", ok, label)
	}
}

func TestBreaker_TripsAfterFailureThreshold(t *testing.T) {
	b := newBreakerState(makePolicy())
	for i := 0; i < 3; i++ {
		ok, _ := b.allow()
		if !ok {
			t.Fatalf("attempt %d should be admitted while in closed", i+1)
		}
		b.recordFailure()
	}
	ok, label := b.allow()
	if ok || label != "open" {
		t.Errorf("after %d failures want open; got (%t, %q)", 3, ok, label)
	}
}

func TestBreaker_SuccessResetsFailureCount(t *testing.T) {
	b := newBreakerState(makePolicy())
	// 2 failures, then a success, then 2 more failures — should still be
	// closed because the success reset the count.
	for i := 0; i < 2; i++ {
		_, _ = b.allow()
		b.recordFailure()
	}
	_, _ = b.allow()
	b.recordSuccess()
	for i := 0; i < 2; i++ {
		_, _ = b.allow()
		b.recordFailure()
	}
	ok, label := b.allow()
	if !ok || label != "closed" {
		t.Errorf("success should reset; got (%t, %q)", ok, label)
	}
}

func TestBreaker_OpenToHalfOpenAfterOpenFor(t *testing.T) {
	b := newBreakerState(makePolicy())
	for i := 0; i < 3; i++ {
		_, _ = b.allow()
		b.recordFailure()
	}
	if ok, _ := b.allow(); ok {
		t.Fatal("breaker should be open immediately after trip")
	}
	time.Sleep(60 * time.Millisecond)
	ok, label := b.allow()
	if !ok || label != "half-open" {
		t.Errorf("after openFor want admit half-open; got (%t, %q)", ok, label)
	}
}

func TestBreaker_HalfOpen_SuccessThresholdCloses(t *testing.T) {
	b := newBreakerState(makePolicy())
	for i := 0; i < 3; i++ {
		_, _ = b.allow()
		b.recordFailure()
	}
	time.Sleep(60 * time.Millisecond)
	// First half-open probe
	_, _ = b.allow()
	b.recordSuccess()
	// Second half-open probe — should still be admitted as half-open
	ok, label := b.allow()
	if !ok || label != "half-open" {
		t.Errorf("between probes want half-open; got (%t, %q)", ok, label)
	}
	b.recordSuccess()
	// After successThreshold=2 successes, should close
	ok, label = b.allow()
	if !ok || label != "closed" {
		t.Errorf("after successThreshold want closed; got (%t, %q)", ok, label)
	}
}

func TestBreaker_HalfOpen_FailureReopens(t *testing.T) {
	b := newBreakerState(makePolicy())
	for i := 0; i < 3; i++ {
		_, _ = b.allow()
		b.recordFailure()
	}
	time.Sleep(60 * time.Millisecond)
	_, _ = b.allow()
	b.recordFailure()
	ok, label := b.allow()
	if ok || label != "open" {
		t.Errorf("half-open failure should reopen; got (%t, %q)", ok, label)
	}
}

func TestBreaker_HalfOpen_SerializesProbes(t *testing.T) {
	b := newBreakerState(makePolicy())
	for i := 0; i < 3; i++ {
		_, _ = b.allow()
		b.recordFailure()
	}
	time.Sleep(60 * time.Millisecond)
	ok1, _ := b.allow()
	ok2, label2 := b.allow()
	if !ok1 {
		t.Fatal("first half-open probe should be admitted")
	}
	if ok2 || label2 != "open" {
		t.Errorf("second concurrent half-open probe should be rejected; got (%t, %q)", ok2, label2)
	}
}

func TestBreaker_Disabled_AlwaysAdmits(t *testing.T) {
	b := newBreakerState(breakerPolicy{enabled: false})
	for i := 0; i < 100; i++ {
		b.recordFailure()
	}
	ok, _ := b.allow()
	if !ok {
		t.Error("disabled breaker should always admit")
	}
}

// --- config validation ----------------------------------------------------

func TestValidateBreakerConfig_BadFields(t *testing.T) {
	cfg := &CircuitBreakerConfig{FailureThreshold: -1, SuccessThreshold: -1, OpenFor: Duration(-time.Second)}
	errs := validateBreakerConfig("x", cfg)
	if len(errs) < 3 {
		t.Errorf("expected 3 errors; got %v", errs)
	}
}

func TestResolveBreakerConfig_FrameworkDefaults(t *testing.T) {
	cfg := &CircuitBreakerConfig{}
	p := resolveBreakerConfig(cfg)
	if !p.enabled || p.failureThreshold != 5 || p.successThreshold != 2 || p.openFor != 30*time.Second {
		t.Errorf("framework defaults not applied: %+v", p)
	}
}

func TestResolveBreakerConfig_ExplicitDisable(t *testing.T) {
	f := false
	cfg := &CircuitBreakerConfig{Enabled: &f}
	p := resolveBreakerConfig(cfg)
	if p.enabled {
		t.Error("explicit Enabled=false should disable policy")
	}
}

// --- E2E: breaker against httptest ---------------------------------------

func newBreakerClient(t *testing.T, server *httptest.Server, breakerCfg *CircuitBreakerConfig) *HttpClient {
	t.Helper()
	cfg := &Config{
		Defaults: Defaults{CircuitBreaker: breakerCfg},
		Services: map[string]ServiceConfig{
			"svc": {BaseURL: server.URL, Endpoints: map[string]EndpointConfig{
				"call": {Method: "GET", Path: "/x"},
			}},
		},
	}
	c, err := New(cfg, WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestBreaker_E2E_OpensAfterRepeatedFailures(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c := newBreakerClient(t, srv, &CircuitBreakerConfig{FailureThreshold: 3, SuccessThreshold: 1, OpenFor: Duration(time.Hour)})
	type req struct{}
	ctx := configuration.NewAppContextWithRandomID(configuration.LangPTBR)
	for i := 0; i < 3; i++ {
		_, _ = Call[req, struct{}](ctx, c, "svc", "call", req{})
	}
	if atomic.LoadInt32(&calls) != 3 {
		t.Fatalf("server should see 3 calls before trip; got %d", calls)
	}
	// 4th call should be rejected by breaker without dialing
	atomic.StoreInt32(&calls, 0)
	_, err := Call[req, struct{}](ctx, c, "svc", "call", req{})
	if !errors.Is(err, ErrCircuitOpen) {
		t.Errorf("expected ErrCircuitOpen; got %v", err)
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Errorf("server should not receive call when breaker open; got %d", calls)
	}
}

func TestBreaker_E2E_RecoversAfterOpenFor(t *testing.T) {
	var calls int32
	var fail int32 = 1
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if atomic.LoadInt32(&fail) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	c := newBreakerClient(t, srv, &CircuitBreakerConfig{FailureThreshold: 2, SuccessThreshold: 1, OpenFor: Duration(40 * time.Millisecond)})
	type req struct{}
	ctx := configuration.NewAppContextWithRandomID(configuration.LangPTBR)
	for i := 0; i < 2; i++ {
		_, _ = Call[req, struct{}](ctx, c, "svc", "call", req{})
	}
	// Breaker open
	_, err := Call[req, struct{}](ctx, c, "svc", "call", req{})
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("expected open; got %v", err)
	}
	// Flip server to success then wait for openFor
	atomic.StoreInt32(&fail, 0)
	time.Sleep(60 * time.Millisecond)
	_, err = Call[req, struct{}](ctx, c, "svc", "call", req{})
	if err != nil {
		t.Errorf("half-open probe should succeed; got %v", err)
	}
	// Should now be closed; new calls should pass through
	_, err = Call[req, struct{}](ctx, c, "svc", "call", req{})
	if err != nil {
		t.Errorf("after close, calls should pass; got %v", err)
	}
}

func TestBreaker_E2E_RetryAttemptsCountAsFailures(t *testing.T) {
	// Per Q7: each retry attempt is a breaker observation. With
	// failureThreshold=3 and a GET endpoint configured to retry 3 times,
	// one call exhausts the threshold and the breaker opens.
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	cfg := &Config{
		Defaults: Defaults{CircuitBreaker: &CircuitBreakerConfig{FailureThreshold: 3, SuccessThreshold: 1, OpenFor: Duration(time.Hour)}},
		Services: map[string]ServiceConfig{
			"svc": {BaseURL: srv.URL, Endpoints: map[string]EndpointConfig{
				"call": {
					Method: "GET", Path: "/x",
					Retry: &RetryConfig{MaxAttempts: 3, Backoff: "constant", InitialDelay: Duration(1 * time.Millisecond)},
				},
			}},
		},
	}
	c, _ := New(cfg, WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	type req struct{}
	ctx := configuration.NewAppContextWithRandomID(configuration.LangPTBR)
	// First call: 3 retry attempts, each a failure → breaker tripped
	_, _ = Call[req, struct{}](ctx, c, "svc", "call", req{})
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("first call should see 3 attempts (retry exhaustion); got %d", got)
	}
	// Second call: breaker is open, should reject immediately
	atomic.StoreInt32(&calls, 0)
	_, err := Call[req, struct{}](ctx, c, "svc", "call", req{})
	if !errors.Is(err, ErrCircuitOpen) {
		t.Errorf("breaker should be open; got %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Errorf("breaker open should prevent dialing; got %d calls", got)
	}
}
