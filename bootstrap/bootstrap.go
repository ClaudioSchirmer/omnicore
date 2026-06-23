package bootstrap

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/application/translation"
	"github.com/ClaudioSchirmer/omnicore/infra"
	"github.com/ClaudioSchirmer/omnicore/infra/audit"
	"github.com/ClaudioSchirmer/omnicore/infra/httpclient"
	"github.com/ClaudioSchirmer/omnicore/infra/integration"
	"reflect"

	fwweb "github.com/ClaudioSchirmer/omnicore/web"
	"github.com/ClaudioSchirmer/omnicore/web/openapi"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/logger"
)

// shutdownTimeout is the Fiber drain duration. Constant for simplicity.
const shutdownTimeout = 10 * time.Second

// Run loads microservice.<profile>.yaml + builds singletons + calls wire(deps)
// + registers default middlewares + runs until receiving SIGINT/SIGTERM.
//
// Boot order:
//  1. LoadConfig (reads APP_PROFILE env, loads microservice.<profile>.yaml or
//     $OMNICORE_CONFIG_PATH; rejects auth.mode=disabled outside dev)
//  2. signal.NotifyContext(SIGINT, SIGTERM)
//  3. NewPostgres + NewMongoDB (defer Close)
//  4. translation.Default + pipeline.New + SlogAuditor + ViewReader
//  5. wire(deps) → Wiring
//  6. validateWiring (rejects nothing-to-serve)
//  7. Translator.Import of the service modules
//  8. Migrations (if cfg.Migrations.AutoRun)
//  9. collectViews (aggregates Views from ReadableFeatures + rejects collision)
// 10. CheckServiceRegistry (DB-per-service guard — warn in dev, abort otherwise)
//     + ApplyMongoSpecs (declared indexes / validators / collation / capped /
//     time-series materialized on the Mongo cluster) — skipped when no views
// 11. SyncEngine.Start if views are not empty
// 12. Fiber + Recover/Logger/AppContextMiddleware + AuthMiddleware (when
//     auth.mode=jwt) + automatic /health
// 13. f.Mount(app, deps) for each Feature
// 14. Wiring.BeforeServe(app, deps) if set
// 15. app.Listen in a goroutine
// 16. waits for ctx.Done() → ShutdownWithContext(10s) → Wiring.OnShutdown
//
// Returns boot error (invalid yaml, failed connection, validate, BeforeServe,
// listen) or nil when the server starts and terminates by signal.
func Run(wire func(Deps) Wiring) error {
	cfg, err := LoadConfig()
	if err != nil {
		return err
	}
	return runWithConfig(cfg, wire)
}

// Build constructs Deps from the profile-specific microservice.<profile>.yaml
// and returns without starting the server. Useful for tests or services with a
// custom lifecycle.
func Build() (Deps, *Config, error) {
	cfg, err := LoadConfig()
	if err != nil {
		return Deps{}, nil, err
	}
	deps, err := buildDeps(cfg)
	return deps, cfg, err
}

// Serve uses already-built Deps (via Build) + Wiring to run the server.
// Does not import translations nor start SyncEngine — whoever uses Build/Serve
// manually is responsible for doing that beforehand (direct access via Deps).
func Serve(ctx context.Context, deps Deps, wiring Wiring) error {
	return serve(ctx, deps, wiring)
}

func runWithConfig(cfg *Config, wire func(Deps) Wiring) error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	deps, err := buildDeps(cfg)
	if err != nil {
		return err
	}
	defer deps.Postgres.Close()
	defer func() { _ = deps.Mongo.Close(context.Background()) }()

	wiring := wire(deps)

	if err := validateWiring(wiring); err != nil {
		return err
	}

	// Late cache injection — when cfg.Cache.Store == "custom" (or
	// cache.shared.store == "custom"), buildDeps left the matching
	// Deps.Cache / Deps.SharedCache nil because the implementation
	// arrives via Wiring. Now that Wire(deps) has run, reconcile:
	// resolveCache pairs the YAML intent with the injected instance,
	// rejects mismatches (e.g. Wire.Cache set but store: memory), and
	// the httpclient picks up the resolved private cache via SetCache
	// — late binding through the atomic pointer means in-flight chain
	// reads see the new instance without rebuild.
	if c, err := resolveCache(cfg.Cache, wiring.Cache); err != nil {
		return fmt.Errorf("bootstrap: cache (post-wire): %w", err)
	} else if c != nil && c != deps.Cache {
		deps.Cache = c
		if deps.HttpClient != nil {
			deps.HttpClient.SetCache(c)
		}
	}
	if c, err := resolveSharedCache(cfg.Cache, wiring.SharedCache); err != nil {
		return fmt.Errorf("bootstrap: shared cache (post-wire): %w", err)
	} else if c != nil && c != deps.SharedCache {
		deps.SharedCache = c
	}

	if wiring.OpenAPI != nil {
		deps.OpenAPIRegistry = openapi.NewRegistry()
		deps.Logger.Info("openapi enabled",
			"title", wiring.OpenAPI.Title,
			"version", wiring.OpenAPI.Version,
			"languageSelector", wiring.OpenAPI.LanguageSelector)
	}

	for _, mod := range wiring.Translations {
		deps.Translator.Import(mod)
	}

	if err := applyMigrations(ctx, cfg, deps); err != nil {
		return err
	}

	// Ensure the next 3 monthly partitions of audit_events exist. Idempotent
	// across boots (CREATE TABLE IF NOT EXISTS), runs after migrations so the
	// parent table is guaranteed to be in place, runs before serving HTTP so
	// the first write of the day never lands in a missing partition. Skipped
	// when audit is fully off (destinations: []) since no row will ever land.
	if cfg.Audit.Includes(audit.DestinationDatabase) {
		if err := audit.EnsureFuturePartitions(ctx, deps.Postgres.Pool(), 3); err != nil {
			return fmt.Errorf("bootstrap: ensure audit partitions: %w", err)
		}
	}

	views, err := collectViews(wiring.Features)
	if err != nil {
		return err
	}

	// Wire the read-side per-view max-limit resolver into the framework's
	// MongoViewReader. Resolution at read time: ViewDefinition.MaxLimit
	// (per-view override) > cfg.Query.MaxLimit (yaml default) > the reader's
	// framework constant 100. Custom ViewReader implementations bypass this
	// hook by design — they own their own limit policy.
	if mvr, ok := deps.ViewReader.(*infra.MongoViewReader); ok {
		mvr.SetMaxLimitResolver(buildViewMaxLimitResolver(views, cfg.Query.MaxLimit))
		// Register the views so the reader can translate criteria/documents
		// between Go field names and physical columns via each view's
		// TableSchema tree.
		mvr.SetViews(views)
	}

	for _, f := range wiring.Features {
		_, readable := f.(ReadableFeature)
		deps.Logger.Info("feature registered", "type", fmt.Sprintf("%T", f), "readable", readable)
	}

	// Cross-service composition: resolve cfg + Wiring subscriptions,
	// apply defaults, run the four boot guards (§8). Runs BEFORE
	// SyncEngine.Start and BEFORE the UpstreamSubscriber goroutines
	// spin so any structural error aborts the boot deterministically.
	upstreamSubs, err := resolveUpstreamSubscriptions(cfg, wiring)
	if err != nil {
		return err
	}
	upstreamSubs = applyUpstreamSubscriptionDefaults(upstreamSubs, cfg.Service)
	if len(upstreamSubs) > 0 {
		if err := validateUpstreamSubscriptions(upstreamSubs, views, cfg.Profile); err != nil {
			return err
		}
	}

	// Schema is mandatory on every view — the read membrane (Go↔column) and the
	// composer (PK + soft-delete) resolve through it, so a view without a root
	// schema would have no lossless mapping. Embed schemas are guaranteed by
	// construction (FromSchema is the only source constructor); the root schema
	// is the one a consumer could forget, so it is enforced here.
	if err := infra.ValidateViewSchemas(views); err != nil {
		return err
	}

	if len(views) > 0 {
		// DB-per-service guard: writes the per-boot marker, scans for
		// foreign collections, warns in dev / aborts otherwise. Runs
		// before ApplyMongoSpecs so a guard failure short-circuits
		// before any write touches the cluster.
		if err := infra.CheckServiceRegistry(ctx, deps.Mongo, cfg.Service, cfg.Profile, views); err != nil {
			return fmt.Errorf("bootstrap: mongo registry guard: %w", err)
		}

		// Apply declared Mongo specs (indexes, $jsonSchema validator,
		// collation, capped, time-series). Idempotent on steady state;
		// strict-on-divergence by default, FORCE_REBUILD env var as the
		// operator escape for index conflicts.
		if err := infra.ApplyMongoSpecs(ctx, deps.Mongo, views); err != nil {
			return fmt.Errorf("bootstrap: mongo apply specs: %w", err)
		}

		syncEngine := infra.NewSyncEngine(deps.Postgres, deps.Mongo,
			cfg.Kafka.Brokers, cfg.Kafka.SyncGroupID, views, cfg.Kafka.SyncWorkers)

		// Drift detection + rebuild reconciliation. Runs AFTER
		// ApplyMongoSpecs (collection shape reconciled) and BEFORE
		// SyncEngine.Start (no live events should reach a drifted view).
		if err := reconcileViewDrift(ctx, cfg, deps, syncEngine, views); err != nil {
			return err
		}

		// Start cross-service subscribers BEFORE SyncEngine so any
		// upstream-projected docs in B's Mongo are ready when local
		// views start composing — recompose-ripple inside the
		// subscriber + the local SyncEngine path both reach the same
		// composer (shared via the explicit handle) so cross-store
		// embeds resolve consistently.
		deps.UpstreamSubscribers = startUpstreamSubscribers(ctx, deps, cfg, upstreamSubs, views)

		syncEngine.Start(ctx)
		deps.Logger.Info("sync engine started",
			"brokers", cfg.Kafka.Brokers,
			"groupId", cfg.Kafka.SyncGroupID,
			"views", len(views),
			"workers", cfg.Kafka.SyncWorkers)
	} else if len(upstreamSubs) > 0 {
		// Degenerate case: B declared upstream subscriptions but no
		// local views. The subscribers still materialize the local
		// Mongo collection (operator may consume via mongosh or a
		// custom adapter); the recompose-ripple is a no-op since no
		// view embeds the collection.
		deps.UpstreamSubscribers = startUpstreamSubscribers(ctx, deps, cfg, upstreamSubs, views)
	}

	return serve(ctx, deps, wiring)
}

func buildDeps(cfg *Config) (Deps, error) {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	ctx := context.Background()

	pg, err := infra.NewPostgres(ctx, cfg.Postgres.DSN)
	if err != nil {
		return Deps{}, fmt.Errorf("bootstrap: postgres connect: %w", err)
	}
	logger.Info("postgres connected", "dsn", redact(cfg.Postgres.DSN))

	mg, err := infra.NewMongoDB(ctx, cfg.Mongo.URI, cfg.Mongo.Database)
	if err != nil {
		pg.Close()
		return Deps{}, fmt.Errorf("bootstrap: mongo connect: %w", err)
	}
	logger.Info("mongo connected", "uri", redact(cfg.Mongo.URI), "db", cfg.Mongo.Database)

	tr := translation.Default()
	pipe := pipeline.New(tr).WithLogger(logger)
	// Audit travels through the Postgres adapter — configured at boot, then
	// every Insert/Update/Archive/Unarchive/Delete emits the configured
	// destinations automatically. nil cfg.Audit destinations slice would
	// have been populated by applyDefaults already.
	pg.WithAudit(&cfg.Audit, logger, cfg.Auth.AuditClaims)
	viewReader := infra.NewMongoViewReader(mg)

	// Resolve the SERVICE-PRIVATE cache from cfg only (no Wire
	// injection at this stage). If cfg.Cache.Store == "custom", the
	// resolution returns nil + error pointing operator to the
	// Build+Serve flow with Wiring.Cache. memory / redis backends
	// resolve directly.
	privateCache, err := resolveCache(cfg.Cache, nil)
	if err != nil {
		pg.Close()
		_ = mg.Close(context.Background())
		return Deps{}, fmt.Errorf("bootstrap: cache init: %w", err)
	}
	sharedCache, err := resolveSharedCache(cfg.Cache, nil)
	if err != nil {
		pg.Close()
		_ = mg.Close(context.Background())
		return Deps{}, fmt.Errorf("bootstrap: shared cache init: %w", err)
	}
	if privateCache != nil {
		logger.Info("cache configured", "store", cfgCacheStore(cfg.Cache))
	}
	if sharedCache != nil {
		logger.Info("shared cache configured", "store", cfgSharedStore(cfg.Cache))
	}

	var hc *httpclient.HttpClient
	if cfg.HttpClient != nil {
		hc, err = httpclient.New(cfg.HttpClient,
			httpclient.WithLogger(logger),
			httpclient.WithCache(privateCache))
		if err != nil {
			pg.Close()
			_ = mg.Close(context.Background())
			return Deps{}, fmt.Errorf("bootstrap: httpclient init: %w", err)
		}
		logger.Info("httpclient configured")
	}

	// Integration registry is constructed unconditionally so consumer
	// features can stash the pointer from their constructor; Configure
	// makes the producer-side singleton available BEFORE feature mounts
	// so a feature's BeforeCommit closure can reference fwintegration.Dispatch
	// without nil-checking. Services that emit nothing AND consume
	// nothing pay only the empty struct cost.
	integration.Configure(cfg.Integration, pg, logger)
	integrationRegistry := integration.NewRegistry()

	return Deps{
		Config:              cfg,
		Logger:              logger,
		Postgres:            pg,
		Mongo:               mg,
		Translator:          tr,
		Pipeline:            pipe,
		ViewReader:          viewReader,
		Export:              fwweb.ExportDeps{Translator: tr, MaxExportRows: cfg.Query.MaxExportRows},
		Cache:               privateCache,
		SharedCache:         sharedCache,
		HttpClient:          hc,
		IntegrationRegistry: integrationRegistry,
	}, nil
}

// cfgCacheStore returns the operator-declared store value (or the
// effective default "memory") for logging. Defensive guard so a missing
// block is reported as "" rather than panicking.
func cfgCacheStore(cfg *CacheConfig) string {
	if cfg == nil {
		return ""
	}
	if cfg.Store == "" {
		return "memory"
	}
	return cfg.Store
}

func cfgSharedStore(cfg *CacheConfig) string {
	if cfg == nil || cfg.Shared == nil {
		return ""
	}
	return cfg.Shared.Store
}

// buildApp assembles the *fiber.App with default middlewares, /health route
// provided by the framework, and the service features. Does not call Listen —
// extracted from serve for testability via app.Test without networking.
func buildApp(deps Deps, wiring Wiring) (*fiber.App, error) {
	// Register the framework's translator-backed gate + standalone
	// translator BEFORE any Mount/MountRaw call (including /health below
	// and every feature). The gate is consumed by Mount/MountRaw when a
	// route declares fwopenapi.RequirePermission(...); the standalone
	// translator is consumed by RespondWithInternalServerError when the
	// canonical 500 path runs outside Pipeline. Both are package-level
	// set-once singletons matching the SetPermissionsClaim/SetTenantClaim
	// pattern.
	fwweb.SetTranslator(deps.Translator)
	openapi.SetGate(fwweb.PermissionGate(deps.Translator))

	// Apply authorization claim-name configuration BEFORE the auth middleware
	// runs so the first request observes the configured names. No-op when the
	// yaml carries no authorization block.
	applyAuthorizationConfig(deps.Config.Auth.Authorization)

	// Enable the permission-gate enforcement when the operator opted in via
	// auth.authorization.enabled. Default-off (the package-level flag's zero
	// value) matches the design's rollout pattern: annotate routes with
	// RequirePermission, see them in the spec, then flip the yaml when the
	// IdP emits the permissions claim.
	fwweb.SetAuthorizationEnabled(deps.Config.Auth.Authorization != nil &&
		deps.Config.Auth.Authorization.Enabled)

	app := fiber.New(fiber.Config{
		AppName:      deps.Config.Service,
		ErrorHandler: fwweb.ErrorHandler(deps.Pipeline),
	})

	app.Use(fwweb.Recover())
	app.Use(logger.New())
	app.Use(fwweb.AppContextMiddleware())

	uiPath := deps.Config.OpenAPI.UIPath
	if uiPath == "" {
		uiPath = defaultOpenAPIUIPath
	}

	// augmentedPublicRoutes is the canonical bypass list shared by the
	// AuthMiddleware AND the authz scan, so both honor the documentation
	// surface and the optional root redirect uniformly.
	augmentedPublicRoutes := append([]string{}, deps.Config.Auth.PublicRoutes...)
	if wiring.OpenAPI != nil {
		augmentedPublicRoutes = append(augmentedPublicRoutes,
			"GET "+openapi.SpecPath, "GET "+uiPath)
		if deps.Config.OpenAPI.RootRedirect {
			augmentedPublicRoutes = append(augmentedPublicRoutes, "GET /")
		}
	}

	if deps.Config.Auth.Mode == AuthModeJWT {
		authOpts := authOptionsFromConfig(deps.Config.Auth)
		authOpts.PublicRoutes = augmentedPublicRoutes
		mw, err := fwweb.AuthMiddleware(authOpts, deps.Pipeline)
		if err != nil {
			return nil, fmt.Errorf("bootstrap: auth middleware: %w", err)
		}
		app.Use(mw)
		deps.Logger.Info("auth middleware enabled", "issuer", deps.Config.Auth.JWT.Issuer, "audience", deps.Config.Auth.JWT.Audience)
	}

	openapi.MountRaw(deps.OpenAPIRegistry, app, fiber.MethodGet, "/health",
		func(c fiber.Ctx) error {
			return c.JSON(fiber.Map{"status": "ok"})
		},
		openapi.RawSpec{
			Summary: "Liveness probe",
			Tags:    []string{"Health"},
			Public:  true,
			Responses: map[int]openapi.ResponseSpec{
				200: {
					Description: "Service is up",
					Type:        reflect.TypeOf(healthResponse{}),
				},
			},
		})

	for _, f := range wiring.Features {
		f.Mount(app, deps)
	}

	// Phase Receivers — runs AFTER Phase HTTP (Mount) so a single
	// feature can register HTTP routes AND integration receivers from
	// the same struct. Opt-in via the IntegrationFeature interface
	// (type assertion). The registry now exposes the populated slice
	// to bootstrap.serve, which spins one ConsumerPool covering every
	// receiver.
	for _, f := range wiring.Features {
		if ifeat, ok := f.(IntegrationFeature); ok {
			ifeat.MountReceivers(deps.IntegrationRegistry, deps)
		}
	}

	if wiring.BeforeServe != nil {
		if err := wiring.BeforeServe(app, deps); err != nil {
			return nil, fmt.Errorf("bootstrap: BeforeServe: %w", err)
		}
	}

	// Boot scans — run AFTER features + BeforeServe so every route the
	// service registers is observed, and BEFORE openapi.Register so the
	// panic (when triggered) precedes any traffic.
	//
	// scanRouteRegistration enforces the framework-wide convention that
	// every Fiber route goes through openapi.Mount/MountRaw — independent
	// of authz, active whenever the spec surface is enabled.
	//
	// scanAuthorization enforces "every non-public route declares
	// RequirePermission" — only when auth.authorization.enabled is set.
	scanRouteRegistration(app, deps.OpenAPIRegistry)
	if authz := deps.Config.Auth.Authorization; authz != nil && authz.Enabled {
		scanAuthorization(deps.OpenAPIRegistry, augmentedPublicRoutes)
	}

	if wiring.OpenAPI != nil {
		opts := []openapi.RegisterOption{openapi.WithUIPath(uiPath)}
		if deps.Config.Auth.Mode == AuthModeJWT {
			// Auth context for the spec uses the SAME augmented allowlist
			// the AuthMiddleware sees, so /openapi.json + uiPath are
			// declared public on both sides of the wire (middleware lets
			// them through, spec does not advertise bearerAuth on them).
			publicRoutes := append([]string{}, deps.Config.Auth.PublicRoutes...)
			publicRoutes = append(publicRoutes, "GET "+openapi.SpecPath, "GET "+uiPath)
			authzEnabled := deps.Config.Auth.Authorization != nil && deps.Config.Auth.Authorization.Enabled
			opts = append(opts, openapi.WithAuth(openapi.AuthContext{
				PublicRoutes:         publicRoutes,
				AuthorizationEnabled: authzEnabled,
			}))
		}
		// Copy by value before populating Languages so the consumer's
		// *openapi.Config pointer is never mutated as a side effect of
		// boot. Auto-discovery only fires when the operator opted in
		// via LanguageSelector=true AND the consumer's own slice is
		// empty — an explicit Languages= override always wins.
		cfg := *wiring.OpenAPI
		if cfg.LanguageSelector && len(cfg.Languages) == 0 {
			cfg.Languages = collectLanguageOptions(wiring.Translations)
			deps.Logger.Info("openapi language selector",
				"languages", len(cfg.Languages))
		}
		openapi.Register(app, cfg, deps.OpenAPIRegistry, opts...)
		deps.Logger.Info("openapi served", "spec", openapi.SpecPath, "ui", uiPath)

		if deps.Config.OpenAPI.RootRedirect {
			registerRootRedirect(app, uiPath, deps.Logger)
		}
	}

	// GraphQL is its own web surface — mounted here, AFTER the REST boot scans
	// (scanRouteRegistration / scanAuthorization police only the OpenAPI route
	// contract) and outside the Swagger document, exactly like the framework's
	// own non-spec routes. The AuthMiddleware (matched by path at request time)
	// still authenticates it when auth.mode=jwt; authorization is per-field in
	// the resolver. The only thing shared with REST is the application-layer
	// handlers the resolvers dispatch to.
	if wiring.GraphQL != nil {
		path := deps.Config.GraphQL.Path
		app.Post(path, wiring.GraphQL.Handler())
		deps.Logger.Info("graphql served", "path", path)
		if deps.Config.GraphQL.RootRedirect {
			registerRootRedirect(app, path, deps.Logger)
		}
	}

	// Validate the operator-declared auth.publicRoutes against the fully
	// registered route set (features + /health + the OpenAPI spec/UI + the
	// optional root redirect). Runs last so every route the service exposes
	// is observable, and before serving HTTP so a typo / unmatchable param
	// path aborts the boot rather than silently leaving a route behind auth.
	scanPublicRoutes(app, deps.Config.Auth.PublicRoutes)

	return app, nil
}

// registerRootRedirect attaches GET / → 302 uiPath when the operator
// opted in via OpenAPIConfig.RootRedirect. Called AFTER feature mounts
// and BeforeServe so a service that owns "/" (custom landing page,
// vendor redirect, etc.) wins by registration order — on collision the
// framework logs a slog.Warn and skips so duplicate-route panics never
// reach the operator.
func registerRootRedirect(app *fiber.App, uiPath string, logger *slog.Logger) {
	// GetRoutes(true) filters out routes registered via app.Use(...)
	// so we only see explicit Method+Path registrations. A real GET /
	// route from a feature shows up here; the framework's own
	// middleware stack does not.
	for _, route := range app.GetRoutes(true) {
		if route.Method == fiber.MethodGet && route.Path == "/" {
			logger.Warn("openapi rootRedirect requested but GET / is already registered; skipping",
				"uiPath", uiPath)
			return
		}
	}
	app.Get("/", func(c fiber.Ctx) error {
		return c.Redirect().Status(fiber.StatusFound).To(uiPath)
	})
	logger.Info("openapi root redirect enabled", "from", "/", "to", uiPath)
}

// healthResponse is the JSON shape returned by GET /health. Lives here
// rather than in web/responses so the OpenAPI spec can document the
// route via reflection without consumer services having to declare
// anything.
type healthResponse struct {
	Status string `json:"status"`
}

// authOptionsFromConfig flattens the parsed AuthConfig into the primitives
// the web middleware expects, keeping web/ independent of bootstrap/.
func authOptionsFromConfig(a AuthConfig) fwweb.AuthOptions {
	opts := fwweb.AuthOptions{
		PublicRoutes: a.PublicRoutes,
	}
	if a.JWT != nil {
		opts.Algorithms = a.JWT.Algorithms
		opts.Issuer = a.JWT.Issuer
		opts.Audience = a.JWT.Audience
		opts.LeewaySeconds = a.JWT.LeewaySeconds
		opts.JWKSURL = a.JWT.JWKSURL
		opts.PublicKeyPEM = a.JWT.PublicKeyPEM
	}
	if a.ExternalValidator != nil {
		opts.ExternalValidator = &fwweb.ExternalValidatorOptions{
			Method:         a.ExternalValidator.Method,
			URL:            a.ExternalValidator.URL,
			TokenPlacement: string(a.ExternalValidator.TokenPlacement),
			TokenField:     a.ExternalValidator.TokenField,
			ExtraHeaders:   a.ExternalValidator.ExtraHeaders,
			Success: fwweb.ExternalValidatorSuccess{
				JSONPath:      a.ExternalValidator.Success.JSONPath,
				ExpectedValue: a.ExternalValidator.Success.ExpectedValue,
			},
			TimeoutMS:       a.ExternalValidator.TimeoutMS,
			FailMode:        string(a.ExternalValidator.FailMode),
			CacheTTLSeconds: a.ExternalValidator.CacheTTLSeconds,
		}
	}
	if a.Authorization != nil && a.Authorization.Tenant.Enabled {
		opts.TenantRequired = a.Authorization.Tenant.Required
		opts.TenantClaim = a.Authorization.Tenant.Claim
	}
	return opts
}

func serve(ctx context.Context, deps Deps, wiring Wiring) error {
	app, err := buildApp(deps, wiring)
	if err != nil {
		return err
	}

	// Every subscription declared in YAML must have a registered receiver.
	// Runs BEFORE the IsEmpty short-circuit below so a service that declares
	// integration.subscribes but forgets MountReceivers entirely (registry
	// empty) still aborts the boot instead of silently consuming nothing.
	if deps.Config.Integration != nil {
		if err := integration.ValidateSubscriptionsCovered(
			deps.Config.Integration, deps.IntegrationRegistry.Receivers()); err != nil {
			return fmt.Errorf("bootstrap: %w", err)
		}
	}

	// Start the integration ConsumerPool AFTER Phase Receivers (which
	// ran inside buildApp) so every Receiver is resolved against YAML
	// before any goroutine pulls a message. nil registry / empty
	// receivers makes Start a no-op.
	var consumerPool *integration.ConsumerPool
	if deps.IntegrationRegistry != nil && !deps.IntegrationRegistry.IsEmpty() {
		consumerPool = integration.NewConsumerPool(
			deps.IntegrationRegistry,
			deps.Config.Integration,
			deps.Postgres,
			deps.Config.Kafka.Brokers,
			deps.Pipeline,
			deps.Logger,
		)
		if err := consumerPool.Start(ctx); err != nil {
			return fmt.Errorf("bootstrap: integration consumer pool: %w", err)
		}
	}

	errCh := make(chan error, 1)
	go func() {
		if err := app.Listen(deps.Config.HTTP.Addr, fiber.ListenConfig{
			DisableStartupMessage: true,
		}); err != nil {
			errCh <- err
		}
	}()
	deps.Logger.Info("http listening", "addr", deps.Config.HTTP.Addr)

	select {
	case <-ctx.Done():
		deps.Logger.Info("shutdown signal received, draining...")
	case err := <-errCh:
		return fmt.Errorf("bootstrap: http listen: %w", err)
	}

	drainSeconds := deps.Config.Shutdown.DrainTimeoutSeconds
	if drainSeconds <= 0 {
		drainSeconds = FrameworkDefaultShutdownTimeoutSeconds
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Duration(drainSeconds)*time.Second)
	defer cancel()

	// Coordinated drain: HTTP server + integration consumer pool +
	// upstream subscribers run their drains in parallel under the
	// shared shutdownCtx so one slow drain does not eat the whole
	// timeout budget.
	var drainWG sync.WaitGroup
	drain := func(label string, fn func() error) {
		drainWG.Add(1)
		go func() {
			defer drainWG.Done()
			if err := fn(); err != nil {
				deps.Logger.Warn("drain timeout", "stage", label, "err", err)
			}
		}()
	}

	drain("http", func() error { return app.ShutdownWithContext(shutdownCtx) })
	if consumerPool != nil {
		drain("integration", func() error { return consumerPool.Shutdown(shutdownCtx) })
	}
	for i, sub := range deps.UpstreamSubscribers {
		i, sub := i, sub
		drain(fmt.Sprintf("upstream[%d]", i), func() error { return sub.Shutdown(shutdownCtx) })
	}
	drainWG.Wait()

	if wiring.OnShutdown != nil {
		if err := wiring.OnShutdown(shutdownCtx); err != nil {
			deps.Logger.Warn("OnShutdown hook", "err", err)
		}
	}
	deps.Logger.Info("shutdown complete")
	return nil
}

// collectLanguageOptions walks Wiring.Translations and builds the
// dropdown content for openapi.Config.Languages. Dedup is by
// configuration.Language: a service that registers both the framework's
// CorePTBR and its own apptrans.PTBR collapses to a single "PT_BR" entry
// in the dropdown. Order is preserved by declaration — the first
// translation.Module pinned to a Language wins the slot. Label =
// Language.String() (the framework's canonical label); Value =
// Language.HTTPPrefix() (matches what web.AppContextMiddleware reads
// from the Accept-Language header).
//
// One ordering exception: when LangENG is among the surviving entries,
// it is rotated to position 0 so the dropdown's default-selected
// <option> is English. Declaration order is otherwise preserved.
// English-first as the default is the framework's chosen baseline —
// most operators reading Swagger UI on a shared deployment expect
// English first; services that want a different default override the
// whole slice via openapi.Config.Languages.
func collectLanguageOptions(modules []translation.Module) []openapi.LanguageOption {
	if len(modules) == 0 {
		return nil
	}
	seen := make(map[configuration.Language]bool, len(modules))
	out := make([]openapi.LanguageOption, 0, len(modules))
	for _, m := range modules {
		l := m.Language()
		if seen[l] {
			continue
		}
		seen[l] = true
		prefix := l.HTTPPrefix()
		if prefix == "" {
			// LangUnknown / unmapped languages have no HTTP prefix.
			// Skip rather than render an option with an empty value
			// (would inject Accept-Language: "" — meaningless on the
			// wire and confusing in the dropdown).
			continue
		}
		out = append(out, openapi.LanguageOption{
			Label: l.String(),
			Value: prefix,
		})
	}
	if len(out) == 0 {
		return nil
	}
	// English-first preference: rotate LangENG to position 0 when
	// found. HTML's natural <select> behavior selects the first
	// <option>, so position 0 IS the default.
	engPrefix := configuration.LangENG.HTTPPrefix()
	for i, opt := range out {
		if opt.Value == engPrefix && i > 0 {
			eng := out[i]
			copy(out[1:i+1], out[0:i])
			out[0] = eng
			break
		}
	}
	return out
}

// redact masks the password of a connection URL for logs.
//
//	postgres://user:PASS@host/db → postgres://user:***@host/db
func redact(uri string) string {
	at := strings.LastIndex(uri, "@")
	if at < 0 {
		return uri
	}
	scheme := strings.Index(uri, "://")
	if scheme < 0 || scheme+3 >= at {
		return uri
	}
	userInfo := uri[scheme+3 : at]
	colon := strings.Index(userInfo, ":")
	if colon < 0 {
		return uri
	}
	return uri[:scheme+3] + userInfo[:colon] + ":***" + uri[at:]
}
