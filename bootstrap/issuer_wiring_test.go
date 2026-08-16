package bootstrap

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/application/translation"
	"github.com/ClaudioSchirmer/omnicore/web/authcore"
	"github.com/ClaudioSchirmer/omnicore/web/openapi"
	"log/slog"
)

func realRSAPrivatePEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa keygen: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal pkcs8: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

func issuerDeps(t *testing.T, jwks *IssuerJWKSConfig, refreshTTL int) Deps {
	t.Helper()
	return Deps{
		Config: &Config{
			Service: "test-service",
			Auth: AuthConfig{
				Mode: AuthModeDisabled,
				Issuer: &IssuerConfig{
					Enabled:                true,
					SelfURL:                "http://auth",
					Audience:               []string{"users-api"},
					TokenTTLSeconds:        900,
					MaxTokenTTLSeconds:     3600,
					RefreshTokenTTLSeconds: refreshTTL,
					Keys: []IssuerKeyConfig{
						{KID: "k1", Algorithm: "RS256", State: issuerKeyStateCurrent, PrivateKeyPEM: realRSAPrivatePEM(t)},
					},
					JWKS: jwks,
				},
			},
		},
		Logger:   slog.New(slog.NewJSONHandler(io.Discard, nil)),
		Pipeline: pipeline.New(translation.Default()),
	}
}

// --- buildIssuer -------------------------------------------------------------

func TestBuildIssuer_PopulatesDeps(t *testing.T) {
	d := issuerDeps(t, nil, 0)
	if err := buildIssuer(&d, Wiring{}); err != nil {
		t.Fatalf("buildIssuer: %v", err)
	}
	if d.Issuer == nil {
		t.Fatal("Deps.Issuer should be populated")
	}
}

func TestBuildIssuer_NilWhenNotEnabled(t *testing.T) {
	d := silentDeps()
	if err := buildIssuer(&d, Wiring{}); err != nil {
		t.Fatalf("buildIssuer: %v", err)
	}
	if d.Issuer != nil {
		t.Fatal("Deps.Issuer should stay nil when auth.issuer is absent")
	}
}

func TestBuildIssuer_Idempotent(t *testing.T) {
	d := issuerDeps(t, nil, 0)
	if err := buildIssuer(&d, Wiring{}); err != nil {
		t.Fatalf("buildIssuer (1st): %v", err)
	}
	first := d.Issuer
	// Corrupt the config so a SECOND real construction would fail — proves
	// the second call is a genuine no-op, not just "happens to succeed
	// again".
	d.Config.Auth.Issuer.Keys[0].PrivateKeyPEM = "not a pem"
	if err := buildIssuer(&d, Wiring{}); err != nil {
		t.Fatalf("buildIssuer (2nd, should be no-op): %v", err)
	}
	if d.Issuer != first {
		t.Fatal("second buildIssuer call should not reconstruct deps.Issuer")
	}
}

func TestBuildIssuer_RefreshTokenRequiresStore(t *testing.T) {
	d := issuerDeps(t, nil, 3600)
	err := buildIssuer(&d, Wiring{})
	if err == nil || !strings.Contains(err.Error(), "requires Wiring.RefreshTokenStore") {
		t.Fatalf("expected RefreshTokenStore-required boot error, got %v", err)
	}
}

// stubRefreshStore is a minimal authcore.RefreshTokenStore — buildIssuer
// only needs a non-nil value to satisfy the boot guard; the round-trip
// behavior is already covered by web/authcore's own tests.
type stubRefreshStore struct{}

func (stubRefreshStore) Save(context.Context, authcore.RefreshTokenRecord) error { return nil }
func (stubRefreshStore) Lookup(context.Context, string) (authcore.RefreshTokenRecord, error) {
	return authcore.RefreshTokenRecord{}, authcore.ErrRefreshTokenNotFound
}
func (stubRefreshStore) MarkUsed(context.Context, string) error     { return nil }
func (stubRefreshStore) RevokeFamily(context.Context, string) error { return nil }

func TestBuildIssuer_MapsNextAndPreviousKeyStates(t *testing.T) {
	d := issuerDeps(t, nil, 0)
	d.Config.Auth.Issuer.Keys = append(d.Config.Auth.Issuer.Keys,
		IssuerKeyConfig{KID: "k0", Algorithm: "RS256", State: issuerKeyStatePrevious, PrivateKeyPEM: realRSAPrivatePEM(t)},
		IssuerKeyConfig{KID: "k2", Algorithm: "RS256", State: issuerKeyStateNext, PrivateKeyPEM: realRSAPrivatePEM(t)},
	)
	if err := buildIssuer(&d, Wiring{}); err != nil {
		t.Fatalf("buildIssuer: %v", err)
	}
	doc, err := d.Issuer.JWKS()
	if err != nil {
		t.Fatalf("JWKS: %v", err)
	}
	for _, kid := range []string{"k0", "k1", "k2"} {
		if !strings.Contains(string(doc), kid) {
			t.Errorf("JWKS missing kid %q (state mapping dropped a key): %s", kid, doc)
		}
	}
}

// TestBuildIssuer_PropagatesAuthcoreError covers the case where
// AuthConfig.validate() (schema-only: non-empty string) passes but the
// PEM is not real key material — authcore.NewIssuer is the layer that
// actually parses it, and its error must propagate as a boot failure.
func TestBuildIssuer_PropagatesAuthcoreError(t *testing.T) {
	d := issuerDeps(t, nil, 0)
	d.Config.Auth.Issuer.Keys[0].PrivateKeyPEM = "not a real PEM"
	err := buildIssuer(&d, Wiring{})
	if err == nil || !strings.Contains(err.Error(), "build auth.issuer") {
		t.Fatalf("expected wrapped authcore.NewIssuer error, got %v", err)
	}
}

func TestBuildIssuer_RefreshTokenWithStoreSucceeds(t *testing.T) {
	d := issuerDeps(t, nil, 3600)
	if err := buildIssuer(&d, Wiring{RefreshTokenStore: stubRefreshStore{}}); err != nil {
		t.Fatalf("buildIssuer: %v", err)
	}
	if d.Issuer == nil {
		t.Fatal("Deps.Issuer should be populated")
	}
}

// --- JWKS route mount (via buildApp) -----------------------------------------

func TestBuildApp_JWKSRoute_MountedWhenConfigured(t *testing.T) {
	d := issuerDeps(t, &IssuerJWKSConfig{Path: "/.well-known/jwks.json"}, 0)
	app, err := buildApp(context.Background(), d, Wiring{
		OpenAPI: &openapi.Config{Title: "T", Version: "1.0.0"},
	})
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}
	resp, err := app.Test(httptest.NewRequest("GET", "/.well-known/jwks.json", nil))
	if err != nil {
		t.Fatalf("GET jwks: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("GET jwks = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"k1"`) {
		t.Fatalf("jwks body missing kid: %s", body)
	}
}

func TestBuildApp_JWKSRoute_AbsentWithoutJWKSBlock(t *testing.T) {
	d := issuerDeps(t, nil, 0)
	app, err := buildApp(context.Background(), d, Wiring{
		OpenAPI: &openapi.Config{Title: "T", Version: "1.0.0"},
	})
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}
	resp, err := app.Test(httptest.NewRequest("GET", "/.well-known/jwks.json", nil))
	if err != nil {
		t.Fatalf("GET jwks: %v", err)
	}
	if resp.StatusCode == 200 {
		t.Fatal("jwks route should not be mounted when auth.issuer.jwks is absent")
	}
}

func TestBuildApp_JWKSRoute_PublicEvenUnderJWTMode(t *testing.T) {
	d := issuerDeps(t, &IssuerJWKSConfig{Path: "/.well-known/jwks.json"}, 0)
	d.Config.Auth.Mode = AuthModeJWT
	d.Config.Auth.JWT = &JWTConfig{
		Algorithms: []string{"RS256"},
		Issuer:     "http://auth",
		Audience:   "orders-api",
		JWKSURL:    "http://unused.invalid/.well-known/jwks.json",
	}
	app, err := buildApp(context.Background(), d, Wiring{
		OpenAPI: &openapi.Config{Title: "T", Version: "1.0.0"},
	})
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}
	// No Authorization header at all — if the JWKS route were NOT
	// auto-added to the public bypass list, AuthMiddleware would reject
	// this with 401 before the handler ever runs.
	resp, err := app.Test(httptest.NewRequest("GET", "/.well-known/jwks.json", nil))
	if err != nil {
		t.Fatalf("GET jwks: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("GET jwks under auth.mode=jwt (no bearer) = %d, want 200 (auto-public)", resp.StatusCode)
	}
}
