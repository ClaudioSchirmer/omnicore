package httpclient

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
)

// --- config validation ---------------------------------------------------

func TestValidateIdempotency_MissingHeader(t *testing.T) {
	errs := validateIdempotencyConfig("x", &IdempotencyConfig{})
	if len(errs) == 0 || !strings.Contains(errs[0], "header: required") {
		t.Errorf("expected header missing error; got %v", errs)
	}
}

func TestValidateIdempotency_BadSource(t *testing.T) {
	errs := validateIdempotencyConfig("x", &IdempotencyConfig{Header: "X-Key", Source: "random"})
	if len(errs) == 0 || !strings.Contains(errs[0], "ctx|explicit") {
		t.Errorf("expected source rejection; got %v", errs)
	}
}

func TestValidateIdempotency_OK(t *testing.T) {
	for _, src := range []string{"", "ctx", "CTX", "explicit", "Explicit"} {
		errs := validateIdempotencyConfig("x", &IdempotencyConfig{Header: "X-K", Source: src})
		if len(errs) != 0 {
			t.Errorf("source %q rejected unexpectedly: %v", src, errs)
		}
	}
}

func TestResolveIdempotencyPolicy_DefaultsToCtx(t *testing.T) {
	p := resolveIdempotencyPolicy(&IdempotencyConfig{Header: "X-K"})
	if !p.enabled || p.source != idempotencyCtx {
		t.Errorf("got %+v", p)
	}
}

func TestResolveIdempotencyPolicy_Explicit(t *testing.T) {
	p := resolveIdempotencyPolicy(&IdempotencyConfig{Header: "X-K", Source: "explicit"})
	if p.source != idempotencyExplicit {
		t.Errorf("got %+v", p)
	}
}

func TestResolveIdempotencyPolicy_Nil(t *testing.T) {
	p := resolveIdempotencyPolicy(nil)
	if p.enabled {
		t.Errorf("nil config should yield disabled policy")
	}
}

// --- POST/PATCH gate matrix ----------------------------------------------

func TestConfig_Validate_POSTRetryAllowedWithIdempotency(t *testing.T) {
	yml := `
services:
  s:
    baseURL: https://s.example.com
    endpoints:
      e:
        method: POST
        path: /charges
        retry: {maxAttempts: 3}
        idempotency: {header: X-Idempotency-Key, source: ctx}
`
	var c Config
	if err := yaml.Unmarshal([]byte(yml), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		t.Errorf("POST + retry + idempotency should be accepted; got %v", err)
	}
}

func TestConfig_Validate_POSTRetryStillRejectedWithoutIdempotency(t *testing.T) {
	yml := `
services:
  s:
    baseURL: https://s.example.com
    endpoints:
      e:
        method: POST
        path: /charges
        retry: {maxAttempts: 3}
`
	var c Config
	if err := yaml.Unmarshal([]byte(yml), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	c.applyDefaults()
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "requires idempotency") {
		t.Errorf("expected POST gate without idempotency; got %v", err)
	}
}

// --- E2E: idempotency middleware against httptest ------------------------

func newIdempotencyClient(t *testing.T, server *httptest.Server, ep EndpointConfig) *HttpClient {
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

func TestIdempotency_CtxSource_GeneratesUUIDv7(t *testing.T) {
	var received string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = r.Header.Get("X-Idempotency-Key")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	c := newIdempotencyClient(t, srv, EndpointConfig{
		Method: "POST", Path: "/x",
		Idempotency: &IdempotencyConfig{Header: "X-Idempotency-Key", Source: "ctx"},
	})
	type req struct{}
	_, _ = Call[req, struct{}](configuration.NewAppContextWithRandomID(configuration.LangPTBR), c, "svc", "call", req{})
	parsed, err := uuid.Parse(received)
	if err != nil {
		t.Errorf("server header %q is not a UUID: %v", received, err)
	}
	if parsed.Version() != 7 {
		t.Errorf("expected UUIDv7, got version %d", parsed.Version())
	}
}

func TestIdempotency_ExplicitSource_RequiresKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	c := newIdempotencyClient(t, srv, EndpointConfig{
		Method: "POST", Path: "/x",
		Idempotency: &IdempotencyConfig{Header: "X-Idempotency-Key", Source: "explicit"},
	})
	type req struct{}
	_, err := Call[req, struct{}](configuration.NewAppContextWithRandomID(configuration.LangPTBR), c, "svc", "call", req{})
	if err == nil || !strings.Contains(err.Error(), "requires CallConfig.IdempotencyKey") {
		t.Errorf("expected explicit-requires-key error; got %v", err)
	}
}

func TestIdempotency_ExplicitSource_WithKey(t *testing.T) {
	var received string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = r.Header.Get("X-Idempotency-Key")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	c := newIdempotencyClient(t, srv, EndpointConfig{
		Method: "POST", Path: "/x",
		Idempotency: &IdempotencyConfig{Header: "X-Idempotency-Key", Source: "explicit"},
	})
	type req struct{}
	_, err := Call[req, struct{}](configuration.NewAppContextWithRandomID(configuration.LangPTBR), c, "svc", "call", req{}, WithConfig(CallConfig{IdempotencyKey: "my-key"}))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if received != "my-key" {
		t.Errorf("server header = %q, want my-key", received)
	}
}

func TestIdempotency_SameKeyAcrossRetries(t *testing.T) {
	var keys []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keys = append(keys, r.Header.Get("X-Idempotency-Key"))
		if len(keys) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	c := newIdempotencyClient(t, srv, EndpointConfig{
		Method: "POST", Path: "/x",
		Idempotency: &IdempotencyConfig{Header: "X-Idempotency-Key", Source: "ctx"},
		Retry:       &RetryConfig{MaxAttempts: 3, Backoff: "constant", InitialDelay: Duration(1_000_000)}, // 1ms
	})
	type req struct{}
	_, _ = Call[req, struct{}](configuration.NewAppContextWithRandomID(configuration.LangPTBR), c, "svc", "call", req{})
	if len(keys) != 3 {
		t.Fatalf("expected 3 attempts; got %d", len(keys))
	}
	for i := 1; i < len(keys); i++ {
		if keys[i] != keys[0] {
			t.Errorf("attempt %d key %q differs from attempt 1 key %q", i+1, keys[i], keys[0])
		}
	}
}

func TestIdempotency_DifferentKeysAcrossCalls(t *testing.T) {
	var keys []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keys = append(keys, r.Header.Get("X-Idempotency-Key"))
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	c := newIdempotencyClient(t, srv, EndpointConfig{
		Method: "POST", Path: "/x",
		Idempotency: &IdempotencyConfig{Header: "X-Idempotency-Key", Source: "ctx"},
	})
	type req struct{}
	ctx := configuration.NewAppContextWithRandomID(configuration.LangPTBR)
	_, _ = Call[req, struct{}](ctx, c, "svc", "call", req{})
	_, _ = Call[req, struct{}](ctx, c, "svc", "call", req{})
	if len(keys) != 2 {
		t.Fatalf("expected 2 keys; got %d", len(keys))
	}
	if keys[0] == keys[1] {
		t.Errorf("two separate calls should have different keys; both = %q", keys[0])
	}
}

func TestIdempotency_AppContextSet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	c := newIdempotencyClient(t, srv, EndpointConfig{
		Method: "POST", Path: "/x",
		Idempotency: &IdempotencyConfig{Header: "X-Idempotency-Key", Source: "ctx"},
	})
	type req struct{}
	ctx := configuration.NewAppContextWithRandomID(configuration.LangPTBR)
	_, _ = Call[req, struct{}](ctx, c, "svc", "call", req{})
	v, ok := ctx.Get(AppContextIdempotencyKey)
	if !ok {
		t.Fatal("AppContext should carry the idempotency key after Call")
	}
	if s, _ := v.(string); s == "" {
		t.Errorf("AppContext key value is empty: %v", v)
	}
}

func TestIdempotency_DisabledByDefault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Idempotency-Key") != "" {
			t.Error("no idempotency block should mean no header")
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	c := newIdempotencyClient(t, srv, EndpointConfig{Method: "POST", Path: "/x"})
	type req struct{}
	_, _ = Call[req, struct{}](configuration.NewAppContextWithRandomID(configuration.LangPTBR), c, "svc", "call", req{})
}

// keep used import
var _ = atomic.LoadInt32(new(int32))
