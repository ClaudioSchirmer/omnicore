package httpclient

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// resolverEchoServer returns a server that responds with the request path so
// tests can assert the resolved baseURL was used.
func resolverEchoServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"hello":"`+r.URL.Path+`"}`)
	}))
}

func resolverNewClient(t *testing.T, yamlBaseURL string, opts ...Option) *HttpClient {
	t.Helper()
	cfg := &Config{
		Services: map[string]ServiceConfig{
			"svc": {
				BaseURL: yamlBaseURL,
				Endpoints: map[string]EndpointConfig{
					"call": {Method: "GET", Path: "/echo"},
				},
			},
		},
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	all := append([]Option{WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))}, opts...)
	c, err := New(cfg, all...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

type resolverEcho struct{}
type resolverEchoResp struct {
	Hello string `json:"hello"`
}

func TestResolver_NoResolver_UsesYAMLBaseURL(t *testing.T) {
	srv := resolverEchoServer(t)
	defer srv.Close()
	c := resolverNewClient(t, srv.URL)

	got, err := Call[resolverEcho, resolverEchoResp](newCtx(t), c, "svc", "call", resolverEcho{})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got.Hello != "/echo" {
		t.Fatalf("hello=%q want /echo", got.Hello)
	}
}

type fixedResolver string

func (f fixedResolver) Resolve(_ context.Context, _ string) (string, error) {
	return string(f), nil
}

func TestResolver_ReturnsURL_OverridesYAML(t *testing.T) {
	srv := resolverEchoServer(t)
	defer srv.Close()
	// YAML points elsewhere; resolver redirects to the test server.
	c := resolverNewClient(t, "http://invalid.example.com", WithResolver(fixedResolver(srv.URL)))

	got, err := Call[resolverEcho, resolverEchoResp](newCtx(t), c, "svc", "call", resolverEcho{})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got.Hello != "/echo" {
		t.Fatalf("expected resolver baseURL to be used, got %q", got.Hello)
	}
}

type emptyResolver struct{}

func (emptyResolver) Resolve(_ context.Context, _ string) (string, error) {
	return "", nil
}

func TestResolver_ReturnsEmpty_FallsBackToYAML(t *testing.T) {
	srv := resolverEchoServer(t)
	defer srv.Close()
	c := resolverNewClient(t, srv.URL, WithResolver(emptyResolver{}))

	got, err := Call[resolverEcho, resolverEchoResp](newCtx(t), c, "svc", "call", resolverEcho{})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got.Hello != "/echo" {
		t.Fatalf("expected YAML fallback, got %q", got.Hello)
	}
}

type errorResolver struct{ err error }

func (e errorResolver) Resolve(_ context.Context, _ string) (string, error) {
	return "", e.err
}

func TestResolver_ReturnsError_SurfacedAsHttpError(t *testing.T) {
	sentinel := errors.New("upstream discovery down")
	c := resolverNewClient(t, "http://placeholder.example.com", WithResolver(errorResolver{err: sentinel}))

	_, err := Call[resolverEcho, resolverEchoResp](newCtx(t), c, "svc", "call", resolverEcho{})
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel to wrap through HttpError, got %v", err)
	}
	var he *HttpError
	if !errors.As(err, &he) {
		t.Fatalf("expected *HttpError, got %T", err)
	}
	if he.Service != "svc" || he.Endpoint != "call" {
		t.Fatalf("HttpError missing service/endpoint: %+v", he)
	}
}

func TestResolver_BothEmpty_Errors(t *testing.T) {
	c := resolverNewClient(t, "" /* empty YAML */, WithResolver(emptyResolver{}))

	_, err := Call[resolverEcho, resolverEchoResp](newCtx(t), c, "svc", "call", resolverEcho{})
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "no baseURL") {
		t.Fatalf("expected 'no baseURL' error, got %v", err)
	}
}

func TestResolver_StaticHit(t *testing.T) {
	srv := resolverEchoServer(t)
	defer srv.Close()
	c := resolverNewClient(t, "http://stale.example.com",
		WithResolver(StaticBaseURLResolver{"svc": srv.URL}))

	got, err := Call[resolverEcho, resolverEchoResp](newCtx(t), c, "svc", "call", resolverEcho{})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got.Hello != "/echo" {
		t.Fatalf("expected static resolver URL to win, got %q", got.Hello)
	}
}

func TestResolver_StaticMiss_FallsBackToYAML(t *testing.T) {
	srv := resolverEchoServer(t)
	defer srv.Close()
	// Resolver knows a different service; for "svc" the static returns "".
	c := resolverNewClient(t, srv.URL,
		WithResolver(StaticBaseURLResolver{"other": "http://wherever"}))

	got, err := Call[resolverEcho, resolverEchoResp](newCtx(t), c, "svc", "call", resolverEcho{})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got.Hello != "/echo" {
		t.Fatalf("expected YAML fallback on static miss, got %q", got.Hello)
	}
}

func TestWithBaseURL_OverridesResolver(t *testing.T) {
	srv := resolverEchoServer(t)
	defer srv.Close()
	// YAML and resolver both point to dead URLs; per-call WithBaseURL must win.
	c := resolverNewClient(t, "http://yaml.invalid",
		WithResolver(fixedResolver("http://resolver.invalid")))

	got, err := Call[resolverEcho, resolverEchoResp](newCtx(t), c,
		"svc", "call", resolverEcho{}, WithConfig(CallConfig{BaseURL: srv.URL}))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got.Hello != "/echo" {
		t.Fatalf("expected per-call override to win, got %q", got.Hello)
	}
}

func TestWithBaseURL_OverridesYAMLWithoutResolver(t *testing.T) {
	srv := resolverEchoServer(t)
	defer srv.Close()
	// YAML points to a dead URL; no resolver; per-call WithBaseURL rescues.
	c := resolverNewClient(t, "http://yaml.invalid")

	got, err := Call[resolverEcho, resolverEchoResp](newCtx(t), c,
		"svc", "call", resolverEcho{}, WithConfig(CallConfig{BaseURL: srv.URL}))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got.Hello != "/echo" {
		t.Fatalf("expected WithBaseURL to win over YAML, got %q", got.Hello)
	}
}

func TestWithBaseURL_RescuesEmptyYAML(t *testing.T) {
	srv := resolverEchoServer(t)
	defer srv.Close()
	// YAML empty + no resolver would normally error; per-call override unblocks it.
	c := resolverNewClient(t, "")

	got, err := Call[resolverEcho, resolverEchoResp](newCtx(t), c,
		"svc", "call", resolverEcho{}, WithConfig(CallConfig{BaseURL: srv.URL}))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got.Hello != "/echo" {
		t.Fatalf("expected per-call override to rescue empty YAML, got %q", got.Hello)
	}
}
