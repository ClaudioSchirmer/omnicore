package httpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
)

// --- helpers --------------------------------------------------------------

func newCtx(t *testing.T) *configuration.AppContext {
	t.Helper()
	return configuration.NewAppContextWithRandomID(configuration.LangPTBR)
}

func newClient(t *testing.T, server *httptest.Server, ep EndpointConfig, defaults Defaults) *HttpClient {
	t.Helper()
	cfg := &Config{
		Defaults: defaults,
		Services: map[string]ServiceConfig{
			"svc": {
				BaseURL:   server.URL,
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

type echoBody struct {
	Hello string `json:"hello"`
}

// --- Call: happy paths ----------------------------------------------------

func TestCall_Get_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/echo/42" {
			t.Errorf("server saw method=%q path=%q", r.Method, r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"hello":"world"}`)
	}))
	defer srv.Close()
	c := newClient(t, srv, EndpointConfig{Method: "GET", Path: "/echo/{id}"}, Defaults{})

	type req struct {
		ID string `http:"path,id"`
	}
	got, err := Call[req, echoBody](newCtx(t), c, "svc", "call", req{ID: "42"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got.Hello != "world" {
		t.Errorf("got %+v", got)
	}
}

func TestCall_Post_BodyRoundTrip(t *testing.T) {
	var received echoBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&received)
		_, _ = io.WriteString(w, `{"hello":"echoed"}`)
	}))
	defer srv.Close()
	c := newClient(t, srv, EndpointConfig{Method: "POST", Path: "/echo"}, Defaults{})

	type req struct {
		Body echoBody `http:"body,json"`
	}
	got, err := Call[req, echoBody](newCtx(t), c, "svc", "call", req{Body: echoBody{Hello: "ada"}})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if received.Hello != "ada" {
		t.Errorf("server received body %+v", received)
	}
	if got.Hello != "echoed" {
		t.Errorf("got %+v", got)
	}
}

// --- Call: correlation headers --------------------------------------------

func TestCall_ThreadIdInjected(t *testing.T) {
	ctx := newCtx(t)
	want := ctx.ID().String()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Thread-Id"); got != want {
			t.Errorf("X-Thread-Id = %q, want %q", got, want)
		}
		if got := r.Header.Get("X-Request-ID"); got != want {
			t.Errorf("X-Request-ID = %q, want %q", got, want)
		}
		_, _ = io.WriteString(w, `{"hello":"ok"}`)
	}))
	defer srv.Close()
	c := newClient(t, srv, EndpointConfig{Method: "GET", Path: "/x"}, Defaults{})

	type req struct{}
	_, err := Call[req, echoBody](ctx, c, "svc", "call", req{})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
}

func TestCall_DefaultsHeadersApplied(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("User-Agent"); got != "svc/1.0" {
			t.Errorf("User-Agent = %q", got)
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	c := newClient(t, srv,
		EndpointConfig{Method: "GET", Path: "/x"},
		Defaults{Headers: map[string]string{"User-Agent": "svc/1.0"}})

	type req struct{}
	_, _ = Call[req, struct{}](newCtx(t), c, "svc", "call", req{})
}

// --- Call: options --------------------------------------------------------

func TestCall_WithExtraHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Trace"); got != "abc" {
			t.Errorf("X-Trace = %q", got)
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	c := newClient(t, srv, EndpointConfig{Method: "GET", Path: "/x"}, Defaults{})

	type req struct{}
	_, _ = Call[req, struct{}](newCtx(t), c, "svc", "call", req{}, WithExtraHeader("X-Trace", "abc"))
}

func TestCall_WithExtraQuery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("debug"); got != "1" {
			t.Errorf("debug = %q", got)
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	c := newClient(t, srv, EndpointConfig{Method: "GET", Path: "/x"}, Defaults{})

	type req struct{}
	_, _ = Call[req, struct{}](newCtx(t), c, "svc", "call", req{}, WithExtraQuery("debug", "1"))
}

func TestCall_TimeoutOverride(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	c := newClient(t, srv, EndpointConfig{Method: "GET", Path: "/x"}, Defaults{})

	type req struct{}
	_, err := Call[req, struct{}](newCtx(t), c, "svc", "call", req{}, WithConfig(CallConfig{Timeout: 50 * time.Millisecond}))
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !IsRetriable(err) {
		t.Errorf("timeout should be retriable; got %v", err)
	}
}

// --- Call: status branches ------------------------------------------------

func TestCall_404NotAcceptable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"detail":"missing"}`)
	}))
	defer srv.Close()
	c := newClient(t, srv, EndpointConfig{Method: "GET", Path: "/x"}, Defaults{})

	type req struct{}
	_, err := Call[req, echoBody](newCtx(t), c, "svc", "call", req{})
	if err == nil {
		t.Fatal("expected error for 404")
	}
	if IsAcceptableStatus(err, 404) {
		t.Error("404 should not be acceptable here (endpoint did not declare it)")
	}
}

func TestCall_404AcceptableViaYAML(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"hello":"missing"}`)
	}))
	defer srv.Close()
	c := newClient(t, srv, EndpointConfig{Method: "GET", Path: "/x", AcceptableStatus: []int{404}}, Defaults{})

	type req struct{}
	resp, err := Call[req, echoBody](newCtx(t), c, "svc", "call", req{})
	if !IsAcceptableStatus(err, 404) {
		t.Fatalf("expected acceptable 404; got err=%v", err)
	}
	if resp.Hello != "missing" {
		t.Errorf("body should still decode on acceptable status; got %+v", resp)
	}
}

func TestCall_404AcceptableViaOption(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	c := newClient(t, srv, EndpointConfig{Method: "GET", Path: "/x"}, Defaults{})

	type req struct{}
	_, err := Call[req, struct{}](newCtx(t), c, "svc", "call", req{}, WithConfig(CallConfig{AcceptableStatus: []int{404}}))
	if !IsAcceptableStatus(err, 404) {
		t.Errorf("expected acceptable; got %v", err)
	}
}

func TestCall_503Retriable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c := newClient(t, srv, EndpointConfig{Method: "GET", Path: "/x"}, Defaults{})

	type req struct{}
	_, err := Call[req, struct{}](newCtx(t), c, "svc", "call", req{})
	if !IsRetriable(err) {
		t.Errorf("503 should be retriable; got %v", err)
	}
}

func TestCall_204NoBody_EmptyStruct(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	c := newClient(t, srv, EndpointConfig{Method: "DELETE", Path: "/x"}, Defaults{})

	type req struct{}
	_, err := Call[req, struct{}](newCtx(t), c, "svc", "call", req{})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
}

// --- Call: cancellation ---------------------------------------------------

func TestCall_ContextCanceled(t *testing.T) {
	releaseSrv := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-releaseSrv
	}))
	defer srv.Close()
	defer close(releaseSrv)
	c := newClient(t, srv, EndpointConfig{Method: "GET", Path: "/x"}, Defaults{})

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
		t.Fatal("expected error from cancellation")
	}
}

// --- Call: unknown service / endpoint -------------------------------------

func TestCall_UnknownService(t *testing.T) {
	c, _ := New(nil)
	type req struct{}
	_, err := Call[req, struct{}](newCtx(t), c, "nope", "call", req{})
	if err == nil || !strings.Contains(err.Error(), "no services configured") {
		t.Errorf("expected no-services error; got %v", err)
	}
}

func TestCall_UnknownEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	c := newClient(t, srv, EndpointConfig{Method: "GET", Path: "/x"}, Defaults{})

	type req struct{}
	_, err := Call[req, struct{}](newCtx(t), c, "svc", "missing", req{})
	if err == nil || !strings.Contains(err.Error(), "no endpoint") {
		t.Errorf("expected missing endpoint error; got %v", err)
	}
}

// --- Call: downstream threadId capture ------------------------------------

func TestCall_DownstreamThreadIdCaptured(t *testing.T) {
	var logBuf bytes.Buffer
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Thread-Id", "downstream-id-xyz")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	cfg := &Config{
		Services: map[string]ServiceConfig{
			"svc": {BaseURL: srv.URL, Endpoints: map[string]EndpointConfig{"call": {Method: "GET", Path: "/x"}}},
		},
	}
	c, err := New(cfg, WithLogger(slog.New(slog.NewJSONHandler(&logBuf, nil))))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	type req struct{}
	_, _ = Call[req, struct{}](newCtx(t), c, "svc", "call", req{})
	if !strings.Contains(logBuf.String(), `"downstreamThreadId":"downstream-id-xyz"`) {
		t.Errorf("slog should carry downstreamThreadId; got: %s", logBuf.String())
	}
}

// --- Call: logBodies / redaction ------------------------------------------

func TestCall_LogBodiesFalse_SuppressesBodies(t *testing.T) {
	var logBuf bytes.Buffer
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"hello":"secret"}`)
	}))
	defer srv.Close()
	f := false
	cfg := &Config{
		Defaults: Defaults{LogBodies: &f},
		Services: map[string]ServiceConfig{
			"svc": {BaseURL: srv.URL, Endpoints: map[string]EndpointConfig{"call": {Method: "GET", Path: "/x"}}},
		},
	}
	c, _ := New(cfg, WithLogger(slog.New(slog.NewJSONHandler(&logBuf, nil))))
	type req struct{}
	_, _ = Call[req, echoBody](newCtx(t), c, "svc", "call", req{})
	if strings.Contains(logBuf.String(), `"hello":"secret"`) {
		t.Errorf("body should be suppressed: %s", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), `"responseBytes":18`) {
		t.Errorf("expected responseBytes; got: %s", logBuf.String())
	}
}

func TestCall_RedactsAuthorizationHeader(t *testing.T) {
	var logBuf bytes.Buffer
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	cfg := &Config{
		Services: map[string]ServiceConfig{
			"svc": {BaseURL: srv.URL, Endpoints: map[string]EndpointConfig{"call": {Method: "GET", Path: "/x"}}},
		},
	}
	c, _ := New(cfg, WithLogger(slog.New(slog.NewJSONHandler(&logBuf, nil))))
	type req struct{}
	_, _ = Call[req, struct{}](newCtx(t), c, "svc", "call", req{}, WithExtraHeader("Authorization", "Bearer s3cr3t"))
	if strings.Contains(logBuf.String(), "s3cr3t") {
		t.Errorf("Authorization should be redacted: %s", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), "REDACTED") {
		t.Errorf("expected REDACTED marker: %s", logBuf.String())
	}
}

// --- defensive: nil client ------------------------------------------------

func TestCall_NilClient(t *testing.T) {
	type req struct{}
	var c *HttpClient
	_, err := Call[req, struct{}](newCtx(t), c, "svc", "call", req{})
	if err == nil || !strings.Contains(err.Error(), "nil client") {
		t.Errorf("expected nil-client error; got %v", err)
	}
}

// keep used import
var _ int32 = atomic.LoadInt32(new(int32))
