package httpclient

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/infra/cache"
)

// Backend-level Cache tests (memory, redis, JSON helpers) live in
// `omnicore/infra/cache/cache_test.go`. The cases below cover the
// httpclient cache MIDDLEWARE — key formula, validation cascade,
// hit/miss/bypass policy against a real httptest.Server. They all
// drive the middleware through a real *HttpClient + cache.NewMemory
// to exercise the same code path bootstrap takes at runtime.

// --- cache key formula ---------------------------------------------------

func TestBuildCacheKey_StableAcrossQueryOrder(t *testing.T) {
	p := cachePolicy{enabled: true}
	r1, _ := http.NewRequest("GET", "https://x.example.com/u?a=1&b=2", nil)
	r2, _ := http.NewRequest("GET", "https://x.example.com/u?b=2&a=1", nil)
	if buildCacheKey("svc", "ep", r1, p) != buildCacheKey("svc", "ep", r2, p) {
		t.Errorf("key should be stable across query order")
	}
}

func TestBuildCacheKey_VaryOnHeader_DifferentValuesDiffer(t *testing.T) {
	p := cachePolicy{enabled: true, varyHeaders: []string{"Accept-Language"}}
	r1, _ := http.NewRequest("GET", "https://x.example.com/u", nil)
	r2, _ := http.NewRequest("GET", "https://x.example.com/u", nil)
	r1.Header.Set("Accept-Language", "en-US")
	r2.Header.Set("Accept-Language", "pt-BR")
	if buildCacheKey("svc", "ep", r1, p) == buildCacheKey("svc", "ep", r2, p) {
		t.Errorf("varyOn header should produce distinct keys")
	}
}

func TestBuildCacheKey_VaryOnQuery_DifferentValuesDiffer(t *testing.T) {
	p := cachePolicy{enabled: true, varyQueries: []string{"tenant"}}
	r1, _ := http.NewRequest("GET", "https://x.example.com/u?tenant=acme", nil)
	r2, _ := http.NewRequest("GET", "https://x.example.com/u?tenant=widget", nil)
	if buildCacheKey("svc", "ep", r1, p) == buildCacheKey("svc", "ep", r2, p) {
		t.Errorf("varyOn query should produce distinct keys")
	}
}

// --- validate ------------------------------------------------------------

func TestValidateEndpointCache_BadMethodRejected(t *testing.T) {
	errs := validateEndpointCache("x", "POST", &EndpointCacheConfig{TTL: Duration(time.Minute)})
	if len(errs) == 0 || !strings.Contains(errs[0], "GET and HEAD") {
		t.Errorf("expected method rejection; got %v", errs)
	}
}

func TestValidateEndpointCache_BadVaryOn(t *testing.T) {
	errs := validateEndpointCache("x", "GET", &EndpointCacheConfig{VaryOn: []string{"badspec"}})
	if len(errs) == 0 || !strings.Contains(errs[0], "header:Name or query:Name") {
		t.Errorf("expected varyOn rejection; got %v", errs)
	}
}

func TestValidateEndpointCache_NegativeTTL(t *testing.T) {
	errs := validateEndpointCache("x", "GET", &EndpointCacheConfig{TTL: Duration(-time.Second)})
	if len(errs) == 0 || !strings.Contains(errs[0], "non-negative") {
		t.Errorf("expected ttl rejection; got %v", errs)
	}
}

// --- resolveCachePolicy --------------------------------------------------

func TestResolveCachePolicy_DefaultsDisabledOverrides(t *testing.T) {
	f := false
	d := &CacheDefaults{Enabled: &f}
	e := &EndpointCacheConfig{TTL: Duration(time.Second)}
	p := resolveCachePolicy(d, e)
	if p.enabled {
		t.Errorf("defaults.enabled=false should disable policy")
	}
}

func TestResolveCachePolicy_EndpointTTLOverridesDefault(t *testing.T) {
	d := &CacheDefaults{DefaultTTL: Duration(time.Minute)}
	e := &EndpointCacheConfig{TTL: Duration(time.Second)}
	p := resolveCachePolicy(d, e)
	if p.ttl != time.Second {
		t.Errorf("endpoint TTL should win: got %v", p.ttl)
	}
}

// --- E2E: cache middleware against httptest ------------------------------

func newCacheClient(t *testing.T, server *httptest.Server, ep EndpointConfig, defaults Defaults) *HttpClient {
	t.Helper()
	cfg := &Config{
		Defaults: defaults,
		Services: map[string]ServiceConfig{
			"svc": {BaseURL: server.URL, Endpoints: map[string]EndpointConfig{"call": ep}},
		},
	}
	// Bootstrap resolves cache.Cache from the top-level cache: yaml block
	// and forwards it via WithCache. The middleware test bench mirrors
	// that wiring with a vanilla in-process memory backend so the cases
	// exercise the same code path the production boot takes.
	c, err := New(cfg,
		WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		WithCache(cache.NewMemory(0)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestCache_MissThenHit_ServerCalledOnce(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		_, _ = io.WriteString(w, `{"v":"x"}`)
	}))
	defer srv.Close()
	c := newCacheClient(t, srv, EndpointConfig{
		Method: "GET", Path: "/x",
		Cache: &EndpointCacheConfig{TTL: Duration(time.Minute)},
	}, Defaults{})

	type req struct{}
	type resp struct {
		V string `json:"v"`
	}
	ctx := configuration.NewAppContextWithRandomID(configuration.LangPTBR)
	_, _ = Call[req, resp](ctx, c, "svc", "call", req{})
	_, _ = Call[req, resp](ctx, c, "svc", "call", req{})
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("server saw %d calls, want 1 (second should hit cache)", got)
	}
}

func TestCache_TTLExpired_HitsServerAgain(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	c := newCacheClient(t, srv, EndpointConfig{
		Method: "GET", Path: "/x",
		Cache: &EndpointCacheConfig{TTL: Duration(10 * time.Millisecond)},
	}, Defaults{})

	type req struct{}
	ctx := configuration.NewAppContextWithRandomID(configuration.LangPTBR)
	_, _ = Call[req, struct{}](ctx, c, "svc", "call", req{})
	time.Sleep(40 * time.Millisecond)
	_, _ = Call[req, struct{}](ctx, c, "svc", "call", req{})
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("server saw %d calls, want 2 (TTL expired)", got)
	}
}

func TestCache_VaryOnHeader_DifferentValuesDifferentEntries(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	c := newCacheClient(t, srv, EndpointConfig{
		Method: "GET", Path: "/x",
		Cache: &EndpointCacheConfig{TTL: Duration(time.Minute), VaryOn: []string{"header:X-Tenant"}},
	}, Defaults{})

	type req struct {
		Tenant string `http:"header,X-Tenant"`
	}
	ctx := configuration.NewAppContextWithRandomID(configuration.LangPTBR)
	_, _ = Call[req, struct{}](ctx, c, "svc", "call", req{Tenant: "a"})
	_, _ = Call[req, struct{}](ctx, c, "svc", "call", req{Tenant: "b"})
	_, _ = Call[req, struct{}](ctx, c, "svc", "call", req{Tenant: "a"})
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("server saw %d calls, want 2 (a + b miss, second a hits)", got)
	}
}

func TestCache_POSTBypass(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	// POST endpoint cannot have cache block (validator rejects). But the
	// chain bypass also covers methods that would never enter cache: any
	// non-GET/HEAD call should hit server twice. Use PUT as the test.
	c := newCacheClient(t, srv, EndpointConfig{
		Method: "PUT", Path: "/x",
	}, Defaults{})

	type req struct{}
	ctx := configuration.NewAppContextWithRandomID(configuration.LangPTBR)
	_, _ = Call[req, struct{}](ctx, c, "svc", "call", req{})
	_, _ = Call[req, struct{}](ctx, c, "svc", "call", req{})
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("server saw %d calls, want 2 (PUT bypasses cache)", got)
	}
}

func TestCache_WithoutCacheOption_Bypasses(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	c := newCacheClient(t, srv, EndpointConfig{
		Method: "GET", Path: "/x",
		Cache: &EndpointCacheConfig{TTL: Duration(time.Minute)},
	}, Defaults{})

	type req struct{}
	ctx := configuration.NewAppContextWithRandomID(configuration.LangPTBR)
	_, _ = Call[req, struct{}](ctx, c, "svc", "call", req{}, WithConfig(CallConfig{NoCache: true}))
	_, _ = Call[req, struct{}](ctx, c, "svc", "call", req{}, WithConfig(CallConfig{NoCache: true}))
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("server saw %d calls, want 2 (per-call bypass)", got)
	}
}

func TestCache_404NotCached_WhenCacheAcceptableFalse(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c := newCacheClient(t, srv, EndpointConfig{
		Method:           "GET",
		Path:             "/x",
		AcceptableStatus: []int{404},
		Cache:            &EndpointCacheConfig{TTL: Duration(time.Minute)},
		CacheAcceptable:  false,
	}, Defaults{})

	type req struct{}
	ctx := configuration.NewAppContextWithRandomID(configuration.LangPTBR)
	_, _ = Call[req, struct{}](ctx, c, "svc", "call", req{})
	_, _ = Call[req, struct{}](ctx, c, "svc", "call", req{})
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("server saw %d calls, want 2 (404 not cached when cacheAcceptable=false)", got)
	}
}

func TestCache_404Cached_WhenCacheAcceptableTrue(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c := newCacheClient(t, srv, EndpointConfig{
		Method:           "GET",
		Path:             "/x",
		AcceptableStatus: []int{404},
		Cache:            &EndpointCacheConfig{TTL: Duration(time.Minute)},
		CacheAcceptable:  true,
	}, Defaults{})

	type req struct{}
	ctx := configuration.NewAppContextWithRandomID(configuration.LangPTBR)
	_, _ = Call[req, struct{}](ctx, c, "svc", "call", req{})
	_, _ = Call[req, struct{}](ctx, c, "svc", "call", req{})
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("server saw %d calls, want 1 (404 cached when cacheAcceptable=true)", got)
	}
}

func TestCache_NoStoreHonored(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Cache-Control", "no-store")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	c := newCacheClient(t, srv, EndpointConfig{
		Method: "GET", Path: "/x",
		Cache: &EndpointCacheConfig{TTL: Duration(time.Minute)},
	}, Defaults{})

	type req struct{}
	ctx := configuration.NewAppContextWithRandomID(configuration.LangPTBR)
	_, _ = Call[req, struct{}](ctx, c, "svc", "call", req{})
	_, _ = Call[req, struct{}](ctx, c, "svc", "call", req{})
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("server saw %d calls, want 2 (Cache-Control: no-store)", got)
	}
}

func TestCache_MaxAgeOverridesTTL(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		// max-age=0 forces immediate expiry; defacto bypass on read.
		w.Header().Set("Cache-Control", "max-age=0")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	c := newCacheClient(t, srv, EndpointConfig{
		Method: "GET", Path: "/x",
		Cache: &EndpointCacheConfig{TTL: Duration(time.Hour)}, // long YAML TTL
	}, Defaults{})

	type req struct{}
	ctx := configuration.NewAppContextWithRandomID(configuration.LangPTBR)
	_, _ = Call[req, struct{}](ctx, c, "svc", "call", req{})
	time.Sleep(20 * time.Millisecond)
	_, _ = Call[req, struct{}](ctx, c, "svc", "call", req{})
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("server saw %d calls, want 2 (max-age=0 expired by 2nd call)", got)
	}
}
