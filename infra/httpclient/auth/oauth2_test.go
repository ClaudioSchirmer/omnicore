package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- jwt -----------------------------------------------------------------

func makeJWT(claims map[string]any) string {
	payload, _ := json.Marshal(claims)
	return "header." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func TestDecodeJWTExp_HappyPath(t *testing.T) {
	exp := time.Now().Add(time.Hour).Unix()
	tok := makeJWT(map[string]any{"exp": exp})
	got, err := decodeJWTExp(tok)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Unix() != exp {
		t.Errorf("exp = %v, want %v", got.Unix(), exp)
	}
}

func TestDecodeJWTExp_BadStructure(t *testing.T) {
	if _, err := decodeJWTExp("only.two"); err == nil {
		t.Error("expected error for two-part token")
	}
}

func TestDecodeJWTExp_NoExpClaim(t *testing.T) {
	tok := makeJWT(map[string]any{"sub": "alice"})
	if _, err := decodeJWTExp(tok); err == nil {
		t.Error("expected error for missing exp")
	}
}

// --- jsonpath ------------------------------------------------------------

func TestWalkJSONPath_HappyPath(t *testing.T) {
	root := map[string]any{"a": map[string]any{"b": map[string]any{"c": 42.0}}}
	v, err := walkJSONPath("$.a.b.c", root)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if v.(float64) != 42.0 {
		t.Errorf("got %v", v)
	}
}

func TestWalkJSONPath_MissingKey(t *testing.T) {
	root := map[string]any{"a": map[string]any{"b": 1}}
	if _, err := walkJSONPath("$.a.c", root); err == nil {
		t.Error("expected error for missing key")
	}
}

func TestWalkJSONPath_RootReturn(t *testing.T) {
	root := map[string]any{"x": 1}
	v, err := walkJSONPath("$", root)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if v.(map[string]any)["x"] != 1 {
		t.Error("root return broken")
	}
}

// --- tokenCache ----------------------------------------------------------

func TestTokenCache_GetSetMissAfterExpiry(t *testing.T) {
	c := &tokenCache{}
	if _, ok := c.Get(0); ok {
		t.Error("empty cache should miss")
	}
	c.Set("tok", time.Now().Add(50*time.Millisecond))
	if _, ok := c.Get(0); !ok {
		t.Error("immediate Get should hit")
	}
	time.Sleep(60 * time.Millisecond)
	if _, ok := c.Get(0); ok {
		t.Error("expired entry should miss")
	}
}

func TestTokenCache_RespectsSkew(t *testing.T) {
	c := &tokenCache{}
	c.Set("tok", time.Now().Add(100*time.Millisecond))
	if _, ok := c.Get(200 * time.Millisecond); ok {
		t.Error("skew window should treat token as expired")
	}
}

func TestTokenCache_Invalidate(t *testing.T) {
	c := &tokenCache{}
	c.Set("tok", time.Now().Add(time.Hour))
	c.Invalidate()
	if _, ok := c.Get(0); ok {
		t.Error("invalidate should clear")
	}
}

// --- singleFlight --------------------------------------------------------

func TestSingleFlight_CollapsesConcurrentCalls(t *testing.T) {
	var sf singleFlight
	var underlyingCount int32
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = sf.Do(func() (string, error) {
				atomic.AddInt32(&underlyingCount, 1)
				time.Sleep(50 * time.Millisecond)
				return "shared", nil
			})
		}()
	}
	wg.Wait()
	if got := atomic.LoadInt32(&underlyingCount); got != 1 {
		t.Errorf("underlying fn called %d times; want 1", got)
	}
}

func TestSingleFlight_ReusableAfterRun(t *testing.T) {
	var sf singleFlight
	a, _ := sf.Do(func() (string, error) { return "a", nil })
	b, _ := sf.Do(func() (string, error) { return "b", nil })
	if a != "a" || b != "b" {
		t.Errorf("got (%q, %q)", a, b)
	}
}

// --- OAuth2 provider end-to-end -----------------------------------------

func newIdP(t *testing.T, response string, status int, hits *int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			atomic.AddInt32(hits, 1)
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "grant_type=client_credentials") {
			t.Errorf("body missing grant_type: %s", body)
		}
		if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Errorf("Content-Type = %q", r.Header.Get("Content-Type"))
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, response)
	}))
}

func TestOAuth2_AcquiresAndCachesToken(t *testing.T) {
	var hits int32
	idp := newIdP(t, `{"access_token":"abc","expires_in":3600}`, http.StatusOK, &hits)
	defer idp.Close()

	p, err := NewOAuth2ClientCredentialsProvider(OAuth2Options{
		Name: "p", TokenEndpoint: idp.URL,
		ClientID: "c", ClientSecret: "s",
		Cache: TokenCacheConfig{Source: SourceTTL, TTL: time.Minute},
	})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	for i := 0; i < 3; i++ {
		req, _ := http.NewRequestWithContext(context.Background(), "GET", "https://r.example.com", nil)
		if err := p.Apply(req); err != nil {
			t.Fatalf("apply: %v", err)
		}
		if req.Header.Get("Authorization") != "Bearer abc" {
			t.Errorf("Authorization = %q", req.Header.Get("Authorization"))
		}
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("token endpoint hit %d times; want 1 (cache active)", got)
	}
}

func TestOAuth2_ResponseFieldSource(t *testing.T) {
	idp := newIdP(t, `{"access_token":"x","expires_in":100}`, http.StatusOK, nil)
	defer idp.Close()
	p, _ := NewOAuth2ClientCredentialsProvider(OAuth2Options{
		Name: "p", TokenEndpoint: idp.URL, ClientID: "c", ClientSecret: "s",
		Cache: TokenCacheConfig{Source: SourceResponseField, JSONPath: "$.expires_in", Unit: UnitSeconds},
	})
	req, _ := http.NewRequestWithContext(context.Background(), "GET", "https://r.example.com", nil)
	if err := p.Apply(req); err != nil {
		t.Fatalf("apply: %v", err)
	}
	// Check that the cache was set with a reasonable expiry
	if _, ok := p.cache.Get(50 * time.Second); !ok {
		t.Error("token should still be valid within 50s skew")
	}
	if _, ok := p.cache.Get(120 * time.Second); ok {
		t.Error("token should be considered expired with 120s skew (>= TTL)")
	}
}

func TestOAuth2_JWTExpSource(t *testing.T) {
	exp := time.Now().Add(2 * time.Hour).Unix()
	tok := makeJWT(map[string]any{"exp": exp})
	idp := newIdP(t, `{"access_token":"`+tok+`"}`, http.StatusOK, nil)
	defer idp.Close()
	p, _ := NewOAuth2ClientCredentialsProvider(OAuth2Options{
		Name: "p", TokenEndpoint: idp.URL, ClientID: "c", ClientSecret: "s",
		Cache: TokenCacheConfig{Source: SourceJWTExp},
	})
	req, _ := http.NewRequestWithContext(context.Background(), "GET", "https://r.example.com", nil)
	if err := p.Apply(req); err != nil {
		t.Fatalf("apply: %v", err)
	}
}

func TestOAuth2_SingleFlight_CollapsesConcurrent(t *testing.T) {
	var hits int32
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		// Slow response to maximize concurrent waiting
		time.Sleep(50 * time.Millisecond)
		_, _ = io.WriteString(w, `{"access_token":"y","expires_in":3600}`)
	}))
	defer idp.Close()
	p, _ := NewOAuth2ClientCredentialsProvider(OAuth2Options{
		Name: "p", TokenEndpoint: idp.URL, ClientID: "c", ClientSecret: "s",
		Cache: TokenCacheConfig{Source: SourceTTL, TTL: time.Hour, SingleFlight: true},
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
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("single-flight failed: token endpoint hit %d times; want 1", got)
	}
}

func TestOAuth2_Invalidate_ForcesReacquire(t *testing.T) {
	var hits int32
	idp := newIdP(t, `{"access_token":"z","expires_in":3600}`, http.StatusOK, &hits)
	defer idp.Close()
	p, _ := NewOAuth2ClientCredentialsProvider(OAuth2Options{
		Name: "p", TokenEndpoint: idp.URL, ClientID: "c", ClientSecret: "s",
		Cache: TokenCacheConfig{Source: SourceTTL, TTL: time.Hour},
	})
	req, _ := http.NewRequestWithContext(context.Background(), "GET", "https://r.example.com", nil)
	_ = p.Apply(req)
	p.Invalidate()
	_ = p.Apply(req)
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Errorf("invalidate should force second acquire; hits = %d", got)
	}
}

func TestOAuth2_TokenEndpointFailure(t *testing.T) {
	idp := newIdP(t, `{"error":"invalid_client"}`, http.StatusUnauthorized, nil)
	defer idp.Close()
	p, _ := NewOAuth2ClientCredentialsProvider(OAuth2Options{
		Name: "p", TokenEndpoint: idp.URL, ClientID: "c", ClientSecret: "wrong",
		Cache: TokenCacheConfig{Source: SourceTTL, TTL: time.Hour},
	})
	req, _ := http.NewRequestWithContext(context.Background(), "GET", "https://r.example.com", nil)
	if err := p.Apply(req); err == nil {
		t.Error("expected error from token endpoint")
	}
}

func TestOAuth2_RevocableInterface(t *testing.T) {
	idp := newIdP(t, `{"access_token":"x","expires_in":3600}`, http.StatusOK, nil)
	defer idp.Close()
	p, _ := NewOAuth2ClientCredentialsProvider(OAuth2Options{
		Name: "p", TokenEndpoint: idp.URL, ClientID: "c", ClientSecret: "s",
		Cache: TokenCacheConfig{Source: SourceTTL, TTL: time.Hour},
	})
	var _ RevocableProvider = p
}

func TestOAuth2_RequiresFields(t *testing.T) {
	cases := []OAuth2Options{
		{Name: "p", ClientID: "c", ClientSecret: "s"},      // missing token endpoint
		{Name: "p", TokenEndpoint: "x", ClientSecret: "s"}, // missing clientID
		{Name: "p", TokenEndpoint: "x", ClientID: "c"},     // missing clientSecret
	}
	for _, opts := range cases {
		if _, err := NewOAuth2ClientCredentialsProvider(opts); err == nil {
			t.Errorf("expected error for opts %+v", opts)
		}
	}
}
