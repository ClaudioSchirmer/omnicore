package httpclient

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newEmptyClient returns a client with no YAML-declared services — the
// starting point for the dynamic-target (webhook) flow.
func newEmptyClient(t *testing.T) *HttpClient {
	t.Helper()
	c, err := New(nil, WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatalf("New(nil): %v", err)
	}
	return c
}

// webhookConfig builds a one-service Config (a "customer" webhook) wired to an
// oauth2-client-credentials provider — the canonical dynamic-target shape.
func webhookConfig(name, srvURL, idpURL string) *Config {
	return &Config{
		Services: map[string]ServiceConfig{
			name: {
				BaseURL:   srvURL,
				Auth:      &ServiceAuthConfig{Provider: "auth:" + name},
				Endpoints: map[string]EndpointConfig{"notify": {Method: "POST", Path: "/events"}},
			},
		},
		AuthProviders: map[string]AuthProviderConfig{
			"auth:" + name: {
				Type:          "oauth2-client-credentials",
				TokenEndpoint: idpURL,
				ClientID:      "id",
				ClientSecret:  "secret",
				TokenCache:    &TokenCacheConfig{Source: "ttl", TTL: Duration(time.Hour)},
			},
		},
	}
}

type rtReq struct{}

// TestRegisterIfAbsent_DispatchAndWarmTokenCache is the core guarantee: a
// service wired in code dispatches by name, fetches its token through the same
// machinery as a YAML service, and reuses the warm token cache across Calls —
// and an idempotent re-register does NOT reset that cache.
func TestRegisterIfAbsent_DispatchAndWarmTokenCache(t *testing.T) {
	var idpHits, resourceHits int32
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&idpHits, 1)
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600}`)
	}))
	defer idp.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&resourceHits, 1)
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization = %q, want Bearer tok", got)
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()

	c := newEmptyClient(t)
	if err := c.RegisterIfAbsent(webhookConfig("webhook:42", srv.URL, idp.URL)); err != nil {
		t.Fatalf("RegisterIfAbsent: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := Call[rtReq, struct{}](newCtx(t), c, "webhook:42", "notify", rtReq{}); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	// Idempotent re-register: same names already present → no-op, warm cache kept.
	if err := c.RegisterIfAbsent(webhookConfig("webhook:42", srv.URL, idp.URL)); err != nil {
		t.Fatalf("re-register: %v", err)
	}
	if _, err := Call[rtReq, struct{}](newCtx(t), c, "webhook:42", "notify", rtReq{}); err != nil {
		t.Fatalf("call after re-register: %v", err)
	}

	if got := atomic.LoadInt32(&idpHits); got != 1 {
		t.Errorf("token endpoint hits = %d, want 1 (cached across calls + idempotent re-register)", got)
	}
	if got := atomic.LoadInt32(&resourceHits); got != 4 {
		t.Errorf("resource hits = %d, want 4", got)
	}
}

// TestCountRegistered_RuntimeOnly proves Count/Registered exclude YAML
// services so a purge loop can never evict a boot upstream.
func TestCountRegistered_RuntimeOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()

	yamlCfg := &Config{Services: map[string]ServiceConfig{
		"core": {BaseURL: srv.URL, Endpoints: map[string]EndpointConfig{"call": {Method: "GET", Path: "/x"}}},
	}}
	c, err := New(yamlCfg, WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.Count() != 0 {
		t.Errorf("fresh runtime count = %d, want 0 (YAML services excluded)", c.Count())
	}

	if err := c.RegisterIfAbsent(&Config{Services: map[string]ServiceConfig{
		"rt": {BaseURL: srv.URL, Endpoints: map[string]EndpointConfig{"call": {Method: "GET", Path: "/x"}}},
	}}); err != nil {
		t.Fatalf("RegisterIfAbsent: %v", err)
	}
	if c.Count() != 1 {
		t.Errorf("count = %d, want 1", c.Count())
	}
	reg := c.Registered()
	if len(reg) != 1 || reg[0].Name != "rt" {
		t.Fatalf("Registered = %+v, want only [rt]", reg)
	}
	if reg[0].RegisteredAt.IsZero() || reg[0].LastUsedAt.IsZero() {
		t.Errorf("timestamps must be set: %+v", reg[0])
	}
}

// TestRegisterIfAbsent_YAMLCollisionNoOp: registering a name that a YAML
// service already owns is a no-op — the original target stands, no runtime
// entry is created.
func TestRegisterIfAbsent_YAMLCollisionNoOp(t *testing.T) {
	var aHits, bHits int32
	srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&aHits, 1)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srvA.Close()
	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&bHits, 1)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srvB.Close()

	c, err := New(&Config{Services: map[string]ServiceConfig{
		"svc": {BaseURL: srvA.URL, Endpoints: map[string]EndpointConfig{"call": {Method: "GET", Path: "/x"}}},
	}}, WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := c.RegisterIfAbsent(&Config{Services: map[string]ServiceConfig{
		"svc": {BaseURL: srvB.URL, Endpoints: map[string]EndpointConfig{"call": {Method: "GET", Path: "/x"}}},
	}}); err != nil {
		t.Fatalf("RegisterIfAbsent: %v", err)
	}
	if _, err := Call[rtReq, struct{}](newCtx(t), c, "svc", "call", rtReq{}); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if aHits != 1 || bHits != 0 {
		t.Errorf("collision must keep the YAML target: aHits=%d bHits=%d", aHits, bHits)
	}
	if c.Count() != 0 {
		t.Errorf("YAML collision must not create a runtime entry; count=%d", c.Count())
	}
}

// TestUnregister removes the runtime service and its runtime-origin provider,
// and refuses unknown / already-removed names.
func TestUnregister(t *testing.T) {
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600}`)
	}))
	defer idp.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()

	c := newEmptyClient(t)
	if err := c.RegisterIfAbsent(webhookConfig("w", srv.URL, idp.URL)); err != nil {
		t.Fatalf("RegisterIfAbsent: %v", err)
	}
	// A second runtime service keeps the registry non-empty after the
	// unregister so the lookup exercises the "unknown service" branch.
	if err := c.RegisterIfAbsent(&Config{Services: map[string]ServiceConfig{
		"keeper": {BaseURL: srv.URL, Endpoints: map[string]EndpointConfig{"notify": {Method: "POST", Path: "/events"}}},
	}}); err != nil {
		t.Fatalf("RegisterIfAbsent(keeper): %v", err)
	}
	if !c.snap().auth.Has("auth:w") {
		t.Fatal("provider not registered")
	}
	if !c.Unregister("w") {
		t.Fatal("Unregister(w) = false, want true")
	}
	if c.Count() != 1 {
		t.Errorf("count after unregister = %d, want 1 (keeper remains)", c.Count())
	}
	if c.snap().auth.Has("auth:w") {
		t.Error("runtime-origin provider must be dropped once its service is gone")
	}
	if _, err := Call[rtReq, struct{}](newCtx(t), c, "w", "notify", rtReq{}); err == nil ||
		!strings.Contains(err.Error(), "unknown service") {
		t.Errorf("expected unknown-service error after unregister, got %v", err)
	}
	if c.Unregister("w") {
		t.Error("second Unregister(w) should be false")
	}
	if c.Unregister("nope") {
		t.Error("Unregister(unknown) should be false")
	}
}

// TestUnregister_RefusesYAML: a YAML-declared service is never removable.
func TestUnregister_RefusesYAML(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	c, err := New(&Config{Services: map[string]ServiceConfig{
		"core": {BaseURL: srv.URL, Endpoints: map[string]EndpointConfig{"call": {Method: "GET", Path: "/x"}}},
	}}, WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.Unregister("core") {
		t.Error("Unregister must refuse a YAML-declared service")
	}
}

// TestUnregister_SharedProviderSurvivesUntilLastReferrer: a runtime provider
// shared by two runtime services is dropped only when the last referrer goes.
func TestUnregister_SharedProviderSurvivesUntilLastReferrer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()

	c := newEmptyClient(t)
	cfg := &Config{
		Services: map[string]ServiceConfig{
			"s1": {BaseURL: srv.URL, Auth: &ServiceAuthConfig{Provider: "shared"}, Endpoints: map[string]EndpointConfig{"call": {Method: "GET", Path: "/x"}}},
			"s2": {BaseURL: srv.URL, Auth: &ServiceAuthConfig{Provider: "shared"}, Endpoints: map[string]EndpointConfig{"call": {Method: "GET", Path: "/x"}}},
		},
		AuthProviders: map[string]AuthProviderConfig{
			"shared": {Type: "bearer-static", Token: "t"},
		},
	}
	if err := c.RegisterIfAbsent(cfg); err != nil {
		t.Fatalf("RegisterIfAbsent: %v", err)
	}
	if !c.Unregister("s1") {
		t.Fatal("Unregister(s1) = false")
	}
	if !c.snap().auth.Has("shared") {
		t.Error("shared provider dropped while s2 still references it")
	}
	if !c.Unregister("s2") {
		t.Fatal("Unregister(s2) = false")
	}
	if c.snap().auth.Has("shared") {
		t.Error("shared provider must be dropped after the last referrer is gone")
	}
}

// TestRegisterIfAbsent_ValidationErrorMergesNothing: a malformed cfg surfaces
// the validation error and leaves the live registry untouched.
func TestRegisterIfAbsent_ValidationErrorMergesNothing(t *testing.T) {
	c := newEmptyClient(t)
	bad := &Config{Services: map[string]ServiceConfig{
		"s": {BaseURL: "https://x.example.com", Auth: &ServiceAuthConfig{Provider: "missing"}, Endpoints: map[string]EndpointConfig{"e": {Method: "GET", Path: "/x"}}},
	}}
	if err := c.RegisterIfAbsent(bad); err == nil {
		t.Fatal("expected validation error for undeclared provider reference")
	}
	if c.Count() != 0 {
		t.Errorf("failed register must merge nothing; count=%d", c.Count())
	}
}

// TestRegisterIfAbsent_Nil is a no-op.
func TestRegisterIfAbsent_Nil(t *testing.T) {
	c := newEmptyClient(t)
	if err := c.RegisterIfAbsent(nil); err != nil {
		t.Errorf("nil cfg: %v", err)
	}
	if c.Count() != 0 {
		t.Error("nil cfg must not register anything")
	}
}

// TestLastUsedAt_AdvancesOnCall: a Call stamps LastUsedAt without touching
// RegisteredAt — the signal a least-recently-used purge sorts on.
func TestLastUsedAt_AdvancesOnCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()

	c := newEmptyClient(t)
	if err := c.RegisterIfAbsent(&Config{Services: map[string]ServiceConfig{
		"rt": {BaseURL: srv.URL, Endpoints: map[string]EndpointConfig{"call": {Method: "GET", Path: "/x"}}},
	}}); err != nil {
		t.Fatalf("RegisterIfAbsent: %v", err)
	}
	before := c.Registered()[0]
	time.Sleep(2 * time.Millisecond)
	if _, err := Call[rtReq, struct{}](newCtx(t), c, "rt", "call", rtReq{}); err != nil {
		t.Fatalf("Call: %v", err)
	}
	after := c.Registered()[0]
	if !after.LastUsedAt.After(before.LastUsedAt) {
		t.Errorf("LastUsedAt did not advance: before=%v after=%v", before.LastUsedAt, after.LastUsedAt)
	}
	if !after.RegisteredAt.Equal(before.RegisteredAt) {
		t.Error("RegisteredAt must not change on use")
	}
}

// TestRuntimeRegistry_Concurrent exercises register / unregister / call / read
// concurrently — meaningful under -race against the lock-free read path.
func TestRuntimeRegistry_Concurrent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()

	c := newEmptyClient(t)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("w%d", i%4)
			_ = c.RegisterIfAbsent(&Config{Services: map[string]ServiceConfig{
				name: {BaseURL: srv.URL, Endpoints: map[string]EndpointConfig{"call": {Method: "GET", Path: "/x"}}},
			}})
			_, _ = Call[rtReq, struct{}](newCtx(t), c, name, "call", rtReq{})
			_ = c.Count()
			_ = c.Registered()
			if i%3 == 0 {
				c.Unregister(name)
			}
		}(i)
	}
	wg.Wait()
}
