package httpclient

import (
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// withConfigClient is a minimal client builder shared by the per-dimension
// tests. Defaults: GET / json/json against the supplied test server.
func withConfigClient(t *testing.T, srv *httptest.Server, ep EndpointConfig) *HttpClient {
	t.Helper()
	cfg := &Config{
		Services: map[string]ServiceConfig{
			"svc": {
				BaseURL:   srv.URL,
				Endpoints: map[string]EndpointConfig{"call": ep},
			},
		},
	}
	c, err := New(cfg, WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestWithConfig_Method_Overrides(t *testing.T) {
	var saw string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		saw = r.Method
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	// YAML declares GET; override forces PUT.
	c := withConfigClient(t, srv, EndpointConfig{Method: "GET", Path: "/x"})
	type req struct{}
	_, err := Call[req, struct{}](newCtx(t), c, "svc", "call", req{},
		WithConfig(CallConfig{Method: "PUT"}))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if saw != "PUT" {
		t.Fatalf("method override missed; server saw %q", saw)
	}
}

func TestWithConfig_Path_Overrides(t *testing.T) {
	var saw string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		saw = r.URL.Path
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	c := withConfigClient(t, srv, EndpointConfig{Method: "GET", Path: "/yaml"})
	type req struct{}
	_, err := Call[req, struct{}](newCtx(t), c, "svc", "call", req{},
		WithConfig(CallConfig{Path: "/runtime/path"}))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if saw != "/runtime/path" {
		t.Fatalf("path override missed; server saw %q", saw)
	}
}

func TestWithConfig_WebhookShape_BaseURL_Path_Method(t *testing.T) {
	type seen struct {
		method, path string
	}
	var got seen
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = seen{r.Method, r.URL.Path}
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	// YAML declares a placeholder endpoint; everything comes from the payload.
	c := withConfigClient(t, srv, EndpointConfig{Method: "GET", Path: "/placeholder"})
	type req struct{}
	_, err := Call[req, struct{}](newCtx(t), c, "svc", "call", req{},
		WithConfig(CallConfig{
			BaseURL: srv.URL,
			Method:  "PATCH",
			Path:    "/api/v3/orders/12345/notify",
		}))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got.method != "PATCH" || got.path != "/api/v3/orders/12345/notify" {
		t.Fatalf("webhook override missed; server saw %+v", got)
	}
}

func TestWithConfig_InlineAuth_Bearer(t *testing.T) {
	var sawAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	c := withConfigClient(t, srv, EndpointConfig{Method: "GET", Path: "/x"})
	type req struct{}
	_, err := Call[req, struct{}](newCtx(t), c, "svc", "call", req{},
		WithConfig(CallConfig{InlineAuth: &InlineAuth{Bearer: "abc123"}}))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if sawAuth != "Bearer abc123" {
		t.Fatalf("inline bearer header = %q", sawAuth)
	}
}

func TestWithConfig_InlineAuth_APIKey_DefaultHeader(t *testing.T) {
	var saw string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		saw = r.Header.Get("X-API-Key")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	c := withConfigClient(t, srv, EndpointConfig{Method: "GET", Path: "/x"})
	type req struct{}
	_, err := Call[req, struct{}](newCtx(t), c, "svc", "call", req{},
		WithConfig(CallConfig{InlineAuth: &InlineAuth{APIKey: &APIKeyAuth{Value: "secret-key"}}}))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if saw != "secret-key" {
		t.Fatalf("inline apikey header X-API-Key = %q", saw)
	}
}

func TestWithConfig_InlineAuth_APIKey_CustomHeader(t *testing.T) {
	var saw string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		saw = r.Header.Get("X-Tenant-Key")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	c := withConfigClient(t, srv, EndpointConfig{Method: "GET", Path: "/x"})
	type req struct{}
	_, err := Call[req, struct{}](newCtx(t), c, "svc", "call", req{},
		WithConfig(CallConfig{InlineAuth: &InlineAuth{APIKey: &APIKeyAuth{Header: "X-Tenant-Key", Value: "tenant-secret"}}}))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if saw != "tenant-secret" {
		t.Fatalf("inline apikey custom header X-Tenant-Key = %q", saw)
	}
}

func TestWithConfig_InlineAuth_Basic(t *testing.T) {
	var sawAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	c := withConfigClient(t, srv, EndpointConfig{Method: "GET", Path: "/x"})
	type req struct{}
	_, err := Call[req, struct{}](newCtx(t), c, "svc", "call", req{},
		WithConfig(CallConfig{InlineAuth: &InlineAuth{Basic: &BasicAuth{Username: "alice", Password: "wonderland"}}}))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	wantPrefix := "Basic "
	if !strings.HasPrefix(sawAuth, wantPrefix) {
		t.Fatalf("inline basic missing prefix; header = %q", sawAuth)
	}
	encoded := strings.TrimPrefix(sawAuth, wantPrefix)
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}
	if string(decoded) != "alice:wonderland" {
		t.Fatalf("basic creds decode = %q, want alice:wonderland", decoded)
	}
}

func TestWithConfig_InlineAuth_RejectsAmbiguousShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("server must not be reached when inline auth shape is ambiguous")
	}))
	defer srv.Close()
	c := withConfigClient(t, srv, EndpointConfig{Method: "GET", Path: "/x"})
	type req struct{}
	_, err := Call[req, struct{}](newCtx(t), c, "svc", "call", req{},
		WithConfig(CallConfig{InlineAuth: &InlineAuth{
			Bearer: "tok",
			APIKey: &APIKeyAuth{Value: "k"},
		}}))
	if !errors.Is(err, ErrTokenAcquire) {
		t.Fatalf("expected ErrTokenAcquire for ambiguous inline auth, got %v", err)
	}
}

func TestWithConfig_InlineAuth_RejectsEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("server must not be reached when inline auth has no scheme")
	}))
	defer srv.Close()
	c := withConfigClient(t, srv, EndpointConfig{Method: "GET", Path: "/x"})
	type req struct{}
	_, err := Call[req, struct{}](newCtx(t), c, "svc", "call", req{},
		WithConfig(CallConfig{InlineAuth: &InlineAuth{}}))
	if !errors.Is(err, ErrTokenAcquire) {
		t.Fatalf("expected ErrTokenAcquire for empty inline auth, got %v", err)
	}
}

func TestWithConfig_RetryOverride_AppliedPerCall(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if n < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	// YAML endpoint declares no retry → single attempt by default.
	c := withConfigClient(t, srv, EndpointConfig{Method: "GET", Path: "/x"})
	type req struct{}
	_, err := Call[req, struct{}](newCtx(t), c, "svc", "call", req{},
		WithConfig(CallConfig{Retry: &RetryOverride{
			MaxAttempts:  3,
			Backoff:      "constant",
			InitialDelay: 1 * time.Millisecond,
			MaxDelay:     5 * time.Millisecond,
			RetryOn:      []string{"502"},
		}}))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Fatalf("expected 3 attempts via per-call retry, got %d", got)
	}
}

func TestWithConfig_PathOverride_FromTemplatedYAML(t *testing.T) {
	// Guards against a regression where the binding cache stored a plan
	// whose path-coverage check was validated against the YAML path
	// (containing {placeholders}), while BuildRequest later dispatched
	// against the override path. The cache hit then surfaced a stale
	// "placeholder X has no matching http:\"path,name\" field" error
	// even though the override path had no placeholders at all.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/runtime/upload" || r.Method != "POST" {
			t.Errorf("server saw method=%q path=%q", r.Method, r.URL.Path)
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	// YAML declares the endpoint with a path placeholder; the request DTO
	// has no path tag, so the YAML path on its own would fail validation.
	c := withConfigClient(t, srv, EndpointConfig{Method: "GET", Path: "/yaml/stream/{size}"})

	type req struct {
		Body any `http:"body,json"`
	}
	_, err := Call[req, struct{}](newCtx(t), c, "svc", "call", req{Body: map[string]string{"k": "v"}},
		WithConfig(CallConfig{Method: "POST", Path: "/runtime/upload"}))
	if err != nil {
		t.Fatalf("Call should succeed when override path has no placeholders, got %v", err)
	}
}

func TestWithConfig_AcceptableStatusUnionsYAML(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusGone) // 410
	}))
	defer srv.Close()
	c := withConfigClient(t, srv, EndpointConfig{
		Method: "GET", Path: "/x",
		AcceptableStatus: []int{404}, // YAML accepts 404 only
	})
	type req struct{}
	// 410 is not in YAML; per-call accepts it as a union.
	_, err := Call[req, struct{}](newCtx(t), c, "svc", "call", req{},
		WithConfig(CallConfig{AcceptableStatus: []int{410}}))
	if !IsAcceptableStatus(err, 410) {
		t.Fatalf("expected 410 to be acceptable via per-call union, got %v", err)
	}
}
