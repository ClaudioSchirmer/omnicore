package httpclient

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
)

// captureHandler is a minimal slog.Handler that records every emitted
// record so tests can assert individual attributes (e.g. obs.URL on
// pre-build failures). Concurrency-safe — the logging middleware may emit
// from a goroutine in retry paths.
type captureHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *captureHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}

func (h *captureHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(_ string) slog.Handler      { return h }

// stringAttr returns the value of the named attribute on the first record
// whose Message matches msg, or ("", false) when not found.
func (h *captureHandler) stringAttr(msg, name string) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		if r.Message != msg {
			continue
		}
		var found string
		var ok bool
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == name {
				found = a.Value.String()
				ok = true
				return false
			}
			return true
		})
		if ok {
			return found, true
		}
	}
	return "", false
}

// --- Bug 23 regression -----------------------------------------------------

// failingResolver always returns an error, forcing the resolveBaseURL
// branch in Call to emit its observation before any chain layer runs.
type failingResolver struct{ err error }

func (r failingResolver) Resolve(_ context.Context, _ string) (string, error) {
	return "", r.err
}

// On resolveBaseURL failure the observation must surface the path the
// caller actually intended (CallConfig.Path override) and the override
// baseURL — not the YAML defaults — so operator logs reflect the real
// attempted dial. Pre-fix it logged "svc.baseURL + ep.path" verbatim.
func TestObservation_ResolveBaseURLFailure_LogsEffectiveURL(t *testing.T) {
	capture := &captureHandler{}
	cfg := &Config{
		Services: map[string]ServiceConfig{
			"svc": {
				BaseURL: "https://yaml.example.com",
				Endpoints: map[string]EndpointConfig{
					"call": {Method: "GET", Path: "/yaml/path"},
				},
			},
		},
	}
	c, err := New(cfg,
		WithLogger(slog.New(capture)),
		WithResolver(failingResolver{err: errors.New("dns unavailable")}),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	type req struct{}
	_, callErr := Call[req, struct{}](
		configuration.NewAppContextWithRandomID(configuration.LangPTBR),
		c, "svc", "call", req{},
		WithConfig(CallConfig{BaseURL: "https://override.example.com", Path: "/runtime/path"}),
	)
	if callErr == nil {
		t.Fatal("expected error from failing resolver")
	}
	// Override baseURL wins over the resolver consultation: when the
	// caller supplied CallConfig.BaseURL, the resolver is bypassed and
	// no log appears at all. So drop the override to actually hit the
	// resolver path; keep only Path override.
	_, callErr = Call[req, struct{}](
		configuration.NewAppContextWithRandomID(configuration.LangPTBR),
		c, "svc", "call", req{},
		WithConfig(CallConfig{Path: "/runtime/path"}),
	)
	if callErr == nil {
		t.Fatal("expected error from failing resolver (no override)")
	}
	url, ok := capture.stringAttr("http.outbound", "url")
	if !ok {
		t.Fatal("no slog record captured for failed resolveBaseURL path")
	}
	if !strings.HasSuffix(url, "/runtime/path") {
		t.Errorf("obs.URL should end with the override path; got %q", url)
	}
	if strings.Contains(url, "/yaml/path") {
		t.Errorf("obs.URL should NOT contain the YAML path when override is set; got %q", url)
	}
}

// On binding.BuildRequest failure (path template references a placeholder
// missing from the request struct) the observation must reflect the
// override path the caller dialed — not the YAML default. Pre-fix
// obs.URL = baseURL + ep.path; post-fix uses baseURL + meta.Path.
func TestObservation_BuildRequestFailure_LogsEffectivePath(t *testing.T) {
	capture := &captureHandler{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	cfg := &Config{
		Services: map[string]ServiceConfig{
			"svc": {
				BaseURL: srv.URL,
				Endpoints: map[string]EndpointConfig{
					"call": {Method: "GET", Path: "/yaml/{id}"},
				},
			},
		},
	}
	c, err := New(cfg, WithLogger(slog.New(capture)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Override path declares a placeholder not present on the request DTO,
	// forcing binding.BuildRequest to error before dispatch.
	type req struct{}
	_, callErr := Call[req, struct{}](
		configuration.NewAppContextWithRandomID(configuration.LangPTBR),
		c, "svc", "call", req{},
		WithConfig(CallConfig{Path: "/runtime/{missingPlaceholder}"}),
	)
	if callErr == nil {
		t.Fatal("expected BuildRequest error for unbound path placeholder")
	}
	url, ok := capture.stringAttr("http.outbound", "url")
	if !ok {
		t.Fatal("no slog record captured for failed BuildRequest path")
	}
	if !strings.Contains(url, "/runtime/{missingPlaceholder}") {
		t.Errorf("obs.URL should contain the override path; got %q", url)
	}
	if strings.Contains(url, "/yaml/{id}") {
		t.Errorf("obs.URL should NOT contain the YAML path; got %q", url)
	}
}
