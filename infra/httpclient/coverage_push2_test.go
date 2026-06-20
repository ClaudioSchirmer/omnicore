package httpclient

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/infra/cache"
)

// --- shouldRetry: DNS + network transport branches --------------------------

func TestShouldRetry_DNSErrorWhenEnabled(t *testing.T) {
	policy := retryPolicy{retryOnDNS: true}
	dnsErr := &net.DNSError{Err: "no such host", Name: "x.invalid"}
	if !shouldRetry(nil, dnsErr, policy) {
		t.Fatal("DNS error must retry when retryOnDNS is set")
	}
	// Disabled → no retry.
	if shouldRetry(nil, dnsErr, retryPolicy{}) {
		t.Fatal("DNS error must NOT retry when retryOnDNS is unset")
	}
}

func TestShouldRetry_NetworkErrorWhenEnabled(t *testing.T) {
	policy := retryPolicy{retryOnNetwork: true}
	opErr := &net.OpError{Op: "dial", Err: errors.New("connection refused")}
	if !shouldRetry(nil, opErr, policy) {
		t.Fatal("network OpError must retry when retryOnNetwork is set")
	}
}

// --- computeBackoff: jitter ceiling clamp + overflow safeguard --------------

func TestComputeBackoff_JitterClampsToMaxDelay(t *testing.T) {
	policy := retryPolicy{
		backoff:      backoffExponentialJitter,
		initialDelay: time.Second,
		maxDelay:     2 * time.Second,
	}
	// attempt 10 → ceiling = 1s << 9 = 512s, clamped to maxDelay (2s); the
	// jittered result must land within [0, maxDelay].
	for i := 0; i < 20; i++ {
		d := computeBackoff(policy, 10)
		if d < 0 || d > policy.maxDelay {
			t.Fatalf("jittered backoff %v outside [0,%v]", d, policy.maxDelay)
		}
	}
}

func TestComputeBackoff_ExponentialOverflowFallsBackToMax(t *testing.T) {
	policy := retryPolicy{
		backoff:      backoffExponential,
		initialDelay: time.Duration(1), // 1ns; << 63 sets the sign bit
		maxDelay:     time.Minute,
	}
	// A huge attempt overflows the left shift to a negative duration; the
	// guard must coerce it back to maxDelay rather than sleeping forever.
	d := computeBackoff(policy, 64)
	if d != policy.maxDelay {
		t.Fatalf("overflowed backoff = %v, want maxDelay %v", d, policy.maxDelay)
	}
}

// --- canonicalQuery nil URL --------------------------------------------------

func TestCanonicalQuery_NilURL(t *testing.T) {
	if got := canonicalQuery(nil); got != "" {
		t.Fatalf("canonicalQuery(nil) = %q, want empty", got)
	}
}

// --- bodyForSigning: defensive read-error path ------------------------------

type errReadCloser struct{}

func (errReadCloser) Read([]byte) (int, error) { return 0, errors.New("read boom") }
func (errReadCloser) Close() error             { return nil }

func TestBodyForSigning_ReadErrorReturnsNil(t *testing.T) {
	req, _ := http.NewRequest("POST", "https://x.example.com/p", nil)
	req.Body = errReadCloser{}
	// obs nil + non-empty erroring body → defensive ReadAll fails → nil.
	if got := bodyForSigning(req, nil); got != nil {
		t.Fatalf("bodyForSigning on read error = %v, want nil", got)
	}
}

func TestBodyForSigning_NilBodyReturnsNil(t *testing.T) {
	req, _ := http.NewRequest("GET", "https://x.example.com/p", nil)
	req.Body = nil
	if got := bodyForSigning(req, nil); got != nil {
		t.Fatalf("bodyForSigning with nil body = %v, want nil", got)
	}
}

// --- cache middleware error branches via a fake cache.Cache -----------------

// scriptedCache is a hand-rolled cache.Cache that returns scripted Get/Set
// outcomes so the middleware's error/corrupt-entry branches run without a
// real backend.
type scriptedCache struct {
	getErr   error
	getValue []byte
	getOK    bool
	setErr   error
}

func (s *scriptedCache) Get(_ context.Context, _ string) ([]byte, bool, error) {
	if s.getErr != nil {
		return nil, false, s.getErr
	}
	if s.getOK {
		return s.getValue, true, nil
	}
	return nil, false, nil
}

func (s *scriptedCache) Set(_ context.Context, _ string, _ []byte, _ time.Duration) error {
	return s.setErr
}

func (s *scriptedCache) Delete(_ context.Context, _ string) error { return nil }

var _ cache.Cache = (*scriptedCache)(nil)

func newScriptedCacheClient(t *testing.T, srv *httptest.Server, store cache.Cache) *HttpClient {
	t.Helper()
	cfg := &Config{
		Services: map[string]ServiceConfig{
			"svc": {BaseURL: srv.URL, Endpoints: map[string]EndpointConfig{
				"call": {Method: "GET", Path: "/x", Cache: &EndpointCacheConfig{TTL: Duration(time.Minute)}},
			}},
		},
	}
	c, err := New(cfg,
		WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		WithCache(store))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestCacheMiddleware_GetErrorSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	c := newScriptedCacheClient(t, srv, &scriptedCache{getErr: errors.New("cache get boom")})
	type req struct{}
	_, err := Call[req, struct{}](configuration.NewAppContextWithRandomID(configuration.LangENG), c, "svc", "call", req{})
	if err == nil {
		t.Fatal("cache Get error must surface as a Call error")
	}
}

func TestCacheMiddleware_CorruptEntryTreatedAsMiss(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_, _ = io.WriteString(w, `{"v":"ok"}`)
	}))
	defer srv.Close()
	// getOK=true with non-JSON bytes → decode fails → treated as a miss →
	// the upstream is dialed.
	store := &scriptedCache{getOK: true, getValue: []byte("not valid json")}
	c := newScriptedCacheClient(t, srv, store)
	type req struct{}
	type resp struct {
		V string `json:"v"`
	}
	out, err := Call[req, resp](configuration.NewAppContextWithRandomID(configuration.LangENG), c, "svc", "call", req{})
	if err != nil {
		t.Fatalf("corrupt entry should fall through to upstream, got %v", err)
	}
	if out.V != "ok" || calls != 1 {
		t.Fatalf("expected upstream fetch on corrupt entry, calls=%d out=%+v", calls, out)
	}
}

func TestCacheMiddleware_SetErrorSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	// Miss on Get, 200 storable response, but Set fails → Call surfaces it.
	c := newScriptedCacheClient(t, srv, &scriptedCache{setErr: errors.New("cache set boom")})
	type req struct{}
	_, err := Call[req, struct{}](configuration.NewAppContextWithRandomID(configuration.LangENG), c, "svc", "call", req{})
	if err == nil {
		t.Fatal("cache Set error must surface as a Call error")
	}
}

// --- Call: streaming/SSE non-2xx drain + decode error -----------------------

func TestCall_ResponseStream_Non2xxDrainsToHttpError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `upstream exploded`)
	}))
	defer srv.Close()
	c := streamingClient(t, srv, EndpointConfig{Method: "GET", Path: "/x", ResponseStream: true})
	type req struct{}
	_, err := Call[req, StreamResponse](newCtx(t), c, "svc", "call", req{})
	var herr *HttpError
	if !errors.As(err, &herr) || herr.Status != http.StatusBadGateway {
		t.Fatalf("expected 502 HttpError on streaming non-2xx, got %v", err)
	}
	if string(herr.Body) != "upstream exploded" {
		t.Fatalf("drained body = %q, want upstream exploded", string(herr.Body))
	}
}

func TestCall_ResponseSSE_Non2xxDrainsToHttpError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `no stream for you`)
	}))
	defer srv.Close()
	c := streamingClient(t, srv, EndpointConfig{Method: "GET", Path: "/events", ResponseSSE: true})
	type req struct{}
	_, err := Call[req, SSEResponse](newCtx(t), c, "svc", "call", req{})
	var herr *HttpError
	if !errors.As(err, &herr) || herr.Status != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 HttpError on SSE non-2xx, got %v", err)
	}
}

func TestCall_DecodeResponseError_On2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{this is not valid json`)
	}))
	defer srv.Close()
	c := streamingClient(t, srv, EndpointConfig{Method: "GET", Path: "/x"})
	type req struct{}
	type resp struct {
		Name string `json:"name"`
	}
	_, err := Call[req, resp](newCtx(t), c, "svc", "call", req{})
	if !errors.Is(err, ErrResponseDecode) {
		t.Fatalf("expected ErrResponseDecode on malformed 2xx body, got %v", err)
	}
}

func TestCanonicalQuery_SortsRepeatedKeys(t *testing.T) {
	u, _ := url.Parse("https://x.example.com/p?b=2&a=3&a=1")
	got := canonicalQuery(u)
	// keys sorted; repeated key values sorted ascending → a=1&a=3&b=2
	if got != "a=1&a=3&b=2" {
		t.Fatalf("canonicalQuery = %q, want a=1&a=3&b=2", got)
	}
}
