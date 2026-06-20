package auth

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// applyForcesAcquire drives a fresh token acquisition through Apply and
// returns the resulting error (nil on success).
func applyForcesAcquire(t *testing.T, p *OAuth2ClientCredentialsProvider) error {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), "GET", "https://r.example.com", nil)
	return p.Apply(req)
}

func newOAuth2(t *testing.T, idpURL string, cache TokenCacheConfig) *OAuth2ClientCredentialsProvider {
	t.Helper()
	p, err := NewOAuth2ClientCredentialsProvider(OAuth2Options{
		Name: "p", TokenEndpoint: idpURL, ClientID: "c", ClientSecret: "s",
		Cache: cache,
	})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	return p
}

func TestOAuth2_MalformedJSONResponse(t *testing.T) {
	idp := newIdP(t, `{not json`, http.StatusOK, nil)
	defer idp.Close()
	p := newOAuth2(t, idp.URL, TokenCacheConfig{Source: SourceTTL, TTL: time.Minute})
	if err := applyForcesAcquire(t, p); err == nil {
		t.Fatal("expected parse error on malformed token response")
	}
}

func TestOAuth2_MissingAccessToken(t *testing.T) {
	idp := newIdP(t, `{"token_type":"bearer"}`, http.StatusOK, nil)
	defer idp.Close()
	p := newOAuth2(t, idp.URL, TokenCacheConfig{Source: SourceTTL, TTL: time.Minute})
	if err := applyForcesAcquire(t, p); err == nil {
		t.Fatal("expected missing access_token error")
	}
}

func TestOAuth2_AccessTokenNotString(t *testing.T) {
	idp := newIdP(t, `{"access_token":12345}`, http.StatusOK, nil)
	defer idp.Close()
	p := newOAuth2(t, idp.URL, TokenCacheConfig{Source: SourceTTL, TTL: time.Minute})
	if err := applyForcesAcquire(t, p); err == nil {
		t.Fatal("expected non-string access_token error")
	}
}

func TestOAuth2_JWTExpSourceWithNonJWTToken(t *testing.T) {
	// access_token is not a JWT → decodeJWTExp fails → computeExpiry error.
	idp := newIdP(t, `{"access_token":"opaque-token"}`, http.StatusOK, nil)
	defer idp.Close()
	p := newOAuth2(t, idp.URL, TokenCacheConfig{Source: SourceJWTExp})
	if err := applyForcesAcquire(t, p); err == nil {
		t.Fatal("expected jwt-exp decode error for an opaque token")
	}
}

func TestOAuth2_ResponseFieldSourceMissingPath(t *testing.T) {
	idp := newIdP(t, `{"access_token":"x"}`, http.StatusOK, nil)
	defer idp.Close()
	p := newOAuth2(t, idp.URL, TokenCacheConfig{
		Source: SourceResponseField, JSONPath: "$.expires_in", Unit: UnitSeconds,
	})
	if err := applyForcesAcquire(t, p); err == nil {
		t.Fatal("expected response-field error when the JSONPath key is absent")
	}
}

// --- credentials-exchange acquireToken error branches -----------------------

func TestCredentialsExchange_MalformedJSONResponse(t *testing.T) {
	idp := newCredsIdP(t, `{not json`, nil, nil, nil)
	defer idp.Close()
	p, _ := NewCredentialsExchangeProvider(CredentialsExchangeOptions{
		Name: "p", TokenEndpoint: idp.URL, RequestCodec: "json",
		RequestFields: map[string]string{"a": "1"}, ResponseTokenPath: "$.access_token",
		Cache: TokenCacheConfig{Source: SourceTTL, TTL: time.Hour},
	})
	req, _ := http.NewRequestWithContext(context.Background(), "GET", "https://r.example.com", nil)
	if err := p.Apply(req); err == nil {
		t.Fatal("expected parse error on malformed credentials response")
	}
}

func TestCredentialsExchange_TokenNotString(t *testing.T) {
	idp := newCredsIdP(t, `{"access_token":42}`, nil, nil, nil)
	defer idp.Close()
	p, _ := NewCredentialsExchangeProvider(CredentialsExchangeOptions{
		Name: "p", TokenEndpoint: idp.URL, RequestCodec: "json",
		RequestFields: map[string]string{"a": "1"}, ResponseTokenPath: "$.access_token",
		Cache: TokenCacheConfig{Source: SourceTTL, TTL: time.Hour},
	})
	req, _ := http.NewRequestWithContext(context.Background(), "GET", "https://r.example.com", nil)
	if err := p.Apply(req); err == nil {
		t.Fatal("expected non-string token error")
	}
}

// --- walkJSONPath: traversal through a non-object -----------------------------

func TestWalkJSONPath_NotAnObjectMidPath(t *testing.T) {
	root := map[string]any{"a": 5} // "a" is a scalar, not an object
	if _, err := walkJSONPath("$.a.b", root); err == nil {
		t.Fatal("expected error when a path segment is not an object")
	}
}

// --- transport-failure harness -------------------------------------------------

// rtErr is a RoundTripper that always fails the transport.
type rtErr struct{}

func (rtErr) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("dial tcp: simulated transport failure")
}

// errBody is a ReadCloser whose Read always errors, to drive the
// io.ReadAll branch of acquireToken.
type errBody struct{}

func (errBody) Read([]byte) (int, error) { return 0, fmt.Errorf("simulated body read failure") }
func (errBody) Close() error             { return nil }

// rtErrBody is a RoundTripper that returns a 200 response whose body
// errors on read.
type rtErrBody struct{}

func (rtErrBody) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       errBody{},
	}, nil
}

// --- oauth2 acquireToken: scope + audience form fields -------------------------

func TestOAuth2_AcquireToken_ScopeAndAudience(t *testing.T) {
	var captured string
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured = string(body)
		_, _ = io.WriteString(w, `{"access_token":"t"}`)
	}))
	defer idp.Close()
	p, err := NewOAuth2ClientCredentialsProvider(OAuth2Options{
		Name: "p", TokenEndpoint: idp.URL, ClientID: "c", ClientSecret: "s",
		Scope: []string{"read", "write"}, Audience: "https://api.example.com",
		Cache: TokenCacheConfig{Source: SourceTTL, TTL: time.Hour},
	})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	req, _ := http.NewRequestWithContext(context.Background(), "GET", "https://r.example.com", nil)
	if err := p.Apply(req); err != nil {
		t.Fatalf("apply: %v", err)
	}
	form, _ := url.ParseQuery(captured)
	if form.Get("scope") != "read write" {
		t.Errorf("scope = %q; want \"read write\"", form.Get("scope"))
	}
	if form.Get("audience") != "https://api.example.com" {
		t.Errorf("audience = %q", form.Get("audience"))
	}
}

// --- oauth2 acquireToken: transport + body-read failure ------------------------

func TestOAuth2_AcquireToken_TransportError(t *testing.T) {
	p, err := NewOAuth2ClientCredentialsProvider(OAuth2Options{
		Name: "p", TokenEndpoint: "https://idp.invalid/token", ClientID: "c", ClientSecret: "s",
		Cache:      TokenCacheConfig{Source: SourceTTL, TTL: time.Hour},
		HttpClient: &http.Client{Transport: rtErr{}},
	})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	req, _ := http.NewRequestWithContext(context.Background(), "GET", "https://r.example.com", nil)
	if err := p.Apply(req); err == nil || !strings.Contains(err.Error(), "token endpoint") {
		t.Fatalf("expected transport error; got %v", err)
	}
}

func TestOAuth2_AcquireToken_BodyReadError(t *testing.T) {
	p, err := NewOAuth2ClientCredentialsProvider(OAuth2Options{
		Name: "p", TokenEndpoint: "https://idp.invalid/token", ClientID: "c", ClientSecret: "s",
		Cache:      TokenCacheConfig{Source: SourceTTL, TTL: time.Hour},
		HttpClient: &http.Client{Transport: rtErrBody{}},
	})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	req, _ := http.NewRequestWithContext(context.Background(), "GET", "https://r.example.com", nil)
	if err := p.Apply(req); err == nil || !strings.Contains(err.Error(), "read token response") {
		t.Fatalf("expected body-read error; got %v", err)
	}
}

// --- oauth2 acquireToken: malformed token endpoint URL -------------------------

func TestOAuth2_AcquireToken_BuildRequestError(t *testing.T) {
	p, err := NewOAuth2ClientCredentialsProvider(OAuth2Options{
		Name: "p", TokenEndpoint: "http://exa\x7fmple.com/token", ClientID: "c", ClientSecret: "s",
		Cache: TokenCacheConfig{Source: SourceTTL, TTL: time.Hour},
	})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	req, _ := http.NewRequestWithContext(context.Background(), "GET", "https://r.example.com", nil)
	if err := p.Apply(req); err == nil || !strings.Contains(err.Error(), "build token request") {
		t.Fatalf("expected build-request error; got %v", err)
	}
}

// --- oauth2 computeExpiry: no source configured --------------------------------

func TestOAuth2_ComputeExpiry_NoSource(t *testing.T) {
	p, err := NewOAuth2ClientCredentialsProvider(OAuth2Options{
		Name: "p", TokenEndpoint: "https://x", ClientID: "c", ClientSecret: "s",
		Cache: TokenCacheConfig{Source: SourceUnknown},
	})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	if _, err := p.computeExpiry("tok", nil); err == nil {
		t.Fatal("expected error for unconfigured token cache source")
	}
}

// --- credentials-exchange acquireToken: transport + body-read failure ----------

func TestCredentialsExchange_AcquireToken_TransportError(t *testing.T) {
	p, err := NewCredentialsExchangeProvider(CredentialsExchangeOptions{
		Name: "p", TokenEndpoint: "https://idp.invalid/token",
		RequestCodec: "json", RequestFields: map[string]string{"a": "1"},
		ResponseTokenPath: "$.access_token",
		Cache:             TokenCacheConfig{Source: SourceTTL, TTL: time.Hour},
		HttpClient:        &http.Client{Transport: rtErr{}},
	})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	req, _ := http.NewRequestWithContext(context.Background(), "GET", "https://r.example.com", nil)
	if err := p.Apply(req); err == nil || !strings.Contains(err.Error(), "token endpoint") {
		t.Fatalf("expected transport error; got %v", err)
	}
}

func TestCredentialsExchange_AcquireToken_BodyReadError(t *testing.T) {
	p, err := NewCredentialsExchangeProvider(CredentialsExchangeOptions{
		Name: "p", TokenEndpoint: "https://idp.invalid/token",
		RequestCodec: "json", RequestFields: map[string]string{"a": "1"},
		ResponseTokenPath: "$.access_token",
		Cache:             TokenCacheConfig{Source: SourceTTL, TTL: time.Hour},
		HttpClient:        &http.Client{Transport: rtErrBody{}},
	})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	req, _ := http.NewRequestWithContext(context.Background(), "GET", "https://r.example.com", nil)
	if err := p.Apply(req); err == nil || !strings.Contains(err.Error(), "read token response") {
		t.Fatalf("expected body-read error; got %v", err)
	}
}

// --- credentials-exchange acquireToken: malformed token endpoint URL -----------

func TestCredentialsExchange_AcquireToken_BuildRequestError(t *testing.T) {
	// A control character in the endpoint makes http.NewRequestWithContext
	// fail at URL parse time, before any dial.
	p, err := NewCredentialsExchangeProvider(CredentialsExchangeOptions{
		Name: "p", TokenEndpoint: "http://exa\x7fmple.com/token",
		RequestCodec: "json", RequestFields: map[string]string{"a": "1"},
		ResponseTokenPath: "$.access_token",
		Cache:             TokenCacheConfig{Source: SourceTTL, TTL: time.Hour},
	})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	req, _ := http.NewRequestWithContext(context.Background(), "GET", "https://r.example.com", nil)
	if err := p.Apply(req); err == nil || !strings.Contains(err.Error(), "build token request") {
		t.Fatalf("expected build-request error; got %v", err)
	}
}

// --- credentials-exchange acquireToken: computeExpiry propagation ---------------

func TestCredentialsExchange_AcquireToken_ComputeExpiryError(t *testing.T) {
	// Opaque (non-JWT) token + jwt-exp source → computeExpiry fails inside
	// acquireToken after a successful token fetch.
	idp := newCredsIdP(t, `{"access_token":"opaque-not-a-jwt"}`, nil, nil, nil)
	defer idp.Close()
	p, err := NewCredentialsExchangeProvider(CredentialsExchangeOptions{
		Name: "p", TokenEndpoint: idp.URL,
		RequestCodec: "json", RequestFields: map[string]string{"a": "1"},
		ResponseTokenPath: "$.access_token",
		Cache:             TokenCacheConfig{Source: SourceJWTExp},
	})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	req, _ := http.NewRequestWithContext(context.Background(), "GET", "https://r.example.com", nil)
	if err := p.Apply(req); err == nil || !strings.Contains(err.Error(), "jwt-exp") {
		t.Fatalf("expected computeExpiry jwt-exp error; got %v", err)
	}
}

// --- credentials-exchange buildBody: unsupported codec -------------------------

func TestCredentialsExchange_BuildBody_UnsupportedCodec(t *testing.T) {
	// Construct directly with an unsupported codec (the constructor would
	// normally reject it) to exercise the buildBody default branch and its
	// propagation through acquireToken.
	p := &CredentialsExchangeProvider{name: "x", requestCodec: "xml"}
	if _, _, err := p.buildBody(map[string]string{"a": "1"}); err == nil {
		t.Fatal("expected unsupported-codec error from buildBody")
	}
	cache := &tokenCache{}
	if _, err := p.acquireToken(context.Background(), cache, map[string]string{"a": "1"}); err == nil {
		t.Fatal("expected acquireToken to propagate the buildBody error")
	}
}
