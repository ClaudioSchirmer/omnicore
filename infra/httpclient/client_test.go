package httpclient

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestNew_NilConfig_ReturnsClient(t *testing.T) {
	c, err := New(nil)
	if err != nil {
		t.Fatalf("New(nil) error: %v", err)
	}
	if c == nil {
		t.Fatal("New(nil) returned nil client")
	}
}

func TestNew_EmptyConfig_ReturnsClient(t *testing.T) {
	c, err := New(&Config{})
	if err != nil {
		t.Fatalf("New(&Config{}) error: %v", err)
	}
	if c == nil {
		t.Fatal("New(&Config{}) returned nil client")
	}
}

func TestNew_ValidConfig_BuildsServicePerEntry(t *testing.T) {
	cfg := &Config{
		Services: map[string]ServiceConfig{
			"keycloak": validService("https://kc.example.com"),
			"payment":  validService("https://pay.example.com"),
		},
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if len(c.services) != 2 {
		t.Fatalf("want 2 services, got %d", len(c.services))
	}
	if _, err := c.service("keycloak"); err != nil {
		t.Errorf("service(keycloak): %v", err)
	}
	if _, err := c.service("payment"); err != nil {
		t.Errorf("service(payment): %v", err)
	}
}

func TestNew_PerServiceTransportIsolation(t *testing.T) {
	cfg := &Config{
		Services: map[string]ServiceConfig{
			"a": validService("https://a.example.com"),
			"b": validService("https://b.example.com"),
		},
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	a, _ := c.service("a")
	b, _ := c.service("b")
	if a.httpClient == b.httpClient {
		t.Fatal("services share the same *http.Client; expected isolated instances per the per-service-transport contract")
	}
	ta, ok := a.httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatal("service 'a' transport is not *http.Transport")
	}
	tb, ok := b.httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatal("service 'b' transport is not *http.Transport")
	}
	if ta == tb {
		t.Fatal("services share the same *http.Transport instance")
	}
}

func TestNew_TransportCarriesPoolDefaults(t *testing.T) {
	cfg := &Config{
		Services: map[string]ServiceConfig{
			"x": validService("https://x.example.com"),
		},
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s, _ := c.service("x")
	tr, _ := s.httpClient.Transport.(*http.Transport)
	if tr.MaxIdleConnsPerHost != defaultMaxIdleConnsPerHost {
		t.Errorf("MaxIdleConnsPerHost = %d, want %d", tr.MaxIdleConnsPerHost, defaultMaxIdleConnsPerHost)
	}
	if tr.MaxConnsPerHost != defaultMaxConnsPerHost {
		t.Errorf("MaxConnsPerHost = %d, want %d", tr.MaxConnsPerHost, defaultMaxConnsPerHost)
	}
	if tr.IdleConnTimeout != defaultIdleConnTimeout {
		t.Errorf("IdleConnTimeout = %v, want %v", tr.IdleConnTimeout, defaultIdleConnTimeout)
	}
}

func TestNew_AppliesTimeoutCascade(t *testing.T) {
	cases := []struct {
		name             string
		defaultsTimeout  Duration
		serviceTimeout   Duration
		wantClientTimeout time.Duration
	}{
		{"defaults win when service omits", Duration(5 * time.Second), 0, 5 * time.Second},
		{"service overrides defaults", Duration(5 * time.Second), Duration(2 * time.Second), 2 * time.Second},
		{"framework fallback when both omitted", 0, 0, 30 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc := validService("https://t.example.com")
			sc.Timeout = tc.serviceTimeout
			cfg := &Config{
				Defaults: Defaults{Timeout: tc.defaultsTimeout},
				Services: map[string]ServiceConfig{"s": sc},
			}
			c, err := New(cfg)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			s, _ := c.service("s")
			if s.httpClient.Timeout != tc.wantClientTimeout {
				t.Errorf("client timeout = %v, want %v", s.httpClient.Timeout, tc.wantClientTimeout)
			}
		})
	}
}

func TestNew_HeaderCascade_DefaultsServiceEndpoint(t *testing.T) {
	cfg := &Config{
		Defaults: Defaults{Headers: map[string]string{"User-Agent": "svc/1.0", "Common": "default"}},
		Services: map[string]ServiceConfig{
			"s": {
				BaseURL: "https://s.example.com",
				Headers: map[string]string{"Common": "service", "X-Tenant": "acme"},
				Endpoints: map[string]EndpointConfig{
					"e": {
						Method:  "GET",
						Path:    "/x",
						Headers: map[string]string{"Common": "endpoint", "Accept": "application/json"},
					},
				},
			},
		},
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s, _ := c.service("s")
	ep := s.endpoints["e"]
	if got := ep.headers["Common"]; got != "endpoint" {
		t.Errorf("endpoint should win on Common: got %q, want %q", got, "endpoint")
	}
	if got := ep.headers["X-Tenant"]; got != "acme" {
		t.Errorf("service header X-Tenant should reach endpoint: got %q", got)
	}
	if got := ep.headers["User-Agent"]; got != "svc/1.0" {
		t.Errorf("defaults header User-Agent should reach endpoint: got %q", got)
	}
	if got := ep.headers["Accept"]; got != "application/json" {
		t.Errorf("endpoint-only header Accept: got %q", got)
	}
}

func TestNew_ValidationFailure_ReturnsError(t *testing.T) {
	// Phase 9 relaxed empty baseURL to "valid"; an invalid non-empty
	// baseURL still rejects at boot so operators do not ship a typo.
	cfg := &Config{
		Services: map[string]ServiceConfig{
			"broken": {BaseURL: "not a url", Endpoints: map[string]EndpointConfig{
				"x": {Method: "GET", Path: "/x"},
			}},
		},
	}
	if _, err := New(cfg); err == nil {
		t.Fatal("expected validation error for malformed baseURL")
	} else if !strings.Contains(err.Error(), "baseURL") {
		t.Errorf("error should mention baseURL: %v", err)
	}
}

func TestService_UnknownName_Errors(t *testing.T) {
	c, _ := New(nil)
	if _, err := c.service("nope"); err == nil {
		t.Fatal("expected error for unknown service on empty client")
	}
}

// validService produces a minimal ServiceConfig that passes Validate.
func validService(baseURL string) ServiceConfig {
	return ServiceConfig{
		BaseURL: baseURL,
		Endpoints: map[string]EndpointConfig{
			"getX": {Method: "GET", Path: "/x"},
		},
	}
}
