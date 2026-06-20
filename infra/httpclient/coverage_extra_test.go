package httpclient

import (
	"testing"
	"time"

	"github.com/ClaudioSchirmer/omnicore/infra/cache"
)

// --- isCacheableMethod --------------------------------------------------

func TestIsCacheableMethod(t *testing.T) {
	cases := []struct {
		method string
		want   bool
	}{
		{"GET", true},
		{"HEAD", true},
		{"get", true},
		{"head", true},
		{"POST", false},
		{"PUT", false},
		{"DELETE", false},
		{"PATCH", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isCacheableMethod(tc.method); got != tc.want {
			t.Errorf("isCacheableMethod(%q) = %v, want %v", tc.method, got, tc.want)
		}
	}
}

// --- SetCache -----------------------------------------------------------

func TestSetCache_SwapAndDisable(t *testing.T) {
	c, err := New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	getter := c.cacheStoreGetter()
	if getter() != nil {
		t.Fatal("fresh client should have no cache")
	}

	mem := cache.NewMemory(0)
	c.SetCache(mem)
	if getter() == nil {
		t.Fatal("SetCache(non-nil) should install a backend")
	}

	// Disable by passing nil.
	c.SetCache(nil)
	if getter() != nil {
		t.Fatal("SetCache(nil) should clear the backend")
	}
}

func TestWithCache_Option(t *testing.T) {
	mem := cache.NewMemory(0)
	c, err := New(nil, WithCache(mem))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.cacheStoreGetter()() == nil {
		t.Fatal("WithCache option should install a backend at construction")
	}
}

// --- breaker ------------------------------------------------------------

func TestBreaker_NilClient(t *testing.T) {
	var c *HttpClient
	if c.breaker("svc", "ep") != nil {
		t.Error("nil client breaker should be nil")
	}
}

func TestBreaker_Disabled(t *testing.T) {
	cfg := &Config{
		Services: map[string]ServiceConfig{
			"s": validService("https://s.example.com"),
		},
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.breaker("s", "getX") != nil {
		t.Error("breaker disabled by default; expected nil state")
	}
}

func TestBreaker_EnabledTracksPerPair(t *testing.T) {
	enabled := true
	cfg := &Config{
		Defaults: Defaults{
			CircuitBreaker: &CircuitBreakerConfig{Enabled: &enabled},
		},
		Services: map[string]ServiceConfig{
			"s": validService("https://s.example.com"),
		},
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.breaker("s", "getX") == nil {
		t.Error("enabled breaker should return a state for a declared pair")
	}
	if c.breaker("s", "unknown") != nil {
		t.Error("unknown endpoint should have no breaker state")
	}
}

// --- buildAuthProvider --------------------------------------------------

func TestBuildAuthProvider_AllTypes(t *testing.T) {
	ttlCache := &TokenCacheConfig{Source: "ttl", TTL: Duration(time.Hour)}
	cases := []struct {
		name    string
		cfg     AuthProviderConfig
		wantErr bool
	}{
		{"none", AuthProviderConfig{Type: "none"}, false},
		{"header-static", AuthProviderConfig{
			Type:   "header-static",
			Attach: &AttachConfig{Name: "X-API-Key", Value: "secret"},
		}, false},
		{"bearer-static", AuthProviderConfig{Type: "bearer-static", Token: "tok"}, false},
		{"basic", AuthProviderConfig{Type: "basic", Username: "u", Password: "p"}, false},
		{"forward-bearer", AuthProviderConfig{Type: "forward-bearer"}, false},
		{"oauth2-client-credentials", AuthProviderConfig{
			Type:          "oauth2-client-credentials",
			TokenEndpoint: "https://idp.example.com/token",
			ClientID:      "id", ClientSecret: "secret",
			TokenCache: ttlCache,
		}, false},
		{"credentials-exchange", AuthProviderConfig{
			Type:              "credentials-exchange",
			TokenEndpoint:     "https://idp.example.com/token",
			RequestCodec:      "json",
			RequestFields:     map[string]string{"a": "1"},
			ResponseTokenPath: "$.access_token",
			TokenCache:        ttlCache,
		}, false},
		{"unimplemented", AuthProviderConfig{Type: "oauth2-magic"}, true},
		// Constructor-level validation failure (e.g. missing token) surfaces
		// as an error even though Validate would normally gate it upstream.
		{"bearer-static missing token", AuthProviderConfig{Type: "bearer-static"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := buildAuthProvider(tc.name, tc.cfg)
			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error for %s", tc.name)
				}
				return
			}
			if err != nil {
				t.Fatalf("buildAuthProvider(%s): %v", tc.name, err)
			}
			if p == nil {
				t.Fatalf("buildAuthProvider(%s) returned nil provider", tc.name)
			}
			if p.Name() != tc.name {
				t.Errorf("provider name = %q, want %q", p.Name(), tc.name)
			}
		})
	}
}
