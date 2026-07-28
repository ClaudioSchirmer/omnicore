package bootstrap

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/ClaudioSchirmer/omnicore/application/persistence"
	"github.com/ClaudioSchirmer/omnicore/application/translation"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/audit"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/events"
	"github.com/ClaudioSchirmer/omnicore/infra/integration"
	"github.com/ClaudioSchirmer/omnicore/web/graphql"
	fwgrpc "github.com/ClaudioSchirmer/omnicore/web/grpc"
	"github.com/ClaudioSchirmer/omnicore/web/openapi"
)

// White-box coverage for the boot orchestration paths that need no live
// infrastructure: buildApp's per-surface branches (auth, openapi, graphql,
// grpc), serve's listener + coordinated-drain flow on an ephemeral port with a
// pre-cancelled context, and the small pure config helpers. buildDeps / Run /
// runWithConfig stay integration-only — they dial real Postgres/Mongo.

// testPublicKeyPEM returns a fresh RSA public key in PEM form — enough for
// AuthMiddleware / NewAuthCoreValidator to construct without JWKS.
func testPublicKeyPEM(t *testing.T) string {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa: %v", err)
	}
	der, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

func jwtAuthConfig(pemKey string) AuthConfig {
	return AuthConfig{
		Mode: AuthModeJWT,
		JWT: &JWTConfig{
			Algorithms:   []string{"RS256"},
			Issuer:       "https://issuer.test",
			Audience:     "aud",
			PublicKeyPEM: pemKey,
		},
	}
}

func TestCfgStoreHelpers(t *testing.T) {
	if got := cfgCacheStore(nil); got != "" {
		t.Errorf("cfgCacheStore(nil) = %q", got)
	}
	if got := cfgCacheStore(&CacheConfig{}); got != "memory" {
		t.Errorf("cfgCacheStore(empty) = %q, want memory", got)
	}
	if got := cfgCacheStore(&CacheConfig{Store: "redis"}); got != "redis" {
		t.Errorf("cfgCacheStore(redis) = %q", got)
	}
	if got := cfgSharedStore(nil); got != "" {
		t.Errorf("cfgSharedStore(nil) = %q", got)
	}
	if got := cfgSharedStore(&CacheConfig{}); got != "" {
		t.Errorf("cfgSharedStore(no shared) = %q", got)
	}
	if got := cfgSharedStore(&CacheConfig{Shared: &CacheSharedConfig{Store: "redis"}}); got != "redis" {
		t.Errorf("cfgSharedStore(redis) = %q", got)
	}
}

func TestBuildApp_RequestTimeoutOverride(t *testing.T) {
	d := silentDeps()
	secs := 7
	d.Config.HTTP.RequestTimeoutSeconds = &secs
	if _, err := buildApp(context.Background(), d, Wiring{}); err != nil {
		t.Fatalf("buildApp: %v", err)
	}
}

func TestBuildApp_AuthJWT(t *testing.T) {
	t.Run("enabled", func(t *testing.T) {
		d := silentDeps()
		d.Config.Auth = jwtAuthConfig(testPublicKeyPEM(t))
		app, err := buildApp(context.Background(), d, Wiring{})
		if err != nil {
			t.Fatalf("buildApp: %v", err)
		}
		// A protected route without a token is rejected by the middleware.
		// The /livez probe is framework-owned but NOT auto-public: unless the
		// operator lists it in auth.publicRoutes it sits behind auth like any
		// other route, so a tokenless call is 401 (the option-A contract).
		resp, err := app.Test(httptest.NewRequest("GET", "/livez", nil))
		if err != nil {
			t.Fatalf("GET /livez: %v", err)
		}
		if resp.StatusCode != 401 {
			t.Errorf("GET /livez without token = %d, want 401", resp.StatusCode)
		}
	})
	t.Run("invalidKeyAbortsBoot", func(t *testing.T) {
		d := silentDeps()
		d.Config.Auth = jwtAuthConfig("not a pem")
		if _, err := buildApp(context.Background(), d, Wiring{}); err == nil ||
			!strings.Contains(err.Error(), "auth middleware") {
			t.Fatalf("expected the auth middleware boot error, got %v", err)
		}
	})
}

// receiverFeature registers HTTP routes AND integration receivers from the
// same struct — the IntegrationFeature phase in buildApp.
type receiverFeature struct{ mounted *bool }

func (f *receiverFeature) Mount(_ *fiber.App, _ Deps) {}
func (f *receiverFeature) MountReceivers(_ *integration.Registry, _ Deps) {
	*f.mounted = true
}

func TestBuildApp_IntegrationReceiversPhase(t *testing.T) {
	d := silentDeps()
	d.IntegrationRegistry = integration.NewRegistry()
	mounted := false
	if _, err := buildApp(context.Background(), d, Wiring{Features: []Feature{&receiverFeature{mounted: &mounted}}}); err != nil {
		t.Fatalf("buildApp: %v", err)
	}
	if !mounted {
		t.Error("MountReceivers must run during the receivers phase")
	}
}

func TestBuildApp_AuthorizationScanAndOpenAPIAuthContext(t *testing.T) {
	d := silentDepsWithRegistry()
	d.Config.Auth = jwtAuthConfig(testPublicKeyPEM(t))
	d.Config.Auth.Authorization = &AuthorizationConfig{Enabled: true}
	d.Config.OpenAPI.RootRedirect = true
	if _, err := buildApp(context.Background(), d, Wiring{
		OpenAPI:      &openapi.Config{Title: "T", Version: "1.0.0", LanguageSelector: true},
		Translations: []translation.Module{translation.CoreENG(), translation.CorePTBR()},
	}); err != nil {
		t.Fatalf("buildApp: %v", err)
	}
}

func TestBuildApp_GraphQLRootRedirect(t *testing.T) {
	d := silentDepsWithRegistry()
	d.Config.GraphQL.Path = "/graphql"
	d.Config.GraphQL.RootRedirect = true
	reg := graphqlRegistryForTest(d)
	app, err := buildApp(context.Background(), d, Wiring{GraphQL: reg})
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}
	resp, err := app.Test(httptest.NewRequest("GET", "/", nil))
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	if resp.StatusCode != 302 {
		t.Errorf("GET / = %d, want 302 to the GraphQL endpoint", resp.StatusCode)
	}
}

func TestBuildApp_GRPCPostures(t *testing.T) {
	pemKey := testPublicKeyPEM(t)

	t.Run("inheritWithJWT", func(t *testing.T) {
		d := silentDeps()
		d.Config.Auth = jwtAuthConfig(pemKey)
		d.Config.GRPC.RequestTimeoutSeconds = 3
		d.Config.GRPC.Reflection = true
		if _, err := buildApp(context.Background(), d, Wiring{GRPC: fwgrpc.New(d.Pipeline)}); err != nil {
			t.Fatalf("buildApp: %v", err)
		}
	})
	t.Run("inheritWithJWT_invalidKey", func(t *testing.T) {
		d := silentDeps()
		d.Config.Auth = jwtAuthConfig("garbage")
		d.Config.Auth.Mode = "" // HTTP middleware off — only the gRPC branch builds a validator
		d.Config.Auth.Mode = AuthModeJWT
		// The HTTP AuthMiddleware fails first with the same key; assert the boot
		// aborts either way (both errors wrap the same authcore construction).
		if _, err := buildApp(context.Background(), d, Wiring{GRPC: fwgrpc.New(d.Pipeline)}); err == nil {
			t.Fatal("expected a boot error with an invalid key")
		}
	})
	t.Run("internalWithAttribution", func(t *testing.T) {
		d := silentDeps()
		d.Config.Auth = jwtAuthConfig(pemKey)
		d.Config.Auth.Mode = "" // internal plane: global auth off, attribution still built from JWT material
		d.Config.Auth.JWT.PublicKeyPEM = pemKey
		d.Config.GRPC.Auth.Mode = "internal"
		if _, err := buildApp(context.Background(), d, Wiring{GRPC: fwgrpc.New(d.Pipeline)}); err != nil {
			t.Fatalf("buildApp: %v", err)
		}
	})
	t.Run("internalAttributionInvalidKey", func(t *testing.T) {
		d := silentDeps()
		d.Config.Auth = jwtAuthConfig("garbage")
		d.Config.Auth.Mode = "" // keep the HTTP middleware out of the way
		d.Config.GRPC.Auth.Mode = "internal"
		if _, err := buildApp(context.Background(), d, Wiring{GRPC: fwgrpc.New(d.Pipeline)}); err == nil ||
			!strings.Contains(err.Error(), "grpc attribution validator") {
			t.Fatalf("expected the attribution boot error, got %v", err)
		}
	})
	t.Run("internalWithoutJWTMaterial", func(t *testing.T) {
		d := silentDeps()
		d.Config.GRPC.Auth.Mode = "internal"
		if _, err := buildApp(context.Background(), d, Wiring{GRPC: fwgrpc.New(d.Pipeline)}); err != nil {
			t.Fatalf("buildApp: %v", err)
		}
	})
	t.Run("mtlsPosture", func(t *testing.T) {
		d := silentDeps()
		d.Config.GRPC.Auth.Mode = "mtls"
		if _, err := buildApp(context.Background(), d, Wiring{GRPC: fwgrpc.New(d.Pipeline)}); err != nil {
			t.Fatalf("buildApp: %v", err)
		}
	})
}

// ─── serve ───────────────────────────────────────────────────────────────────

// serveDeps returns Deps sufficient for serve() on an ephemeral port: no
// integration receivers, nil SyncEngine/Tracing (both drain nil-safe).
func serveDeps() Deps {
	d := silentDeps()
	d.Config.HTTP.Addr = "127.0.0.1:0"
	d.IntegrationRegistry = integration.NewRegistry()
	return d
}

// cancelledCtx returns a context that is already done — serve() builds the
// app, spawns the listener, immediately observes ctx.Done() and drains.
func cancelledCtx() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func TestServe_DrainFlowOnCancelledContext(t *testing.T) {
	shutdownRan := false
	err := serve(cancelledCtx(), serveDeps(), Wiring{
		OnShutdown: func(context.Context) error { shutdownRan = true; return nil },
	})
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	if !shutdownRan {
		t.Error("OnShutdown must run during the drain")
	}
}

func TestServe_OnShutdownErrorIsBestEffort(t *testing.T) {
	err := serve(cancelledCtx(), serveDeps(), Wiring{
		OnShutdown: func(context.Context) error { return errors.New("hook boom") },
	})
	if err != nil {
		t.Fatalf("an OnShutdown failure is logged, not propagated: %v", err)
	}
}

func TestServe_BuildAppErrorPropagates(t *testing.T) {
	d := serveDeps()
	d.Config.Auth = jwtAuthConfig("garbage")
	if err := serve(cancelledCtx(), d, Wiring{}); err == nil ||
		!strings.Contains(err.Error(), "auth middleware") {
		t.Fatalf("expected the buildApp error, got %v", err)
	}
}

func TestServe_UncoveredSubscriptionAbortsBoot(t *testing.T) {
	d := serveDeps()
	d.Config.Integration = &integration.Config{
		Subscribes: map[string]integration.SubscribeEntry{
			"billing": {
				Topic:  "billing.events",
				Events: map[string]integration.SubscribeEvent{"invoiced": {}},
			},
		},
	}
	// Registry empty → the declared subscription has no receiver.
	if err := serve(cancelledCtx(), d, Wiring{}); err == nil {
		t.Fatal("expected the uncovered-subscription boot abort")
	}
}

func TestServe_ListenErrorAborts(t *testing.T) {
	d := serveDeps()
	d.Config.HTTP.Addr = "127.0.0.1:-1" // invalid port → listen fails
	// Live context: serve must return via the errCh path, not ctx.Done().
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := serve(ctx, d, Wiring{}); err == nil ||
		!strings.Contains(err.Error(), "http listen") {
		t.Fatalf("expected the http listen error, got %v", err)
	}
}

func TestServe_GRPCListenerH2C(t *testing.T) {
	d := serveDeps()
	d.Config.GRPC.Addr = "127.0.0.1:0"
	d.Config.GRPC.IdleTimeoutSeconds = 30
	if err := serve(cancelledCtx(), d, Wiring{GRPC: fwgrpc.New(d.Pipeline)}); err != nil {
		t.Fatalf("serve with grpc listener: %v", err)
	}
}

func TestServe_MTLSClientCAErrors(t *testing.T) {
	t.Run("missingFile", func(t *testing.T) {
		d := serveDeps()
		d.Config.GRPC.Auth.Mode = "mtls"
		d.Config.GRPC.ClientCAFile = filepath.Join(t.TempDir(), "absent.pem")
		if err := serve(cancelledCtx(), d, Wiring{GRPC: fwgrpc.New(d.Pipeline)}); err == nil ||
			!strings.Contains(err.Error(), "clientCAFile") {
			t.Fatalf("expected the clientCAFile error, got %v", err)
		}
	})
	t.Run("unusableCA", func(t *testing.T) {
		d := serveDeps()
		d.Config.GRPC.Auth.Mode = "mtls"
		caPath := filepath.Join(t.TempDir(), "junk.pem")
		if err := os.WriteFile(caPath, []byte("not a certificate"), 0o600); err != nil {
			t.Fatal(err)
		}
		d.Config.GRPC.ClientCAFile = caPath
		if err := serve(cancelledCtx(), d, Wiring{GRPC: fwgrpc.New(d.Pipeline)}); err == nil ||
			!strings.Contains(err.Error(), "no usable CA certificate") {
			t.Fatalf("expected the unusable-CA error, got %v", err)
		}
	})
}

// ─── reconcileViewDrift: the no-views fast paths ─────────────────────────────

// bootFakeEngine is the minimal core.RelationalEngine for boot-path tests: the
// no-views drift detection touches only Querier()/Dialect() before its loop.
type bootFakeEngine struct{}

func (bootFakeEngine) Insert(persistence.RequestContext, domain.Insertable, *core.TableSchema, core.WriteHook) (domain.WriteResult, error) {
	return domain.WriteResult{}, nil
}
func (bootFakeEngine) Update(persistence.RequestContext, domain.Updatable, *core.TableSchema, core.WriteHook) (domain.WriteResult, error) {
	return domain.WriteResult{}, nil
}
func (bootFakeEngine) Archive(persistence.RequestContext, domain.Archivable, *core.TableSchema, core.WriteHook) error {
	return nil
}
func (bootFakeEngine) Unarchive(persistence.RequestContext, domain.Unarchivable, *core.TableSchema, core.WriteHook) error {
	return nil
}
func (bootFakeEngine) Delete(persistence.RequestContext, domain.Deletable, *core.TableSchema, core.WriteHook) error {
	return nil
}
func (bootFakeEngine) Querier() core.Querier { return nil }
func (bootFakeEngine) Dialect() core.Dialect { return nil }
func (e bootFakeEngine) WithAudit(*audit.Config, *slog.Logger, []string) core.RelationalEngine {
	return e
}
func (e bootFakeEngine) WithEventPublisher(events.Publisher) core.RelationalEngine { return e }
func (bootFakeEngine) AcquireRebuildLock(context.Context, string) (core.RebuildLock, error) {
	return nil, errors.New("not acquirable in boot tests")
}
func (bootFakeEngine) Close() {}

func TestReconcileViewDrift_NoViewsFastPaths(t *testing.T) {
	d := silentDeps()
	d.DB = bootFakeEngine{}

	t.Run("checkModeUpToDate", func(t *testing.T) {
		cfg := &Config{Service: "t"}
		cfg.Mongo.Rebuild.AutoRun = AutoRunCheck
		if _, _, err := reconcileViewDrift(context.Background(), cfg, d, nil, nil); err != nil {
			t.Fatalf("check mode with no views: %v", err)
		}
	})
	t.Run("autoRunTrueNoPlans", func(t *testing.T) {
		cfg := &Config{Service: "t"}
		cfg.Mongo.Rebuild.AutoRun = AutoRunTrue
		if _, _, err := reconcileViewDrift(context.Background(), cfg, d, nil, nil); err != nil {
			t.Fatalf("autoRun=true with no views: %v", err)
		}
	})
	t.Run("autoRunFalseSkips", func(t *testing.T) {
		cfg := &Config{Service: "t"}
		cfg.Mongo.Rebuild.AutoRun = AutoRunFalse
		if _, _, err := reconcileViewDrift(context.Background(), cfg, d, nil, nil); err != nil {
			t.Fatalf("autoRun=false: %v", err)
		}
	})
}

// ─── startUpstreamSubscribers ────────────────────────────────────────────────

func TestStartUpstreamSubscribers(t *testing.T) {
	d := silentDeps()
	d.DB = bootFakeEngine{}
	cfg := &Config{Service: "t"}
	cfg.Transport.Endpoints = []string{"127.0.0.1:1"} // unroutable; the reader loop exits on the cancelled ctx
	// The linked transport adapter over the unroutable broker. Under a tagless
	// build newTransportSubscriber is the transport_none stub — it returns a nil
	// Subscriber, so the startsAndDrains subtest (which opens a real subscription)
	// is skipped rather than dereferencing nil in the reader goroutine.
	d.Transport, _ = newTransportSubscriber(cfg)

	t.Run("emptyList", func(t *testing.T) {
		if got := startUpstreamSubscribers(cancelledCtx(), d, cfg, nil, nil, nil); got != nil {
			t.Fatalf("expected nil for no subscriptions, got %v", got)
		}
	})
	t.Run("constructorErrorSkips", func(t *testing.T) {
		subs := []UpstreamSubscription{{Topic: "", Collection: "c1"}} // empty topic → constructor error
		if got := startUpstreamSubscribers(cancelledCtx(), d, cfg, subs, nil, nil); len(got) != 0 {
			t.Fatalf("a failed constructor must be skipped, got %d subscribers", len(got))
		}
	})
	t.Run("startsAndDrains", func(t *testing.T) {
		if d.Transport == nil {
			t.Skip("no transport adapter linked (build without a transport tag) — startsAndDrains needs a real Subscriber")
		}
		subs := []UpstreamSubscription{{Topic: "t1", Collection: "c1", ConsumerGroup: "g1", Workers: 1}}
		started := startUpstreamSubscribers(cancelledCtx(), d, cfg, subs, nil, nil)
		if len(started) != 1 {
			t.Fatalf("expected 1 subscriber, got %d", len(started))
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := started[0].Shutdown(shutdownCtx); err != nil {
			t.Fatalf("subscriber shutdown: %v", err)
		}
	})
}

// graphqlRegistryForTest builds the empty (stub-Query) GraphQL registry the
// mount tests use.
func graphqlRegistryForTest(d Deps) *graphql.Registry {
	return graphql.New(d.Pipeline)
}
