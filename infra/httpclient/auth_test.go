package httpclient

import (
	"errors"
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

// --- validation ----------------------------------------------------------

func TestValidate_UnknownProviderType(t *testing.T) {
	c := &Config{
		AuthProviders: map[string]AuthProviderConfig{
			"x": {Type: "magic"},
		},
	}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "magic") {
		t.Errorf("expected unknown type error; got %v", err)
	}
}

func TestValidate_ServiceReferencesUnknownProvider(t *testing.T) {
	c := &Config{
		Services: map[string]ServiceConfig{
			"s": {
				BaseURL: "https://s.example.com",
				Auth:    &ServiceAuthConfig{Provider: "missing"},
				Endpoints: map[string]EndpointConfig{
					"e": {Method: "GET", Path: "/x"},
				},
			},
		},
	}
	c.applyDefaults()
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Errorf("expected missing provider reference error; got %v", err)
	}
}

// --- E2E -----------------------------------------------------------------

func newAuthClient(t *testing.T, server *httptest.Server, providers map[string]AuthProviderConfig, serviceAuth *ServiceAuthConfig) *HttpClient {
	t.Helper()
	cfg := &Config{
		AuthProviders: providers,
		Services: map[string]ServiceConfig{
			"svc": {
				BaseURL: server.URL,
				Auth:    serviceAuth,
				Endpoints: map[string]EndpointConfig{
					"call": {Method: "GET", Path: "/x"},
				},
			},
		},
	}
	c, err := New(cfg, WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestE2E_BearerStatic(t *testing.T) {
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	c := newAuthClient(t, srv,
		map[string]AuthProviderConfig{"b": {Type: "bearer-static", Token: "tok"}},
		&ServiceAuthConfig{Provider: "b"},
	)
	type req struct{}
	_, _ = Call[req, struct{}](configuration.NewAppContextWithRandomID(configuration.LangPTBR), c, "svc", "call", req{})
	if seen != "Bearer tok" {
		t.Errorf("Authorization = %q", seen)
	}
}

func TestE2E_HeaderStatic(t *testing.T) {
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("X-API-Key")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	c := newAuthClient(t, srv,
		map[string]AuthProviderConfig{"k": {Type: "header-static", Attach: &AttachConfig{As: "header", Name: "X-API-Key", Value: "secret"}}},
		&ServiceAuthConfig{Provider: "k"},
	)
	type req struct{}
	_, _ = Call[req, struct{}](configuration.NewAppContextWithRandomID(configuration.LangPTBR), c, "svc", "call", req{})
	if seen != "secret" {
		t.Errorf("X-API-Key = %q", seen)
	}
}

func TestE2E_Basic(t *testing.T) {
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	c := newAuthClient(t, srv,
		map[string]AuthProviderConfig{"b": {Type: "basic", Username: "alice", Password: "wonder"}},
		&ServiceAuthConfig{Provider: "b"},
	)
	type req struct{}
	_, _ = Call[req, struct{}](configuration.NewAppContextWithRandomID(configuration.LangPTBR), c, "svc", "call", req{})
	if seen != "Basic YWxpY2U6d29uZGVy" {
		t.Errorf("Authorization = %q", seen)
	}
}

func TestE2E_NoneProvider(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("none provider should not set Authorization")
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	c := newAuthClient(t, srv,
		map[string]AuthProviderConfig{"n": {Type: "none"}},
		&ServiceAuthConfig{Provider: "n"},
	)
	type req struct{}
	_, _ = Call[req, struct{}](configuration.NewAppContextWithRandomID(configuration.LangPTBR), c, "svc", "call", req{})
}

func TestE2E_WithAuthOverride(t *testing.T) {
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	c := newAuthClient(t, srv,
		map[string]AuthProviderConfig{
			"primary":   {Type: "bearer-static", Token: "primary-tok"},
			"secondary": {Type: "bearer-static", Token: "secondary-tok"},
		},
		&ServiceAuthConfig{Provider: "primary"},
	)
	type req struct{}
	_, _ = Call[req, struct{}](configuration.NewAppContextWithRandomID(configuration.LangPTBR), c, "svc", "call", req{}, WithConfig(CallConfig{AuthProvider: "secondary"}))
	if seen != "Bearer secondary-tok" {
		t.Errorf("override Authorization = %q", seen)
	}
}

func TestE2E_WithAuthOverride_UnknownErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("server should not be called when override fails")
	}))
	defer srv.Close()
	c := newAuthClient(t, srv,
		map[string]AuthProviderConfig{"primary": {Type: "bearer-static", Token: "t"}},
		&ServiceAuthConfig{Provider: "primary"},
	)
	type req struct{}
	_, err := Call[req, struct{}](configuration.NewAppContextWithRandomID(configuration.LangPTBR), c, "svc", "call", req{}, WithConfig(CallConfig{AuthProvider: "nope"}))
	if !errors.Is(err, ErrTokenAcquire) {
		t.Errorf("expected ErrTokenAcquire; got %v", err)
	}
}

func TestE2E_ForwardBearer(t *testing.T) {
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	c := newAuthClient(t, srv,
		map[string]AuthProviderConfig{"fb": {Type: "forward-bearer"}},
		&ServiceAuthConfig{Provider: "fb"},
	)
	type req struct{}
	ctx := configuration.NewAppContextWithRandomID(configuration.LangPTBR)
	ctx.SetBearerToken("inbound-jwt")
	_, _ = Call[req, struct{}](ctx, c, "svc", "call", req{})
	if seen != "Bearer inbound-jwt" {
		t.Errorf("Authorization = %q", seen)
	}
}

func TestE2E_ForwardBearer_NoToken_ErrTokenAcquire(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("server should not be called when token is missing")
	}))
	defer srv.Close()
	c := newAuthClient(t, srv,
		map[string]AuthProviderConfig{"fb": {Type: "forward-bearer"}},
		&ServiceAuthConfig{Provider: "fb"},
	)
	type req struct{}
	ctx := configuration.NewAppContextWithRandomID(configuration.LangPTBR)
	// No SetBearerToken → empty
	_, err := Call[req, struct{}](ctx, c, "svc", "call", req{})
	if !errors.Is(err, ErrTokenAcquire) {
		t.Errorf("expected ErrTokenAcquire; got %v", err)
	}
}

// --- OAuth2 E2E ---------------------------------------------------------

func TestE2E_OAuth2ClientCredentials(t *testing.T) {
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"oauth-tok","expires_in":3600}`)
	}))
	defer idp.Close()
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	c := newAuthClient(t, srv,
		map[string]AuthProviderConfig{"oa": {
			Type:          "oauth2-client-credentials",
			TokenEndpoint: idp.URL,
			ClientID:      "id",
			ClientSecret:  "secret",
			TokenCache:    &TokenCacheConfig{Source: "ttl", TTL: Duration(time.Hour)},
		}},
		&ServiceAuthConfig{Provider: "oa"},
	)
	type req struct{}
	_, _ = Call[req, struct{}](configuration.NewAppContextWithRandomID(configuration.LangPTBR), c, "svc", "call", req{})
	if seen != "Bearer oauth-tok" {
		t.Errorf("Authorization = %q", seen)
	}
}

func TestE2E_OAuth2RevocationOnUnauthorized(t *testing.T) {
	var idpHits int32
	tokens := []string{"first-tok", "second-tok"}
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := atomic.AddInt32(&idpHits, 1)
		_, _ = io.WriteString(w, `{"access_token":"`+tokens[i-1]+`","expires_in":3600}`)
	}))
	defer idp.Close()

	var resourceCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&resourceCalls, 1)
		got := r.Header.Get("Authorization")
		if n == 1 && got != "Bearer first-tok" {
			t.Errorf("first call Authorization = %q", got)
		}
		if n == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if got != "Bearer second-tok" {
			t.Errorf("second call Authorization = %q (expected fresh token)", got)
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()

	c := newAuthClient(t, srv,
		map[string]AuthProviderConfig{"oa": {
			Type:                     "oauth2-client-credentials",
			TokenEndpoint:            idp.URL,
			ClientID:                 "id",
			ClientSecret:             "secret",
			TokenCache:               &TokenCacheConfig{Source: "ttl", TTL: Duration(time.Hour)},
			RevocationOnUnauthorized: true,
		}},
		&ServiceAuthConfig{Provider: "oa"},
	)
	type req struct{}
	_, err := Call[req, struct{}](configuration.NewAppContextWithRandomID(configuration.LangPTBR), c, "svc", "call", req{})
	if err != nil {
		t.Errorf("Call: %v", err)
	}
	if atomic.LoadInt32(&idpHits) != 2 {
		t.Errorf("token endpoint hits = %d, want 2 (invalidate + re-acquire)", idpHits)
	}
	if atomic.LoadInt32(&resourceCalls) != 2 {
		t.Errorf("resource calls = %d, want 2 (first 401, retry 200)", resourceCalls)
	}
}

func TestE2E_OAuth2_NoRevocation_PropagatesAuthorized401(t *testing.T) {
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600}`)
	}))
	defer idp.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	c := newAuthClient(t, srv,
		map[string]AuthProviderConfig{"oa": {
			Type: "oauth2-client-credentials", TokenEndpoint: idp.URL,
			ClientID: "id", ClientSecret: "secret",
			TokenCache: &TokenCacheConfig{Source: "ttl", TTL: Duration(time.Hour)},
			// RevocationOnUnauthorized: false (default)
		}},
		&ServiceAuthConfig{Provider: "oa"},
	)
	type req struct{}
	_, err := Call[req, struct{}](configuration.NewAppContextWithRandomID(configuration.LangPTBR), c, "svc", "call", req{})
	var he *HttpError
	if !errors.As(err, &he) || he.Status != 401 {
		t.Errorf("expected 401 propagated; got %v", err)
	}
}

// --- credentials-exchange tokenCache error message (Bug 21 regression) ---

// validateTokenCacheConfig is shared between oauth2-client-credentials and
// credentials-exchange. The "tokenCache is missing" message must NOT name
// oauth2-client-credentials when it is a credentials-exchange provider that
// triggered the failure — pre-fix the error claimed the wrong provider type.
func TestValidate_CredentialsExchange_TokenCacheMessageNotMisleading(t *testing.T) {
	c := &Config{
		AuthProviders: map[string]AuthProviderConfig{
			"ce": {
				Type:              "credentials-exchange",
				TokenEndpoint:     "https://example.com/token",
				RequestFields:     map[string]string{"grant_type": "password"},
				ResponseTokenPath: "$.access_token",
				// TokenCache deliberately omitted to trigger the validator.
			},
		},
	}
	err := c.Validate()
	if err == nil {
		t.Fatal("expected validation error for missing tokenCache")
	}
	msg := err.Error()
	if !strings.Contains(msg, "authProviders.ce.tokenCache") {
		t.Errorf("error should reference the provider's tokenCache path; got:\n%s", msg)
	}
	if strings.Contains(msg, "oauth2-client-credentials") {
		t.Errorf("error must NOT mention oauth2-client-credentials for a credentials-exchange provider; got:\n%s", msg)
	}
}

// Same expectation, mirror case: an oauth2-client-credentials provider
// without tokenCache still produces the (now generic) error and the path
// in the message identifies the provider — no functional regression on the
// existing oauth2 surface.
func TestValidate_OAuth2ClientCredentials_TokenCacheStillRejected(t *testing.T) {
	c := &Config{
		AuthProviders: map[string]AuthProviderConfig{
			"oa": {
				Type:          "oauth2-client-credentials",
				TokenEndpoint: "https://example.com/token",
				ClientID:      "id",
				ClientSecret:  "secret",
				// TokenCache deliberately omitted.
			},
		},
	}
	err := c.Validate()
	if err == nil {
		t.Fatal("expected validation error for missing tokenCache")
	}
	if !strings.Contains(err.Error(), "authProviders.oa.tokenCache") {
		t.Errorf("error should reference the provider's tokenCache path; got:\n%s", err)
	}
}

func TestE2E_NoAuth_NoMiddleware(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("no auth: block should mean no Authorization header")
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	c := newAuthClient(t, srv, nil, nil)
	type req struct{}
	_, err := Call[req, struct{}](configuration.NewAppContextWithRandomID(configuration.LangPTBR), c, "svc", "call", req{})
	if err != nil {
		t.Errorf("Call: %v", err)
	}
}
