package bootstrap

import (
	"log/slog"

	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/application/translation"
	"github.com/ClaudioSchirmer/omnicore/infra/db/read/mongo"
	"github.com/ClaudioSchirmer/omnicore/infra/cache"
	"github.com/ClaudioSchirmer/omnicore/infra/db"
	"github.com/ClaudioSchirmer/omnicore/infra/db/engine/pg"
	"github.com/ClaudioSchirmer/omnicore/infra/httpclient"
	"github.com/ClaudioSchirmer/omnicore/infra/integration"
	"github.com/ClaudioSchirmer/omnicore/infra/tracing"
	"github.com/ClaudioSchirmer/omnicore/web"
	"github.com/ClaudioSchirmer/omnicore/web/openapi"
)

// Deps are the singletons built by the framework and exposed to the service
// through the Wire callback.
//
// Audit is no longer an explicit dependency: the persistence layer (the
// RelationalEngine) owns the audit emission, configured at boot via
// WithAudit(cfg.Audit, logger, cfg.Auth.AuditClaims). The Repository methods
// route audit transparently — every successful write produces the configured
// slog echo and / or in-TX audit_events row without each handler having to
// thread an Auditor.
//
// DB is the backend-neutral relational engine (Postgres today; the dialect is
// chosen at boot via database.dialect). Consumers depend on the interface, not
// a concrete driver. Code that genuinely needs Postgres-specific access (the
// pgx pool for custom SELECTs) recovers it via pg.AsPostgres(d.DB) — a
// documented PG-only escape hatch.
// pgEngine recovers the concrete *pg.Postgres from Deps for the framework
// wiring that still speaks pgx directly — audit partition maintenance, pgx-based
// migrations, and the Mongo-view rebuild/drift control plane (advisory lock +
// omnicore_mongo_views registry). These boot steps are gated by isPostgres, so
// pgEngine is only reached on a Postgres engine; it panics otherwise
// (composition-root invariant). The composer/sync projection and the integration
// consumer control plane no longer go through here — they speak the neutral seam.
func pgEngine(deps Deps) *pg.Postgres { return pg.AsPostgres(deps.DB) }

// dialectPostgres / dialectMySQL are the registered relational-engine dialect
// names (mirror the infra engine-registry keys and the database.dialect default).
const (
	dialectPostgres = "postgres"
	dialectMySQL    = "mysql"
)

// isPostgres reports whether the selected relational backend is Postgres — the
// gate for the boot steps still bound to pgx: audit partition maintenance and the
// Mongo-view rebuild/drift control plane. A MySQL service boots with those gated
// off (it serves CRUD + the Mongo projection; rebuild/drift stay Postgres-only).
func isPostgres(cfg *Config) bool { return cfg.Database.Dialect == dialectPostgres }

type Deps struct {
	Config     *Config
	Logger     *slog.Logger
	DB         db.RelationalEngine
	Mongo      *mongo.MongoDB
	Translator *translation.Translator
	Pipeline   *pipeline.Pipeline
	ViewReader queries.ViewReader

	// Tracing owns the OpenTelemetry tracer provider's lifetime. Always
	// non-nil after bootstrap.Build/Run (inert when observability.tracing is
	// off). serve() flushes it during the drain so buffered spans are exported
	// before the process exits; framework-managed, services do not read it.
	Tracing *tracing.Provider

	// Export pre-packages the service-ambient inputs every tabular export
	// (CSV/XLSX) shares — the Translator (labelKey header rendering) and the
	// yaml default row ceiling (cfg.Query.MaxExportRows). The consumer threads
	// it straight into fwweb.HandleQueryAsCSVSpec / …XLSXSpec alongside the
	// view, so export routes stop spelling out d.Translator +
	// d.Config.Query.MaxExportRows by hand. Always populated by bootstrap.Run.
	Export web.ExportDeps

	// Cache is the SERVICE-PRIVATE key-value cache. Non-nil when the
	// YAML carries a top-level cache: block. Use for everything scoped
	// to this service: domain cache, computed-value memoization, the
	// outbound httpclient response cache (the framework wires its own
	// middleware to consume this same instance).
	//
	// nil when the cache: block is absent — feature code that relies
	// on it must guard at composition time or use the typed helpers
	// (cache.GetJSON/SetJSON) which tolerate a nil Cache and degrade
	// to a no-op.
	Cache cache.Cache

	// SharedCache is the CROSS-SERVICE key-value cache. Non-nil ONLY
	// when the YAML carries a cache.shared: sub-block. Use for keys
	// that other services in the cluster are expected to read
	// (feature flags coordinated across services, cluster-wide rate
	// limits, sessions consumed by an API gateway, …).
	//
	// nil when cache.shared: is absent — feature code that uses it
	// MUST guard explicitly: an in-process LRU cannot honor cross-
	// service reads, so the framework rejects cache.shared.store:
	// memory at boot. Reaching for SharedCache without the operator
	// declaring it is a programming bug; the nil guard makes the
	// mistake immediate at the call site.
	SharedCache cache.Cache

	// HttpClient is the outbound HTTP registry; non-nil when the YAML carries
	// an httpClient: block. Features that talk to external services forward
	// it to their infra/external service structs. nil when the block is
	// absent — features that rely on it must guard at composition time.
	HttpClient *httpclient.HttpClient

	// OpenAPIRegistry is the spec collector consumed by openapi.Mount /
	// openapi.MountRaw to document the service's routes. Non-nil when
	// Wiring.OpenAPI is set; nil otherwise. Mount / MountRaw treat a nil
	// registry as a passthrough, so features can forward d.OpenAPIRegistry
	// to those calls uniformly without nil-checking.
	OpenAPIRegistry *openapi.Registry

	// IntegrationRegistry collects every Receiver the service's
	// IntegrationFeature implementations register during Phase
	// Receivers. Non-nil when bootstrap.Run runs; consumer features
	// receive it via MountReceivers(reg, deps). Also surfaced for
	// admin retry routes that walk Receivers() to drain pending
	// failure rows.
	IntegrationRegistry *integration.Registry

	// UpstreamSubscribers exposes the live cross-service composition
	// subscribers spun by bootstrap.Run. Surfaced so consumer admin
	// surfaces (HTTP routes, CLI tools) can call
	// UpstreamSubscriber.RetryPendingFailures(ctx) without re-resolving
	// the slice through internal package state. nil when the service
	// declared zero upstream subscriptions in YAML / Wiring.
	UpstreamSubscribers []*mongo.UpstreamSubscriber
}
