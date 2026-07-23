package bootstrap

import (
	"log/slog"

	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/application/translation"
	"github.com/ClaudioSchirmer/omnicore/infra/cache"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/db/query"
	"github.com/ClaudioSchirmer/omnicore/infra/db/query/engine/mongo"
	"github.com/ClaudioSchirmer/omnicore/infra/grpcclient"
	"github.com/ClaudioSchirmer/omnicore/infra/httpclient"
	"github.com/ClaudioSchirmer/omnicore/infra/integration"
	"github.com/ClaudioSchirmer/omnicore/infra/tracing"
	"github.com/ClaudioSchirmer/omnicore/infra/transport"
	"github.com/ClaudioSchirmer/omnicore/web"
	"github.com/ClaudioSchirmer/omnicore/web/openapi"
)

// dialectPostgres / dialectMySQL / dialectSQLServer / dialectOracle are the
// registered relational-engine dialect names — they mirror the engine-registry
// keys and the relational.dialect values. Referenced from the tag-gated engine
// bindings and integration tests (e.g. mysql_boot_integration), which the
// default lint view does not compile — hence the nolint.
//
//nolint:unused // used under the engine build tags.
const (
	dialectPostgres  = "postgres"
	dialectMySQL     = "mysql"
	dialectSQLServer = "sqlserver"
	dialectOracle    = "oracle"
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
// DB is the backend-neutral relational engine; the dialect is chosen at boot via
// relational.dialect and resolved through the engine registry (core.NewEngine).
// Consumers depend on the interface, not a concrete driver. A build links exactly
// one engine, selected by build tag (-tags postgres | -tags mysql | -tags
// sqlserver | -tags oracle); the matching engine_<dialect>.go file registers it
// and carries its migration-runner factory. Every engine reaches its backend the
// same way — the neutral core.RelationalEngine surface (DB.Querier() for custom
// reads, the in-TX Unwrap<Engine>Tx escape hatch for hooks) — with no
// concrete-engine recovery: there is no per-engine "AsX" cast.
type Deps struct {
	Config     *Config
	Logger     *slog.Logger
	DB         core.RelationalEngine
	Mongo      *mongo.MongoDB
	Translator *translation.Translator
	Pipeline   *pipeline.Pipeline
	ViewReader queries.ViewReader

	// Resolver maps a logical view name to the physical Mongo collection it
	// currently resolves to (its active slot). One instance is shared by every
	// read-model component so they observe a single, consistent pointer view.
	Resolver *query.ViewResolver

	// Transport is the message-transport port — the linked broker adapter
	// (Kafka/Redpanda now, NATS later) selected once at boot by the transport
	// build tag (-tags kafka | -tags nats), through the subscriber registry.
	// Every async consumer opens its subscription through this one instance:
	// the SyncEngine, the integration ConsumerPool, and each UpstreamSubscriber.
	// Consumers depend on the transport.Subscriber interface, not a concrete
	// client — the seam that lets a broker drop in without those loops changing.
	Transport transport.Subscriber

	// Tracing owns the OpenTelemetry tracer provider's lifetime. Always
	// non-nil after bootstrap.Build/Run (inert when observability.tracing is
	// off). serve() flushes it during the drain so buffered spans are exported
	// before the process exits; framework-managed, services do not read it.
	Tracing *tracing.Provider

	// Export pre-packages the service-ambient inputs every tabular export
	// (CSV/XLSX) shares — the Translator (labelKey header rendering) and the
	// yaml default row ceiling (cfg.Query.MaxExportRows). The consumer threads
	// it straight into fwweb.QueryAsCSVSpec / …XLSXSpec alongside the
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

	// GRPCClient is the outbound gRPC/Connect toolbox; non-nil when the
	// YAML carries a grpcClient: block. Features construct generated
	// Connect clients through grpcclient.For / the accessors; the adapter
	// lives in the service's infra/, exactly like httpclient consumers.
	GRPCClient *grpcclient.Client

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
	UpstreamSubscribers []*query.UpstreamSubscriber

	// SyncEngine exposes the live read-side projection engine spun by
	// bootstrap.Run. Surfaced so serve's coordinated drain can wait for the
	// projection loop's FULL exit — every in-flight compose+upsert finished,
	// then the Kafka reader closed (LeaveGroup sent) — before the relational
	// and Mongo handles close. nil when the service declares no views;
	// SyncEngine.Shutdown is nil-safe either way.
	SyncEngine *query.SyncEngine

	// bootRebuild coordinates the background boot-time view rebuild with the
	// probes and the drain. Unexported — internal boot machinery. nil when the
	// service declares no views or autoRun skipped the rebuild.
	bootRebuild *bootRebuild
}
