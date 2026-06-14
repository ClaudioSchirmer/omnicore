package auth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- constructor validation --------------------------------------------

func TestCredentialsExchange_RequiresFields(t *testing.T) {
	cases := []struct {
		name string
		opts CredentialsExchangeOptions
		want string
	}{
		{"no endpoint", CredentialsExchangeOptions{Name: "p", RequestFields: map[string]string{"a": "1"}, ResponseTokenPath: "$.t"}, "tokenEndpoint"},
		{"no fields", CredentialsExchangeOptions{Name: "p", TokenEndpoint: "x", ResponseTokenPath: "$.t"}, "requestFields"},
		{"no path", CredentialsExchangeOptions{Name: "p", TokenEndpoint: "x", RequestFields: map[string]string{"a": "1"}}, "responseTokenPath"},
		{"bad codec", CredentialsExchangeOptions{Name: "p", TokenEndpoint: "x", RequestFields: map[string]string{"a": "1"}, ResponseTokenPath: "$.t", RequestCodec: "xml"}, "json|form-urlencoded"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewCredentialsExchangeProvider(tc.opts)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("want error containing %q; got %v", tc.want, err)
			}
		})
	}
}

func TestCredentialsExchange_DefaultsAttachAndCodec(t *testing.T) {
	p, err := NewCredentialsExchangeProvider(CredentialsExchangeOptions{
		Name: "p", TokenEndpoint: "https://x", RequestFields: map[string]string{"u": "alice"},
		ResponseTokenPath: "$.access_token",
		Cache:             TokenCacheConfig{Source: SourceTTL, TTL: time.Hour},
	})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	if p.attach.Name != "Authorization" || p.attach.Format != "Bearer {token}" {
		t.Errorf("default attach not applied: %+v", p.attach)
	}
	if p.requestCodec != "form-urlencoded" {
		t.Errorf("default codec = %q; want form-urlencoded", p.requestCodec)
	}
}

// --- E2E: JSON body with custom field names ----------------------------

func newCredsIdP(t *testing.T, response string, hits *int32, captureBody *string, captureCT *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			atomic.AddInt32(hits, 1)
		}
		body, _ := io.ReadAll(r.Body)
		if captureBody != nil {
			*captureBody = string(body)
		}
		if captureCT != nil {
			*captureCT = r.Header.Get("Content-Type")
		}
		_, _ = io.WriteString(w, response)
	}))
}

func TestCredentialsExchange_JSONBody_CustomFields(t *testing.T) {
	var captured, ct string
	idp := newCredsIdP(t, `{"access_token":"tok-abc","expires_in":3600}`, nil, &captured, &ct)
	defer idp.Close()
	p, err := NewCredentialsExchangeProvider(CredentialsExchangeOptions{
		Name:          "kc-custom",
		TokenEndpoint: idp.URL,
		RequestCodec:  "json",
		RequestFields: map[string]string{
			"user": "alice",
			"pass": "wonder",
		},
		ResponseTokenPath: "$.access_token",
		Cache:             TokenCacheConfig{Source: SourceTTL, TTL: time.Hour},
	})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	req, _ := http.NewRequestWithContext(context.Background(), "GET", "https://r.example.com", nil)
	if err := p.Apply(req); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if req.Header.Get("Authorization") != "Bearer tok-abc" {
		t.Errorf("Authorization = %q", req.Header.Get("Authorization"))
	}
	if ct != "application/json" {
		t.Errorf("Content-Type = %q; want application/json", ct)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(captured), &got); err != nil {
		t.Fatalf("captured body not valid JSON: %s", captured)
	}
	if got["user"] != "alice" || got["pass"] != "wonder" {
		t.Errorf("body fields = %+v", got)
	}
}

// --- E2E: form body RFC OAuth2 password shape ---------------------------

func TestCredentialsExchange_FormBody_PasswordGrantShape(t *testing.T) {
	var captured, ct string
	idp := newCredsIdP(t, `{"access_token":"tok-form","expires_in":3600}`, nil, &captured, &ct)
	defer idp.Close()
	p, err := NewCredentialsExchangeProvider(CredentialsExchangeOptions{
		Name:          "kc-password",
		TokenEndpoint: idp.URL,
		RequestCodec:  "form-urlencoded",
		RequestFields: map[string]string{
			"grant_type":    "password",
			"client_id":     "myapp",
			"client_secret": "shh",
			"username":      "alice",
			"password":      "wonder",
		},
		ResponseTokenPath: "$.access_token",
		Cache:             TokenCacheConfig{Source: SourceTTL, TTL: time.Hour},
	})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	req, _ := http.NewRequestWithContext(context.Background(), "GET", "https://r.example.com", nil)
	if err := p.Apply(req); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if req.Header.Get("Authorization") != "Bearer tok-form" {
		t.Errorf("Authorization = %q", req.Header.Get("Authorization"))
	}
	if ct != "application/x-www-form-urlencoded" {
		t.Errorf("Content-Type = %q", ct)
	}
	got, err := url.ParseQuery(captured)
	if err != nil {
		t.Fatalf("parse form body: %v", err)
	}
	if got.Get("grant_type") != "password" || got.Get("username") != "alice" {
		t.Errorf("form body = %v", got)
	}
}

// --- E2E: nested JSONPath ----------------------------------------------

func TestCredentialsExchange_NestedJSONPath(t *testing.T) {
	idp := newCredsIdP(t, `{"data":{"auth":{"token":"nested-tok"}},"expires_in":3600}`, nil, nil, nil)
	defer idp.Close()
	p, err := NewCredentialsExchangeProvider(CredentialsExchangeOptions{
		Name: "nested", TokenEndpoint: idp.URL,
		RequestCodec: "json", RequestFields: map[string]string{"u": "x"},
		ResponseTokenPath: "$.data.auth.token",
		Cache:             TokenCacheConfig{Source: SourceTTL, TTL: time.Hour},
	})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	req, _ := http.NewRequestWithContext(context.Background(), "GET", "https://r.example.com", nil)
	if err := p.Apply(req); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if req.Header.Get("Authorization") != "Bearer nested-tok" {
		t.Errorf("nested token = %q", req.Header.Get("Authorization"))
	}
}

// --- E2E: cache + single-flight ----------------------------------------

func TestCredentialsExchange_CachesToken(t *testing.T) {
	var hits int32
	idp := newCredsIdP(t, `{"access_token":"cached","expires_in":3600}`, &hits, nil, nil)
	defer idp.Close()
	p, _ := NewCredentialsExchangeProvider(CredentialsExchangeOptions{
		Name: "c", TokenEndpoint: idp.URL,
		RequestCodec: "form-urlencoded", RequestFields: map[string]string{"u": "x"},
		ResponseTokenPath: "$.access_token",
		Cache:             TokenCacheConfig{Source: SourceTTL, TTL: time.Hour},
	})
	for i := 0; i < 3; i++ {
		req, _ := http.NewRequestWithContext(context.Background(), "GET", "https://r.example.com", nil)
		_ = p.Apply(req)
	}
	if atomic.LoadInt32(&hits) != 1 {
		t.Errorf("token endpoint hit %d times; want 1", hits)
	}
}

func TestCredentialsExchange_SingleFlight(t *testing.T) {
	var hits int32
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		time.Sleep(40 * time.Millisecond)
		_, _ = io.WriteString(w, `{"access_token":"sf","expires_in":3600}`)
	}))
	defer idp.Close()
	p, _ := NewCredentialsExchangeProvider(CredentialsExchangeOptions{
		Name: "sf", TokenEndpoint: idp.URL,
		RequestCodec: "form-urlencoded", RequestFields: map[string]string{"u": "x"},
		ResponseTokenPath: "$.access_token",
		Cache:             TokenCacheConfig{Source: SourceTTL, TTL: time.Hour, SingleFlight: true},
	})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequestWithContext(context.Background(), "GET", "https://r.example.com", nil)
			_ = p.Apply(req)
		}()
	}
	wg.Wait()
	if atomic.LoadInt32(&hits) != 1 {
		t.Errorf("single-flight failed: hits = %d; want 1", hits)
	}
}

// --- E2E: invalidate + requestHeaders Basic Auth -----------------------

func TestCredentialsExchange_Invalidate(t *testing.T) {
	var hits int32
	idp := newCredsIdP(t, `{"access_token":"t","expires_in":3600}`, &hits, nil, nil)
	defer idp.Close()
	p, _ := NewCredentialsExchangeProvider(CredentialsExchangeOptions{
		Name: "inv", TokenEndpoint: idp.URL,
		RequestCodec: "json", RequestFields: map[string]string{"a": "1"},
		ResponseTokenPath: "$.access_token",
		Cache:             TokenCacheConfig{Source: SourceTTL, TTL: time.Hour},
	})
	req, _ := http.NewRequestWithContext(context.Background(), "GET", "https://r.example.com", nil)
	_ = p.Apply(req)
	p.Invalidate()
	_ = p.Apply(req)
	if atomic.LoadInt32(&hits) != 2 {
		t.Errorf("invalidate forces re-acquire; hits = %d", hits)
	}
}

func TestCredentialsExchange_RequestHeaders(t *testing.T) {
	var seenAuth string
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{"access_token":"t","expires_in":3600}`)
	}))
	defer idp.Close()
	p, _ := NewCredentialsExchangeProvider(CredentialsExchangeOptions{
		Name: "basic", TokenEndpoint: idp.URL,
		RequestCodec: "form-urlencoded", RequestFields: map[string]string{"grant_type": "password"},
		RequestHeaders:    map[string]string{"Authorization": "Basic abc=="},
		ResponseTokenPath: "$.access_token",
		Cache:             TokenCacheConfig{Source: SourceTTL, TTL: time.Hour},
	})
	req, _ := http.NewRequestWithContext(context.Background(), "GET", "https://r.example.com", nil)
	_ = p.Apply(req)
	if seenAuth != "Basic abc==" {
		t.Errorf("Basic auth header not sent to IdP; got %q", seenAuth)
	}
}

// --- E2E: failure paths -------------------------------------------------

func TestCredentialsExchange_BadResponse_TokenMissing(t *testing.T) {
	idp := newCredsIdP(t, `{"foo":"bar"}`, nil, nil, nil)
	defer idp.Close()
	p, _ := NewCredentialsExchangeProvider(CredentialsExchangeOptions{
		Name: "x", TokenEndpoint: idp.URL,
		RequestCodec: "form-urlencoded", RequestFields: map[string]string{"a": "1"},
		ResponseTokenPath: "$.access_token",
		Cache:             TokenCacheConfig{Source: SourceTTL, TTL: time.Hour},
	})
	req, _ := http.NewRequestWithContext(context.Background(), "GET", "https://r.example.com", nil)
	if err := p.Apply(req); err == nil {
		t.Error("expected error: response missing access_token")
	}
}

func TestCredentialsExchange_EndpointError(t *testing.T) {
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"invalid_client"}`)
	}))
	defer idp.Close()
	p, _ := NewCredentialsExchangeProvider(CredentialsExchangeOptions{
		Name: "x", TokenEndpoint: idp.URL,
		RequestCodec: "json", RequestFields: map[string]string{"a": "1"},
		ResponseTokenPath: "$.access_token",
		Cache:             TokenCacheConfig{Source: SourceTTL, TTL: time.Hour},
	})
	req, _ := http.NewRequestWithContext(context.Background(), "GET", "https://r.example.com", nil)
	if err := p.Apply(req); err == nil {
		t.Error("expected error from token endpoint")
	}
}

// --- multi-tenant via RequestFieldsFromCtx ----------------------------

// fakeReaderCtx satisfies appContextReader and context.Context for tests.
type fakeReaderCtx struct {
	context.Context
	store map[string]any
}

func newFakeCtx(values map[string]any) fakeReaderCtx {
	return fakeReaderCtx{Context: context.Background(), store: values}
}

func (f fakeReaderCtx) Get(key string) (any, bool) {
	v, ok := f.store[key]
	return v, ok
}

func TestCredentialsExchange_CtxFields_ReadsFromAppContext(t *testing.T) {
	var captured string
	idp := newCredsIdP(t, `{"access_token":"tok-alice","expires_in":3600}`, nil, &captured, nil)
	defer idp.Close()
	p, err := NewCredentialsExchangeProvider(CredentialsExchangeOptions{
		Name: "p", TokenEndpoint: idp.URL,
		RequestCodec: "json",
		RequestFieldsFromCtx: map[string]string{
			"user": "idp.user",
			"pass": "idp.pass",
		},
		ResponseTokenPath: "$.access_token",
		Cache:             TokenCacheConfig{Source: SourceTTL, TTL: time.Hour},
	})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	ctx := newFakeCtx(map[string]any{"idp.user": "alice", "idp.pass": "wonder"})
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://r.example.com", nil)
	if err := p.Apply(req); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if req.Header.Get("Authorization") != "Bearer tok-alice" {
		t.Errorf("Authorization = %q", req.Header.Get("Authorization"))
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(captured), &got); err != nil {
		t.Fatalf("captured body: %s", captured)
	}
	if got["user"] != "alice" || got["pass"] != "wonder" {
		t.Errorf("body fields = %+v", got)
	}
}

func TestCredentialsExchange_CtxFields_MissingValue_Errors(t *testing.T) {
	idp := newCredsIdP(t, `{"access_token":"x","expires_in":3600}`, nil, nil, nil)
	defer idp.Close()
	p, _ := NewCredentialsExchangeProvider(CredentialsExchangeOptions{
		Name: "p", TokenEndpoint: idp.URL,
		RequestCodec:         "json",
		RequestFieldsFromCtx: map[string]string{"user": "idp.user"},
		ResponseTokenPath:    "$.access_token",
		Cache:                TokenCacheConfig{Source: SourceTTL, TTL: time.Hour},
	})
	ctx := newFakeCtx(map[string]any{}) // empty
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://r.example.com", nil)
	err := p.Apply(req)
	if err == nil || !strings.Contains(err.Error(), "AppContext is missing key") {
		t.Errorf("expected missing-key error; got %v", err)
	}
}

func TestCredentialsExchange_CtxFields_NonAppContext_Errors(t *testing.T) {
	idp := newCredsIdP(t, `{"access_token":"x","expires_in":3600}`, nil, nil, nil)
	defer idp.Close()
	p, _ := NewCredentialsExchangeProvider(CredentialsExchangeOptions{
		Name: "p", TokenEndpoint: idp.URL,
		RequestCodec:         "json",
		RequestFieldsFromCtx: map[string]string{"user": "idp.user"},
		ResponseTokenPath:    "$.access_token",
		Cache:                TokenCacheConfig{Source: SourceTTL, TTL: time.Hour},
	})
	req, _ := http.NewRequestWithContext(context.Background(), "GET", "https://r.example.com", nil)
	err := p.Apply(req)
	if err == nil || !strings.Contains(err.Error(), "require AppContext") {
		t.Errorf("expected AppContext requirement error; got %v", err)
	}
}

func TestCredentialsExchange_MixedFields_StaticPlusCtx(t *testing.T) {
	var captured string
	idp := newCredsIdP(t, `{"access_token":"mixed","expires_in":3600}`, nil, &captured, nil)
	defer idp.Close()
	p, _ := NewCredentialsExchangeProvider(CredentialsExchangeOptions{
		Name: "p", TokenEndpoint: idp.URL,
		RequestCodec: "form-urlencoded",
		RequestFields: map[string]string{
			"grant_type": "password",
			"client_id":  "static-client",
		},
		RequestFieldsFromCtx: map[string]string{
			"username": "idp.user",
			"password": "idp.pass",
		},
		ResponseTokenPath: "$.access_token",
		Cache:             TokenCacheConfig{Source: SourceTTL, TTL: time.Hour},
	})
	ctx := newFakeCtx(map[string]any{"idp.user": "alice", "idp.pass": "wonder"})
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://r.example.com", nil)
	if err := p.Apply(req); err != nil {
		t.Fatalf("apply: %v", err)
	}
	form, _ := url.ParseQuery(captured)
	if form.Get("grant_type") != "password" || form.Get("client_id") != "static-client" {
		t.Errorf("static fields lost: %v", form)
	}
	if form.Get("username") != "alice" || form.Get("password") != "wonder" {
		t.Errorf("ctx fields wrong: %v", form)
	}
}

func TestCredentialsExchange_CtxFields_PerIdentityCache(t *testing.T) {
	var hits int32
	tokens := map[string]string{}
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		body, _ := io.ReadAll(r.Body)
		var got map[string]string
		_ = json.Unmarshal(body, &got)
		tok := "tok-" + got["user"]
		tokens[got["user"]] = tok
		_, _ = io.WriteString(w, `{"access_token":"`+tok+`","expires_in":3600}`)
	}))
	defer idp.Close()
	p, _ := NewCredentialsExchangeProvider(CredentialsExchangeOptions{
		Name: "p", TokenEndpoint: idp.URL,
		RequestCodec:         "json",
		RequestFieldsFromCtx: map[string]string{"user": "idp.user"},
		ResponseTokenPath:    "$.access_token",
		Cache:                TokenCacheConfig{Source: SourceTTL, TTL: time.Hour},
	})

	// Tenant A
	ctxA := newFakeCtx(map[string]any{"idp.user": "alice"})
	reqA, _ := http.NewRequestWithContext(ctxA, "GET", "https://r.example.com", nil)
	_ = p.Apply(reqA)
	if reqA.Header.Get("Authorization") != "Bearer tok-alice" {
		t.Errorf("tenant A header wrong: %q", reqA.Header.Get("Authorization"))
	}

	// Tenant B
	ctxB := newFakeCtx(map[string]any{"idp.user": "bob"})
	reqB, _ := http.NewRequestWithContext(ctxB, "GET", "https://r.example.com", nil)
	_ = p.Apply(reqB)
	if reqB.Header.Get("Authorization") != "Bearer tok-bob" {
		t.Errorf("tenant B header wrong: %q", reqB.Header.Get("Authorization"))
	}

	// Tenant A again — must hit cache, no new IdP request
	reqA2, _ := http.NewRequestWithContext(ctxA, "GET", "https://r.example.com", nil)
	_ = p.Apply(reqA2)
	if reqA2.Header.Get("Authorization") != "Bearer tok-alice" {
		t.Errorf("tenant A second call wrong: %q", reqA2.Header.Get("Authorization"))
	}

	if atomic.LoadInt32(&hits) != 2 {
		t.Errorf("IdP hits = %d; want 2 (one per tenant; tenant A second call cached)", hits)
	}
}

func TestCredentialsExchange_CtxFields_SameIdentity_Cached(t *testing.T) {
	var hits int32
	idp := newCredsIdP(t, `{"access_token":"t","expires_in":3600}`, &hits, nil, nil)
	defer idp.Close()
	p, _ := NewCredentialsExchangeProvider(CredentialsExchangeOptions{
		Name: "p", TokenEndpoint: idp.URL,
		RequestCodec:         "json",
		RequestFieldsFromCtx: map[string]string{"user": "idp.user"},
		ResponseTokenPath:    "$.access_token",
		Cache:                TokenCacheConfig{Source: SourceTTL, TTL: time.Hour},
	})
	ctx := newFakeCtx(map[string]any{"idp.user": "alice"})
	for i := 0; i < 5; i++ {
		req, _ := http.NewRequestWithContext(ctx, "GET", "https://r.example.com", nil)
		_ = p.Apply(req)
	}
	if atomic.LoadInt32(&hits) != 1 {
		t.Errorf("same identity should cache; hits = %d", hits)
	}
}

func TestCredentialsExchange_RevocableInterface(t *testing.T) {
	p, _ := NewCredentialsExchangeProvider(CredentialsExchangeOptions{
		Name: "x", TokenEndpoint: "https://x",
		RequestCodec: "json", RequestFields: map[string]string{"a": "1"},
		ResponseTokenPath: "$.access_token",
		Cache:             TokenCacheConfig{Source: SourceTTL, TTL: time.Hour},
	})
	var _ RevocableProvider = p
}
