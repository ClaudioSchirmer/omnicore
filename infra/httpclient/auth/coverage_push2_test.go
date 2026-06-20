package auth

import (
	"context"
	"net/http"
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
