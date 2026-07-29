package bootstrap

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"reflect"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/application/translation"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/db/query"
	"github.com/ClaudioSchirmer/omnicore/infra/db/query/engine/mongo"
	"github.com/ClaudioSchirmer/omnicore/infra/events"
	"github.com/ClaudioSchirmer/omnicore/infra/grpcclient"
	"github.com/ClaudioSchirmer/omnicore/infra/httpclient"
	"github.com/ClaudioSchirmer/omnicore/infra/integration"
	"github.com/ClaudioSchirmer/omnicore/infra/tracing"

	fwweb "github.com/ClaudioSchirmer/omnicore/web"
	"github.com/ClaudioSchirmer/omnicore/web/authcore"
	fwgrpc "github.com/ClaudioSchirmer/omnicore/web/grpc"
	"github.com/ClaudioSchirmer/omnicore/web/openapi"

	"connectrpc.com/grpcreflect"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/logger"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c" //nolint:staticcheck // SA1019: h2c is deprecated, but the cleartext-h2c → http.Server.Protocols migration is a separate, QA-gated change (touches the gRPC transport), not a lint-sweep edit.
)

// Run loads microservice.<profile>.yaml + builds singletons + calls wire(deps)
// + registers default middlewares + runs until receiving SIGINT/SIGTERM.
//
// Boot order:
//  1. LoadConfig (reads APP_PROFILE env, loads microservice.<profile>.yaml or
//     $OMNICORE_CONFIG_PATH; rejects auth.mode=disabled outside dev)
//  2. signal.NotifyContext(SIGINT, SIGTERM)
//  3. NewEngine (dialect-selected relational engine) + NewMongoDB (defer Close)
//  4. translation.Default + pipeline.New + SlogAuditor + ViewReader
//  5. wire(deps) → Wiring
//  6. validateWiring (rejects nothing-to-serve; dev accepts the empty shell)
//  7. Translator.Import of the service modules
//  8. Migrations (if cfg.Migrations.AutoRun)
//  9. collectViews (aggregates Views from ReadableFeatures + rejects collision)
//
// 10. CheckServiceRegistry (DB-per-service guard — warn in dev, abort otherwise)
//   - ApplyMongoSpecs (declared indexes / validators / collation / capped /
//     time-series materialized on the Mongo cluster) — skipped when no views
//     11. SyncEngine.Start if views are not empty
//     12. Fiber + Recover/Logger/AppContextMiddleware + AuthMiddleware (when
//     auth.mode=jwt) + the /livez + /readyz probes
//     13. f.Mount(app, deps) for each Feature
//     14. Wiring.BeforeServe(app, deps) if set
//     15. app.Listen in a goroutine
//     16. waits for ctx.Done() → coordinated, dependency-ordered drain (http +
//     integration pool + upstream subscribers + sync engine in parallel, each
//     waiting its own full exit — worker drain, then reader Close/LeaveGroup)
//     → tracing flush → Wiring.OnShutdown → stores close LAST
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
	defer deps.DB.Close()
	defer func() {
		// Bound the Mongo disconnect so a stuck client cannot hang the final
		// close after the coordinated drain already finished.
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		_ = deps.Mongo.Close(closeCtx)
	}()

	wiring := wire(deps)

	if err := validateWiring(wiring, cfg.Profile == profileDev); err != nil {
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

	// Prime the view resolver's pointer cache from the registry now that the
	// schema (including migration 0002's slot columns) is in place. A failure
	// here — e.g. an autoRun=false deployment that has not applied 0002 yet —
	// is non-fatal: the cache stays empty and every view resolves to its bare
	// collection, exactly the pre-blue-green behavior.
	if err := deps.Resolver.Refresh(ctx); err != nil {
		deps.Logger.Warn("view resolver refresh failed; serving bare collections", "err", err)
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
	if mvr, ok := deps.ViewReader.(*mongo.MongoViewReader); ok {
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
	// Composed views are collected HERE, before the subscription guards, because a
	// mirror has TWO kinds of consumer — a view that EMBEDS it and a composed view
	// that LINKS it — and both apply the mirror's soft-delete column. Both must
	// therefore be cross-checked against the subscription's filter (§8.5).
	// Collection is a pure walk over the features; the composition's own
	// validation still runs later, once the upstream collection set is known.
	composedViews, err := collectComposedViews(wiring.Features)
	if err != nil {
		return err
	}
	if len(upstreamSubs) > 0 {
		if err := validateUpstreamSubscriptions(upstreamSubs, views, composedViews, cfg.Profile, deps.Logger); err != nil {
			return err
		}
	}

	// Schema is mandatory on every view — the read membrane (Go↔column) and the
	// composer (ID + soft-delete) resolve through it, so a view without a root
	// schema would have no lossless mapping. Embed schemas are guaranteed by
	// construction (JoinUpstream is the only embed source constructor); the root schema
	// is the one a consumer could forget, so it is enforced here.
	if err := query.ValidateViewSchemas(views); err != nil {
		return err
	}

	// Read-time composition: collect the ComposedViews from every
	// ComposingFeature, validate the whole set (fatal boot on any invalid
	// declaration — R11: never a silent degradation) and install the composed
	// orchestration ON the framework reader (mutation, like SetViews — never a
	// reassignment, so handlers that captured deps.ViewReader before this
	// point, e.g. GraphQL fields registered inside the consumer's Wire(),
	// resolve composed names too). Runs AFTER the upstream subscriptions are
	// resolved (an external leg must name a locally materialized collection).
	if len(composedViews) > 0 {
		if err := query.ValidateComposedViews(composedViews, views, upstreamCollectionSet(upstreamSubs)); err != nil {
			return fmt.Errorf("bootstrap: %w", err)
		}
		mvr, ok := deps.ViewReader.(*mongo.MongoViewReader)
		if !ok {
			return fmt.Errorf(
				"bootstrap: composed view(s) declared but deps.ViewReader is %T — read-time composition requires "+
					"the framework MongoViewReader (a custom ViewReader owns its own read policy and gets no decorator)",
				deps.ViewReader)
		}
		mvr.SetComposedViews(composedViews, cfg.Query.MaxLinkManyLimit)
		deps.Logger.Info("composed views registered", "count", len(composedViews))
	}

	if len(views) > 0 {
		// DB-per-service guard: writes the per-boot marker, scans for
		// foreign collections, warns in dev / aborts otherwise. Runs
		// before ApplyMongoSpecs so a guard failure short-circuits
		// before any write touches the cluster.
		if err := mongo.CheckServiceRegistry(ctx, deps.Mongo, cfg.Service, cfg.Profile, views); err != nil {
			return fmt.Errorf("bootstrap: mongo registry guard: %w", err)
		}

		// Apply declared Mongo specs (indexes, $jsonSchema validator,
		// collation, capped, time-series). Idempotent on steady state;
		// strict-on-divergence by default, FORCE_REBUILD env var as the
		// operator escape for index conflicts.
		if err := mongo.ApplyMongoSpecs(ctx, deps.Mongo, views, deps.Resolver); err != nil {
			return fmt.Errorf("bootstrap: mongo apply specs: %w", err)
		}

		syncEngine := query.NewSyncEngine(deps.DB, deps.Mongo, deps.Resolver,
			deps.Transport, cfg.Transport.SyncGroup, views, cfg.Transport.SyncWorkers).
			WithKafkaTracing(cfg.Observability.Tracing.Resolve(cfg.Service).Instruments(tracing.SubKafka))
		syncEngine.ConfigureParkedRetry(*cfg.Mongo.ParkedRetry.Enabled,
			time.Duration(cfg.Mongo.ParkedRetry.IntervalMinutes)*time.Minute)
		// Surfaced on Deps so serve's coordinated drain can wait for the
		// projection loop's FULL exit (worker drain + reader LeaveGroup)
		// before the stores close — same reason UpstreamSubscribers is there.
		deps.SyncEngine = syncEngine

		// Drift detection + the FAST reconciliation paths run synchronously
		// here (AFTER ApplyMongoSpecs): detection, the unconditional / check-mode
		// aborts (a bad drift still crashes the boot), and the registry-only
		// InitRegistryOnly / RefreshRegistryArtifactOnly paths. It returns the
		// plans that need a full blue-green rebuild — the SLOW path.
		rebuildPlans, rebuildCfg, err := reconcileViewDrift(ctx, cfg, deps, syncEngine, views)
		if err != nil {
			return err
		}

		// Run the slow rebuilds in the background so the HTTP probes come up now:
		// /livez 200 (a long rebuild never gets the pod killed by Kubernetes) and
		// /readyz 503 until the rebuild finishes and the consumer joins. A fatal
		// rebuild error feeds boot.errCh → serve() returns non-zero (parity with
		// the old synchronous boot-abort). The cross-service subscribers and the
		// projection consumer start only AFTER the rebuild — no live event ever
		// reaches a drifted view — so they live in this goroutine too.
		// The goroutine runs on its OWN cancellable context (a child of ctx, so a
		// shutdown signal still cancels it). serve() cancels it via boot.cancel on
		// any early exit and waits for boot.done, so the stores never close under an
		// in-flight rebuild/consumer. defer keeps `go vet` happy and cancels on the
		// normal return path too.
		rebuildCtx, rebuildCancel := context.WithCancel(ctx)
		defer rebuildCancel()
		boot := &bootRebuild{done: make(chan struct{}), errCh: make(chan error, 1), cancel: rebuildCancel, total: len(rebuildPlans)}
		deps.bootRebuild = boot
		go func() {
			defer close(boot.done)
			// notRebuilt names the views this run did NOT bring to a flip (a
			// follower's skip). A view materializing one of them must be skipped
			// too: rebuilding it now would compose its segments from the source's
			// pre-flip content and finish stale, with no event left to repair it.
			// The driver instance holds both plans in the same dependency order and
			// rebuilds the pair correctly, so skipping here converges.
			notRebuilt := map[string]bool{}
			for i, plan := range rebuildPlans {
				if src := blockedEmbedSource(plan.View, notRebuilt); src != "" {
					notRebuilt[plan.View.Name()] = true
					deps.Logger.Info("view rebuild deferred: it materializes a view this instance did not rebuild",
						"view", plan.View.Name(), "source", src)
					continue
				}
				// Record which view (1-based) is rebuilding so /readyz names it in
				// the 503 reason. Set before ExecuteRebuild so the reason reflects
				// the view currently under work, not the one just finished.
				boot.progress.Store(&rebuildProgress{view: plan.View.Name(), index: i + 1})
				if err := syncEngine.ExecuteRebuild(rebuildCtx, plan, rebuildCfg); err != nil {
					if rebuildCtx.Err() != nil {
						return // shutting down / boot aborting mid-rebuild — not a failure
					}
					if errors.Is(err, query.ErrRebuildLockHeld) {
						// Follower: another live instance is driving this view's
						// rebuild. Do NOT abort — serve the current active slot and
						// pick up the driver's flip at runtime via the resolver's
						// lease refresh. Keep going for any other view we can drive.
						deps.Logger.Info("view rebuild driven by another instance; serving the active slot until the flip",
							"view", plan.View.Name())
						notRebuilt[plan.View.Name()] = true
						continue
					}
					deps.Logger.Error("boot rebuild failed", "view", plan.View.Name(), "err", err)
					select {
					case boot.errCh <- fmt.Errorf("bootstrap: rebuild view %q: %w", plan.View.Name(), err):
					default:
					}
					return
				}
			}
			boot.upstream = startUpstreamSubscribers(rebuildCtx, deps, cfg, upstreamSubs, views, syncEngine)
			syncEngine.Start(rebuildCtx)
			boot.complete.Store(true) // readiness gate opens
			deps.Logger.Info("sync engine started",
				"endpoints", cfg.Transport.Endpoints,
				"syncGroup", cfg.Transport.SyncGroup,
				"views", len(views),
				"workers", cfg.Transport.SyncWorkers)
			// The scheduled revision-parity sweep — opt-in via mongo.reconcile.
			// Started only after the boot rebuilds finished (a view mid-rebuild
			// is skipped by the pass anyway, but there is no reason to spin the
			// loop before the read model exists).
			if cfg.Mongo.Reconcile.Enabled {
				go syncEngine.RunReconcileLoop(rebuildCtx,
					time.Duration(cfg.Mongo.Reconcile.IntervalMinutes)*time.Minute,
					query.ReconcileConfig{
						RowsPerSecond: cfg.Mongo.Reconcile.RowsPerSecond,
						BatchSize:     cfg.Mongo.Reconcile.BatchSize,
					})
				deps.Logger.Info("projection reconcile loop started",
					"intervalMinutes", cfg.Mongo.Reconcile.IntervalMinutes,
					"rowsPerSecond", cfg.Mongo.Reconcile.RowsPerSecond)
			}
		}()
	} else if len(upstreamSubs) > 0 {
		// Degenerate case: B declared upstream subscriptions but no
		// local views. The subscribers still materialize the local
		// Mongo collection (operator may consume via mongosh or a
		// custom adapter); the recompose-ripple is a no-op since no
		// view embeds the collection.
		deps.UpstreamSubscribers = startUpstreamSubscribers(ctx, deps, cfg, upstreamSubs, views, nil)
	}

	return serve(ctx, deps, wiring)
}

func buildDeps(cfg *Config) (Deps, error) {
	ctx := context.Background()

	// Tracing is installed first so the logger can be wrapped to stamp
	// traceId/spanId onto context-carrying records, and so the relational
	// engine / Mongo constructors (instrumentation wired in later stages) observe the
	// installed globals. Inert + free when observability.tracing is off.
	tracingCfg := cfg.Observability.Tracing.Resolve(cfg.Service)
	tracingProvider, err := tracing.Setup(ctx, tracingCfg)
	if err != nil {
		return Deps{}, fmt.Errorf("bootstrap: tracing setup: %w", err)
	}

	var logHandler slog.Handler = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})
	if tracingCfg.Enabled {
		logHandler = tracing.NewSlogHandler(logHandler)
	}
	logger := slog.New(logHandler)
	slog.SetDefault(logger)
	if tracingCfg.Enabled {
		logger.Info("tracing enabled",
			"exporter", string(tracingCfg.Exporter),
			"sampler", string(tracingCfg.Sampler),
			"endpoint", tracingCfg.Endpoint)
	}

	eng, err := core.NewEngine(cfg.Relational.Dialect, ctx, core.EngineConfig{
		DSN:     cfg.Relational.DSN,
		Tracing: tracingCfg.Instruments(tracing.SubPgx),
		Pool: core.PoolConfig{
			MaxOpenConns:    *cfg.Relational.Pool.MaxOpenConns,
			MaxIdleConns:    *cfg.Relational.Pool.MaxIdleConns,
			ConnMaxLifetime: time.Duration(*cfg.Relational.Pool.ConnMaxLifetimeSeconds) * time.Second,
		},
	})
	if err != nil {
		return Deps{}, fmt.Errorf("bootstrap: database connect: %w", err)
	}
	logger.Info("database connected", "dialect", cfg.Relational.Dialect, "dsn", redact(cfg.Relational.DSN))

	mg, err := mongo.NewMongoDB(ctx, cfg.Mongo.URI, cfg.Mongo.Database,
		mongo.WithMongoTracing(tracingCfg.Instruments(tracing.SubMongo)))
	if err != nil {
		eng.Close()
		return Deps{}, fmt.Errorf("bootstrap: mongo connect: %w", err)
	}
	logger.Info("mongo connected", "uri", redact(cfg.Mongo.URI), "db", cfg.Mongo.Database)

	tr := translation.Default()
	pipe := pipeline.New(tr).WithLogger(logger)
	// Audit + domain-event publishing are configured on the neutral engine: once
	// set, every write emits the configured audit destinations (in-TX audit_events
	// row + post-commit slog echo) and forwards accumulated domain events
	// post-commit. Each backend writes the audit row through its own dialect
	// (Postgres via pgx, MySQL via database/sql) — the config surface is neutral, so
	// no dialect branch here. nil cfg.Audit destinations were populated by
	// applyDefaults already.
	eng.WithAudit(&cfg.Audit, logger, cfg.Auth.AuditClaims)
	eng.WithEventPublisher(events.NewSlogPublisher(logger))
	// resolver maps every view to the physical collection it currently serves
	// (its active slot). Constructed once here and shared by every read-model
	// component (the reader, SyncEngine, composer, upstream subscribers, drift
	// detection) so they observe one consistent pointer. eng backs Refresh (the
	// registry read); until the first flip every view resolves to its bare name.
	resolver := query.NewViewResolverWithLease(eng, time.Duration(cfg.Mongo.Rebuild.PointerLeaseSeconds)*time.Second)
	viewReader := mongo.NewMongoViewReader(mg, resolver)

	// Resolve the SERVICE-PRIVATE cache from cfg only (no Wire
	// injection at this stage). If cfg.Cache.Store == "custom", the
	// resolution returns nil + error pointing operator to the
	// Build+Serve flow with Wiring.Cache. memory / redis backends
	// resolve directly.
	privateCache, err := resolveCache(cfg.Cache, nil)
	if err != nil {
		eng.Close()
		_ = mg.Close(context.Background())
		return Deps{}, fmt.Errorf("bootstrap: cache init: %w", err)
	}
	sharedCache, err := resolveSharedCache(cfg.Cache, nil)
	if err != nil {
		eng.Close()
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
			httpclient.WithCache(privateCache),
			httpclient.WithClientTracing(tracingCfg.Instruments(tracing.SubHTTPClient)))
		if err != nil {
			eng.Close()
			_ = mg.Close(context.Background())
			return Deps{}, fmt.Errorf("bootstrap: httpclient init: %w", err)
		}
		logger.Info("httpclient configured")
	}

	var gc *grpcclient.Client
	if cfg.GRPCClient != nil {
		gc, err = grpcclient.New(cfg.GRPCClient,
			grpcclient.WithClientTracing(tracingCfg.Instruments(tracing.SubHTTPClient)))
		if err != nil {
			eng.Close()
			_ = mg.Close(context.Background())
			return Deps{}, fmt.Errorf("bootstrap: grpcclient init: %w", err)
		}
		logger.Info("grpcclient configured", "services", len(cfg.GRPCClient.Services))
	}

	// Integration registry is constructed unconditionally so consumer
	// features can stash the pointer from their constructor; Configure
	// makes the producer-side singleton available BEFORE feature mounts
	// so a feature's BeforeCommit closure can reference fwintegration.Dispatch
	// without nil-checking. Services that emit nothing AND consume
	// nothing pay only the empty struct cost.
	//
	// Configured on the neutral engine: the producer's in-TX path uses the
	// canonical core.UnwrapTx bridge and the standalone path the engine's
	// Querier, so Dispatch works on any dialect (Postgres or MySQL).
	integration.Configure(cfg.Integration, eng, logger)
	integrationRegistry := integration.NewRegistry()

	// Build the message-transport subscriber from the linked adapter (selected
	// by the transport build tag — see the transport_<tag>.go bindings). Every
	// async consumer (SyncEngine, integration ConsumerPool, each
	// UpstreamSubscriber) shares this one port. The adapter carries connection
	// settings only and dials lazily at Subscribe / EnsureTopics, so a service
	// that ends up doing no messaging pays nothing here.
	sub, err := newTransportSubscriber(cfg)
	if err != nil {
		eng.Close()
		_ = mg.Close(context.Background())
		return Deps{}, fmt.Errorf("bootstrap: transport init: %w", err)
	}

	return Deps{
		Config:              cfg,
		Logger:              logger,
		Tracing:             tracingProvider,
		DB:                  eng,
		Mongo:               mg,
		Translator:          tr,
		Pipeline:            pipe,
		ViewReader:          viewReader,
		Resolver:            resolver,
		Transport:           sub,
		Export:              fwweb.ExportDeps{Translator: tr, MaxExportRows: cfg.Query.MaxExportRows},
		Cache:               privateCache,
		SharedCache:         sharedCache,
		HttpClient:          hc,
		GRPCClient:          gc,
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

// buildApp assembles the *fiber.App with default middlewares, the /livez +
// /readyz probes provided by the framework, and the service features. The ctx is
// the shutdown context (signal.NotifyContext) the readiness probe watches to flip
// to draining. Does not call Listen — extracted from serve for testability via
// app.Test without networking.
func buildApp(ctx context.Context, deps Deps, wiring Wiring) (*fiber.App, error) {
	// Register the framework's translator-backed gate + standalone
	// translator BEFORE any Mount/MountRaw call (including the probes below
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

	fiberCfg := fiber.Config{
		AppName:      deps.Config.Service,
		ErrorHandler: fwweb.ErrorHandler(deps.Pipeline),
	}
	// Optional HTTP hardening knobs — each left at Fiber's default when unset.
	// BodyLimit rejects an oversized body with 413; ReadTimeout surfaces as 408
	// (both rendered by the ErrorHandler); IdleTimeout silently closes the idle
	// keep-alive with no response. All three enforced at the fasthttp layer.
	if v := deps.Config.HTTP.BodyLimitBytes; v != nil {
		fiberCfg.BodyLimit = *v
	}
	if v := deps.Config.HTTP.ReadTimeoutSeconds; v != nil {
		fiberCfg.ReadTimeout = time.Duration(*v) * time.Second
	}
	if v := deps.Config.HTTP.IdleTimeoutSeconds; v != nil {
		fiberCfg.IdleTimeout = time.Duration(*v) * time.Second
	}
	app := fiber.New(fiberCfg)

	app.Use(fwweb.Recover())
	app.Use(logger.New())
	// The inbound server span is gated by the tracing `http` instrument toggle;
	// off (or tracing disabled) → the middleware builds only the AppContext.
	traceHTTP := deps.Config.Observability.Tracing.Resolve(deps.Config.Service).Instruments(tracing.SubHTTP)
	reqTimeoutSecs := FrameworkDefaultRequestTimeoutSeconds
	if deps.Config.HTTP.RequestTimeoutSeconds != nil {
		reqTimeoutSecs = *deps.Config.HTTP.RequestTimeoutSeconds
	}
	app.Use(fwweb.AppContextMiddleware(
		fwweb.WithServerSpanTracing(traceHTTP),
		fwweb.WithRequestTimeout(time.Duration(reqTimeoutSecs)*time.Second),
	))

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

	// Liveness (GET /livez) — deliberately dumb: static, no dependency checks.
	// It answers "is the process wedged?"; failing it makes Kubernetes RESTART
	// the pod, and a restart never cures a dependency outage. Checking a store
	// here would turn a DB blip into a restart storm across every replica.
	openapi.MountRaw(deps.OpenAPIRegistry, app, fiber.MethodGet, "/livez",
		func(c fiber.Ctx) error {
			return c.JSON(healthResponse{Status: "ok"})
		},
		openapi.RawSpec{
			Summary: "Liveness probe",
			Tags:    []string{"Health"},
			Public:  true,
			Responses: map[int]openapi.ResponseSpec{
				200: {
					Description: "Process is up",
					Type:        reflect.TypeOf(healthResponse{}),
				},
			},
		})

	// Readiness (GET /readyz) — answers "can this pod take traffic now?". Failing
	// it makes Kubernetes REMOVE the pod from the load balancer (not restart it):
	// 503 while draining (SIGTERM received) or while a request-path store is
	// unreachable, 200 otherwise. The broker is excluded on purpose (see the
	// readiness type doc). Like /livez it is framework-owned but NOT auto-public;
	// the operator lists "GET /readyz" in auth.publicRoutes so a tokenless kubelet
	// can reach it.
	ready := &readiness{shutdown: ctx, db: deps.DB, mongo: deps.Mongo, boot: deps.bootRebuild}
	openapi.MountRaw(deps.OpenAPIRegistry, app, fiber.MethodGet, "/readyz",
		func(c fiber.Ctx) error {
			if err := ready.check(c.Context()); err != nil {
				return c.Status(fiber.StatusServiceUnavailable).
					JSON(healthResponse{Status: "unavailable", Reason: err.Error()})
			}
			return c.JSON(healthResponse{Status: "ready"})
		},
		openapi.RawSpec{
			Summary: "Readiness probe",
			Tags:    []string{"Health"},
			Public:  true,
			Responses: map[int]openapi.ResponseSpec{
				200: {
					Description: "Ready to take traffic",
					Type:        reflect.TypeOf(healthResponse{}),
				},
				503: {
					Description: "Draining or a request-path store is unreachable",
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
		gqlCfg := deps.Config.GraphQL
		// Only one surface can own GET /. When both the OpenAPI UI and the
		// GraphQL endpoint are wired AND both opt into rootRedirect, the boot
		// is structurally ambiguous — fail loud rather than let one silently
		// win the registration race.
		if gqlCfg.RootRedirect && wiring.OpenAPI != nil && deps.Config.OpenAPI.RootRedirect {
			panic("bootstrap: openapi.rootRedirect and graphql.rootRedirect are both enabled — only one surface can own GET /; disable one")
		}
		wiring.GraphQL.EnableIntrospection(gqlCfg.Introspection)
		// Layer-1 permission gate master switch — same source as the REST gate
		// (fwweb.SetAuthorizationEnabled above), so RequirePermission on a
		// GraphQL field enforces under auth.authorization.enabled and stays
		// inert otherwise (incremental-rollout parity).
		wiring.GraphQL.EnableAuthorization(deps.Config.Auth.Authorization != nil && deps.Config.Auth.Authorization.Enabled)
		app.Post(gqlCfg.Path, wiring.GraphQL.Handler())
		deps.Logger.Info("graphql served", "path", gqlCfg.Path,
			"introspection", gqlCfg.Introspection, "playground", gqlCfg.Playground)
		if gqlCfg.Playground {
			app.Get(gqlCfg.UIPath, wiring.GraphQL.Playground(gqlCfg.Path))
			deps.Logger.Info("graphql playground served", "ui", gqlCfg.UIPath)
		}
		if gqlCfg.RootRedirect {
			registerRootRedirect(app, gqlCfg.Path, deps.Logger)
		}
	}

	// The gRPC surface (Wiring.GRPC) configures here — policy injection
	// from the yaml `grpc:` block — and is SERVED in serve() on its own
	// dedicated listener (Fiber/fasthttp cannot host HTTP/2 services).
	// Auth rides the SAME JWT core the HTTP middleware uses (web/authcore
	// via fwweb.NewAuthCoreValidator): one validation, two transport shells.
	if wiring.GRPC != nil {
		grpcCfg := deps.Config.GRPC
		switch grpcCfg.Auth.Mode {
		case "internal", "mtls":
			// The trusted internal plane. The attribution validator is a
			// SEPARATE authcore construction WITHOUT the external checker —
			// the two structural guarantees: expiry tolerance never reaches
			// the main door (the edge keeps its own strict validator), and
			// attribution is local cached-JWKS crypto that never calls the
			// IdP. Absent JWT material (auth disabled — dev), forwarded
			// bearers cannot be verified and every call proceeds anonymous.
			var attribution *authcore.Validator
			if deps.Config.Auth.JWT != nil {
				attrOpts := authOptionsFromConfig(deps.Config.Auth)
				attrOpts.ExternalValidator = nil
				var err error
				attribution, err = fwweb.NewAuthCoreValidator(attrOpts)
				if err != nil {
					return nil, fmt.Errorf("bootstrap: grpc attribution validator: %w", err)
				}
			}
			posture := fwgrpc.PostureInternal
			if grpcCfg.Auth.Mode == "mtls" {
				posture = fwgrpc.PostureMTLS
			}
			wiring.GRPC.SetAuthPosture(posture, attribution)
			deps.Logger.Info("grpc auth posture", "mode", grpcCfg.Auth.Mode, "attribution", attribution != nil)
		default: // inherit — the global auth: block governs, like HTTP
			if deps.Config.Auth.Mode == AuthModeJWT {
				authOpts := authOptionsFromConfig(deps.Config.Auth)
				validator, err := fwweb.NewAuthCoreValidator(authOpts)
				if err != nil {
					return nil, fmt.Errorf("bootstrap: grpc auth: %w", err)
				}
				wiring.GRPC.EnableAuth(validator, fwgrpc.AuthPolicy{
					PublicProcedures: grpcCfg.PublicProcedures,
					TenantRequired:   authOpts.TenantRequired,
				})
				deps.Logger.Info("grpc auth enabled", "issuer", deps.Config.Auth.JWT.Issuer, "publicProcedures", len(grpcCfg.PublicProcedures))
			}
		}
		// Layer-1 permission gate — same master switch as the REST gate
		// (fwweb.SetAuthorizationEnabled) and the GraphQL registry, so
		// RequirePermission on a procedure enforces under
		// auth.authorization.enabled and stays inert otherwise.
		wiring.GRPC.EnableAuthorization(deps.Config.Auth.Authorization != nil && deps.Config.Auth.Authorization.Enabled)
		// The `http` tracing instrument gates inbound server spans on BOTH
		// listeners — the gRPC surface is inbound traffic of the same kind.
		wiring.GRPC.EnableServerSpanTracing(
			deps.Config.Observability.Tracing.Resolve(deps.Config.Service).Instruments(tracing.SubHTTP))
		if grpcCfg.RequestTimeoutSeconds > 0 {
			wiring.GRPC.SetRequestTimeout(time.Duration(grpcCfg.RequestTimeoutSeconds) * time.Second)
		}
		if grpcCfg.Reflection {
			reflector := grpcreflect.NewStaticReflector(wiring.GRPC.ServiceNames()...)
			wiring.GRPC.MountRaw(grpcreflect.NewHandlerV1(reflector))
			wiring.GRPC.MountRaw(grpcreflect.NewHandlerV1Alpha(reflector))
			deps.Logger.Info("grpc reflection enabled", "services", wiring.GRPC.ServiceNames())
		}
	}

	// Validate the operator-declared auth.publicRoutes against the fully
	// registered route set (features + /livez + /readyz + the OpenAPI spec/UI + the
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

// healthResponse is the JSON shape returned by the GET /livez and GET /readyz
// probes. Lives here rather than in web/responses so the OpenAPI spec can
// document the routes via reflection without consumer services having to declare
// anything. Reason is populated only on a not-ready readiness response, so an
// operator who curls /readyz sees why the pod is out of rotation.
type healthResponse struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// readinessProbeTimeout bounds the per-store ping the readiness probe runs, so a
// wedged relational backend or Mongo fails the probe fast instead of hanging the
// kubelet's request. Probes fire every few seconds; this ceiling is generous.
const readinessProbeTimeout = 2 * time.Second

// readiness backs GET /readyz. It reports whether the service can take traffic:
// ready only when the request-path stores answer AND the process is not draining.
//
//   - draining — derived from the shutdown context (the same signal.NotifyContext
//     that governs the coordinated drain). The moment SIGTERM lands the context is
//     cancelled, /readyz flips to 503, and Kubernetes removes the pod from the load
//     balancer while the in-flight requests finish draining. This is the missing
//     half of the graceful shutdown: the drain already existed, but nothing told
//     the balancer to stop sending new traffic.
//   - stores — a cheap relational SELECT 1 (dialect-neutral) plus a Mongo ping,
//     the two request-path dependencies. The message transport is deliberately
//     EXCLUDED: the outbox decouples writes from the broker (a write still commits
//     and reads still serve when the broker is down), so a broker outage must not
//     pull the pod from the balancer. Async-consumer health is an alerting concern,
//     not readiness.
type readiness struct {
	shutdown context.Context
	db       core.RelationalEngine
	mongo    *mongo.MongoDB
	// boot gates readiness on the background boot-time view rebuild: while it
	// runs, /readyz returns 503 so the pod stays out of rotation (but /livez is
	// up, so a long rebuild is not killed). Nil when there is no boot rebuild.
	boot *bootRebuild
}

// bootRebuild coordinates the background boot-time view rebuild with the HTTP
// probes and the shutdown drain. The slow blue-green backfill runs in a
// goroutine so /livez comes up immediately (Kubernetes never kills a pod through
// a long rebuild); /readyz stays 503 until complete flips true. A fatal rebuild
// error is sent on errCh so serve() returns non-zero — parity with the old
// synchronous boot-abort. done closes when the goroutine exits (success or a
// shutdown stop), so the drain reads upstream only once it is final. upstream
// carries the subscribers the goroutine starts after the rebuild (deps is passed
// to serve by value, so the goroutine cannot hand them over through deps).
type bootRebuild struct {
	done     chan struct{}
	errCh    chan error
	complete atomic.Bool
	upstream []*query.UpstreamSubscriber
	// cancel stops the goroutine's own context so an early serve() exit (a
	// listener that failed to bind, a fatal rebuild) can abort an in-flight
	// rebuild/consumer and wait for it to unwind BEFORE the stores close.
	cancel context.CancelFunc
	// total is the number of views the goroutine will rebuild; progress carries
	// the one it is on right now. Both feed the /readyz 503 reason so an operator
	// who curls the probe sees WHICH view is rebuilding and how far the run is.
	// progress is written by the rebuild goroutine and read by the probe goroutine
	// (readiness.check), so it is an atomic pointer; nil until the first view
	// starts (the reconcile window falls back to the generic reason).
	total    int
	progress atomic.Pointer[rebuildProgress]
}

// rebuildProgress is the snapshot readiness.check renders into the /readyz reason
// while a boot rebuild runs: the view being rebuilt and its 1-based position in
// the run. view and index update together (one atomic Store of a fresh value), so
// the probe never reads a torn pair.
type rebuildProgress struct {
	view  string
	index int
}

// bootRebuildStopGrace bounds how long serve() waits for the background boot
// rebuild to unwind after cancelling it, so a wedged rebuild that ignores its
// context cannot delay a boot-failure exit forever. A var so tests can shrink it.
var bootRebuildStopGrace = 5 * time.Second

// stopBootRebuild aborts the background boot-rebuild goroutine and waits (bounded)
// for it to exit. serve() defers it so that EVERY exit path — a listener bind
// failure, a fatal rebuild, any early return — unwinds the goroutine before
// runWithConfig's deferred store closes run. Without it those closes race the
// still-running rebuild/consumer and surface a misleading "client is disconnected"
// that masks the real cause (e.g. "bind: address already in use"). No-op when
// there is no boot rebuild, and effectively instant on the graceful-drain path
// (that path has already awaited boot.done, so the channel is closed).
func stopBootRebuild(deps Deps) {
	if deps.bootRebuild == nil {
		return
	}
	if deps.bootRebuild.cancel != nil {
		deps.bootRebuild.cancel()
	}
	select {
	case <-deps.bootRebuild.done:
	case <-time.After(bootRebuildStopGrace):
	}
}

// check returns nil when the service can serve, or an error naming the reason it
// cannot (draining, a rebuild in progress, or a store that failed to answer).
func (r *readiness) check(ctx context.Context) error {
	if r.shutdown != nil && r.shutdown.Err() != nil {
		return errors.New("draining")
	}
	if r.boot != nil && !r.boot.complete.Load() {
		// Name the view under rebuild + its position when the goroutine has
		// started one; the generic reason covers the window before the first view
		// (drift reconcile) where no progress is recorded yet.
		if p := r.boot.progress.Load(); p != nil {
			return fmt.Errorf("initializing: rebuilding view %q (%d/%d)", p.view, p.index, r.boot.total)
		}
		return errors.New("initializing: view rebuild in progress")
	}
	ctx, cancel := context.WithTimeout(ctx, readinessProbeTimeout)
	defer cancel()
	if r.db != nil {
		var one int
		if err := r.db.Querier().QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
			return fmt.Errorf("relational: %w", err)
		}
	}
	if r.mongo != nil {
		if err := r.mongo.Ping(ctx); err != nil {
			return fmt.Errorf("mongo: %w", err)
		}
	}
	return nil
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
	// Whatever ends serve() — a listener that failed to bind, a fatal boot
	// rebuild, any early return, or the graceful drain below — the background
	// boot-rebuild goroutine must be unwound before runWithConfig's deferred
	// store closes run, or those closes race an in-flight rebuild/consumer and
	// surface a misleading "client is disconnected". On the drain path this is a
	// no-op (boot.done already awaited there); on every error path it does the work.
	defer stopBootRebuild(deps)

	app, err := buildApp(ctx, deps, wiring)
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
			deps.DB,
			deps.Transport,
			deps.Pipeline,
			deps.Logger,
		).WithKafkaTracing(deps.Config.Observability.Tracing.Resolve(deps.Config.Service).Instruments(tracing.SubKafka))
		if err := consumerPool.Start(ctx); err != nil {
			return fmt.Errorf("bootstrap: integration consumer pool: %w", err)
		}
	}

	errCh := make(chan error, 2)
	go func() {
		if err := app.Listen(deps.Config.HTTP.Addr, fiber.ListenConfig{
			DisableStartupMessage: true,
		}); err != nil {
			errCh <- fmt.Errorf("http listen: %w", err)
		}
	}()
	deps.Logger.Info("http listening", "addr", deps.Config.HTTP.Addr)

	// The gRPC surface gets its own net/http listener: with TLS the server
	// negotiates HTTP/2 natively; without it the handler is wrapped in h2c
	// so the gRPC protocol works in dev. Serving errors join the same errCh
	// as the Fiber listener — either listener failing aborts the boot.
	var grpcSrv *http.Server
	if wiring.GRPC != nil {
		grpcCfg := deps.Config.GRPC
		grpcTLS := grpcCfg.CertFile != ""
		handler := wiring.GRPC.Handler()
		var grpcTLSConfig *tls.Config
		if grpcCfg.Auth.Mode == "mtls" {
			// Require + verify a client certificate from the internal CA on
			// every connection, and lift the verified certificate's identity
			// into the request context so the anonymous internal call is
			// attributed to the calling SERVICE.
			caPEM, err := os.ReadFile(grpcCfg.ClientCAFile)
			if err != nil {
				return fmt.Errorf("bootstrap: grpc clientCAFile: %w", err)
			}
			caPool := x509.NewCertPool()
			if !caPool.AppendCertsFromPEM(caPEM) {
				return fmt.Errorf("bootstrap: grpc clientCAFile %q carries no usable CA certificate", grpcCfg.ClientCAFile)
			}
			grpcTLSConfig = &tls.Config{
				ClientCAs:  caPool,
				ClientAuth: tls.RequireAndVerifyClientCert,
			}
			handler = fwgrpc.WithClientCertIdentity(handler)
		}
		if !grpcTLS {
			handler = h2c.NewHandler(handler, &http2.Server{}) //nolint:staticcheck // SA1019: see the h2c import note.
		}
		grpcSrv = &http.Server{Addr: grpcCfg.Addr, Handler: handler, TLSConfig: grpcTLSConfig}
		if grpcCfg.IdleTimeoutSeconds > 0 {
			// The producer-side LB lever: recycle idle keep-alives so
			// callers re-dial through kube-proxy and traffic redistributes.
			grpcSrv.IdleTimeout = time.Duration(grpcCfg.IdleTimeoutSeconds) * time.Second
		}
		go func() {
			var err error
			if grpcTLS {
				err = grpcSrv.ListenAndServeTLS(grpcCfg.CertFile, grpcCfg.KeyFile)
			} else {
				err = grpcSrv.ListenAndServe()
			}
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("grpc listen: %w", err)
			}
		}()
		deps.Logger.Info("grpc listening", "addr", grpcCfg.Addr, "tls", grpcTLS)
	}

	// A fatal background boot-rebuild error surfaces here so the process exits
	// non-zero — exactly the old synchronous boot-abort. Nil channel (no boot
	// rebuild) is never selected.
	var bootErrCh <-chan error
	if deps.bootRebuild != nil {
		bootErrCh = deps.bootRebuild.errCh
	}
	select {
	case <-ctx.Done():
		deps.Logger.Info("shutdown signal received, draining...")
	case err := <-errCh:
		return fmt.Errorf("bootstrap: %w", err)
	case err := <-bootErrCh:
		return err
	}

	drainSeconds := deps.Config.Shutdown.DrainTimeoutSeconds
	if drainSeconds <= 0 {
		drainSeconds = FrameworkDefaultShutdownTimeoutSeconds
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Duration(drainSeconds)*time.Second)
	defer cancel()

	// Hard backstop. A context is COOPERATIVE — it cannot interrupt a stage that
	// ignores it (a hook blocking on I/O without ctx, a stuck close). The watchdog
	// guarantees the process exits at drain + grace no matter what, turning a
	// hang-to-SIGKILL into a bounded, logged force-exit. A SECOND signal is the
	// operator's "stop waiting, exit NOW". A negative HardGraceSeconds opts out
	// (the embedder owns the process lifecycle).
	graceSeconds := deps.Config.Shutdown.HardGraceSeconds
	if graceSeconds == 0 { // re-default inline (serve may run without applyDefaults)
		graceSeconds = FrameworkDefaultHardGraceSeconds
	}
	if graceSeconds >= 0 {
		watchdog := time.AfterFunc(time.Duration(drainSeconds+graceSeconds)*time.Second, func() {
			deps.Logger.Error("shutdown exceeded the hard deadline — forcing exit",
				"drainSeconds", drainSeconds, "hardGraceSeconds", graceSeconds)
			os.Exit(1)
		})
		defer watchdog.Stop()

		forceCh := make(chan os.Signal, 1)
		signal.Notify(forceCh, syscall.SIGINT, syscall.SIGTERM)
		defer signal.Stop(forceCh)
		go func() {
			if _, ok := <-forceCh; ok {
				deps.Logger.Warn("second shutdown signal — forcing immediate exit")
				os.Exit(1)
			}
		}()
	}

	// Coordinated drain: HTTP server + integration consumer pool +
	// upstream subscribers run their drains in parallel under the
	// shared shutdownCtx so one slow drain does not eat the whole
	// timeout budget.
	var drainWG sync.WaitGroup
	drain := func(label string, fn func() error) {
		drainWG.Add(1)
		go func() {
			defer drainWG.Done()
			deps.Logger.Info("draining", "stage", label)
			start := time.Now()
			if err := fn(); err != nil {
				deps.Logger.Warn("drain failed", "stage", label, "err", err, "elapsed", time.Since(start))
				return
			}
			deps.Logger.Info("drained", "stage", label, "elapsed", time.Since(start))
		}()
	}

	drain("http", func() error { return app.ShutdownWithContext(shutdownCtx) })
	if grpcSrv != nil {
		drain("grpc", func() error {
			err := grpcSrv.Shutdown(shutdownCtx)
			if errors.Is(err, context.DeadlineExceeded) {
				// Graceful window elapsed — force listener + lingering conns closed
				// so a stuck client cannot outlive the drain. (Fiber v3 exposes no
				// equivalent hard close; the watchdog is its backstop.)
				_ = grpcSrv.Close()
			}
			return err
		})
	}
	if consumerPool != nil {
		drain("integration", func() error { return consumerPool.Shutdown(shutdownCtx) })
	}
	// Wait for the background boot rebuild goroutine to stop before draining the
	// consumers it starts (ctx is cancelled, so a rebuild in flight exits
	// promptly). This makes boot.upstream + the started SyncEngine final — no
	// race between the goroutine's Start and the drain's Shutdown. Bounded by
	// shutdownCtx so a wedged rebuild cannot eat the whole budget.
	if deps.bootRebuild != nil {
		select {
		case <-deps.bootRebuild.done:
		case <-shutdownCtx.Done():
			deps.Logger.Warn("boot rebuild did not stop before the drain deadline")
		}
		for i, sub := range deps.bootRebuild.upstream {
			i, sub := i, sub
			drain(fmt.Sprintf("upstream[%d]", i), func() error { return sub.Shutdown(shutdownCtx) })
		}
	}
	// The degenerate no-views case starts its subscribers synchronously.
	for i, sub := range deps.UpstreamSubscribers {
		i, sub := i, sub
		drain(fmt.Sprintf("upstream[%d]", i), func() error { return sub.Shutdown(shutdownCtx) })
	}
	// The SyncEngine drains like the other consumers: Shutdown unblocks only
	// after the projection loop fully exited — worker drain (every in-flight
	// compose+upsert FINISHED) then reader Close() (the Kafka LeaveGroup) —
	// so the stores never close under it and the next boot never joins the
	// group against a ghost member. Nil-safe when the service has no views.
	drain("sync", func() error { return deps.SyncEngine.Shutdown(shutdownCtx) })
	drainWG.Wait()

	// Flush buffered spans AFTER the servers stopped accepting work, so the
	// final in-flight requests' spans reach the collector. Nil-safe + no-op on
	// the disabled path.
	// Telemetry flush on its OWN short budget: a dead/slow OTLP collector must
	// never consume the whole drain window — running last, it would otherwise
	// inherit whatever the fast parallel drains left (up to the full budget).
	// Spans are best-effort; losing a few on a dead collector beats hanging.
	deps.Logger.Info("draining", "stage", "tracing")
	tracingStart := time.Now()
	tracingSeconds := deps.Config.Shutdown.TracingDrainSeconds
	if tracingSeconds <= 0 { // re-default inline (serve may run without applyDefaults)
		tracingSeconds = FrameworkDefaultTracingDrainSeconds
	}
	traceCtx, traceCancel := context.WithTimeout(shutdownCtx, time.Duration(tracingSeconds)*time.Second)
	if err := deps.Tracing.Shutdown(traceCtx); err != nil {
		deps.Logger.Warn("drain failed", "stage", "tracing", "err", err, "elapsed", time.Since(tracingStart))
	} else {
		deps.Logger.Info("drained", "stage", "tracing", "elapsed", time.Since(tracingStart))
	}
	traceCancel()

	// The user hook runs UNDER the drain budget but is RACED against it: a hook
	// that ignores ctx and blocks can no longer hang the drain (the watchdog is
	// the final backstop). Returning promptly lets the deferred store closes run.
	if wiring.OnShutdown != nil {
		deps.Logger.Info("draining", "stage", "onShutdown")
		hookStart := time.Now()
		hookDone := make(chan error, 1)
		go func() { hookDone <- wiring.OnShutdown(shutdownCtx) }()
		select {
		case err := <-hookDone:
			if err != nil {
				deps.Logger.Warn("drain failed", "stage", "onShutdown", "err", err, "elapsed", time.Since(hookStart))
			} else {
				deps.Logger.Info("drained", "stage", "onShutdown", "elapsed", time.Since(hookStart))
			}
		case <-shutdownCtx.Done():
			deps.Logger.Warn("onShutdown did not return before the drain deadline",
				"err", shutdownCtx.Err(), "elapsed", time.Since(hookStart))
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
