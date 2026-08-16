package bootstrap

import (
	"fmt"
	"runtime"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/ClaudioSchirmer/omnicore/infra/audit"
	"github.com/ClaudioSchirmer/omnicore/infra/grpcclient"
	"github.com/ClaudioSchirmer/omnicore/infra/httpclient"
	"github.com/ClaudioSchirmer/omnicore/infra/integration"
)

// AutoRunMode is the closed set of values for migrations.autoRun and
// mongo.rebuild.autoRun. The three modes share the same semantic:
//
//   - AutoRunCheck — validate without acting. Pending drift aborts boot
//     with a diagnostic listing recovery options (manual SQL reconcile,
//     flip the flag to true, or flip to false).
//   - AutoRunTrue  — validate and reconcile when the framework is certain
//     the action is safe (linear drift, fresh init on empty storage, etc.).
//     Aborts boot on ambiguous cases (AlienData, ForgotToBump, Downgrade
//     without the opt-in flag) where acting could destroy data or audit
//     history.
//   - AutoRunFalse — skip both validation and action. Operator takes full
//     responsibility for keeping storage in sync with the deployed code.
//
// Profile-aware defaults: dev → AutoRunTrue, any other profile →
// AutoRunCheck. Explicit yaml value (string or bool) wins.
//
// See tasks/mongo_schema_evolution_2.md §5 for the rationale.
type AutoRunMode string

const (
	AutoRunCheck AutoRunMode = "check"
	AutoRunTrue  AutoRunMode = "true"
	AutoRunFalse AutoRunMode = "false"
)

// UnmarshalYAML accepts both the canonical strings ("check"/"true"/"false")
// AND bare YAML booleans (`true` / `false`) — the latter normalize to the
// matching string so legacy yamls carrying `autoRun: true` keep parsing
// without quotes. Empty / absent leaves the zero value for downstream
// profile-default resolution. Anything else aborts the boot.
func (m *AutoRunMode) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("autoRun: expected scalar value, got %v", node.Kind)
	}
	switch node.Value {
	case "":
		*m = ""
		return nil
	case string(AutoRunCheck):
		*m = AutoRunCheck
		return nil
	case string(AutoRunTrue):
		*m = AutoRunTrue
		return nil
	case string(AutoRunFalse):
		*m = AutoRunFalse
		return nil
	}
	return fmt.Errorf("autoRun: invalid value %q (want %q | %q | %q)",
		node.Value, AutoRunCheck, AutoRunTrue, AutoRunFalse)
}

// IsTrue reports whether the mode authorizes the framework to act
// (reconcile when safe). False under both Check and False.
func (m AutoRunMode) IsTrue() bool { return m == AutoRunTrue }

// IsCheck reports whether the mode requires validation without action.
func (m AutoRunMode) IsCheck() bool { return m == AutoRunCheck }

// IsFalse reports whether the mode tells the framework to skip both
// validation and action.
func (m AutoRunMode) IsFalse() bool { return m == AutoRunFalse }

// Config is the single channel between microservice.<profile>.yaml and the
// framework. Loaded by LoadConfig (which picks the profile-specific file and
// applies profile-aware guards) or LoadConfigFrom (raw parse from an explicit
// path — used by tests).
type Config struct {
	// Profile is populated by LoadConfig from the APP_PROFILE env var
	// (required, non-empty; "dev" and "prd" are canonical, any other non-empty
	// string is accepted for QA/ops variants). It is NOT read from the YAML —
	// the source of truth is the environment so the same artifact cannot drift
	// between profiles. LoadConfigFrom leaves it empty.
	Profile string `yaml:"-"`

	Service string `yaml:"service"`

	HTTP struct {
		Addr string `yaml:"addr"`
		// RequestTimeoutSeconds bounds how long a single inbound request may run
		// before the framework cancels its context. The relational engine, mongo and outbound
		// httpclient observe the cancellation and abort, releasing the pool
		// connection and goroutine instead of letting a slow request hold them;
		// the request surfaces as 504 Gateway Timeout. nil (unset) → the
		// framework default (FrameworkDefaultRequestTimeoutSeconds); an explicit
		// 0 disables the deadline (a request may run unbounded — the pre-deadline
		// behavior). The cancellation also caps every outbound httpclient call at
		// the request's remaining budget.
		RequestTimeoutSeconds *int `yaml:"requestTimeoutSeconds"`

		// BodyLimitBytes caps the inbound request body size; a larger body is
		// rejected with 413 Payload Too Large (the ErrorHandler renders the
		// standard envelope). nil (unset) → Fiber's 4 MiB default. Raise it for
		// large uploads, lower it to tighten the edge. This is the ONE knob here
		// with a client-visible enveloped response — the two timeouts below are
		// transport-level (see their notes).
		BodyLimitBytes *int `yaml:"bodyLimitBytes"`

		// ReadTimeoutSeconds bounds how long the server waits to read the FULL
		// request (headers + body) off the socket — the transport-level slowloris
		// defense, distinct from requestTimeoutSeconds (which bounds handler
		// processing → 504). Enforced by the fasthttp server loop below the Fiber
		// handler chain; on breach fasthttp routes the timeout through Fiber's
		// serverErrorHandler, which the framework maps to 408 Request Timeout
		// (ReadTimeoutNotification) — the client was too slow, so it is a 408, not
		// a 500. The 408 envelope is written best-effort before the connection
		// closes (a client that already stalled may never read it). nil (unset) /
		// 0 → no read timeout (the default).
		ReadTimeoutSeconds *int `yaml:"readTimeoutSeconds"`

		// IdleTimeoutSeconds bounds how long an idle keep-alive connection is held
		// open awaiting the next request. Also transport-level, but unlike the read
		// timeout it produces NO response: fasthttp silently closes the idle
		// keep-alive (normal — the client reconnects), so there is no client-visible
		// error at all. nil (unset) / 0 → no idle timeout (the default).
		IdleTimeoutSeconds *int `yaml:"idleTimeoutSeconds"`
	} `yaml:"http"`

	// Relational selects AND connects the relational backend — the system of
	// record plus the framework control plane (outbox, audit, integration,
	// the Mongo-view registry). dialect picks the registered engine
	// (postgres / mysql; mysql self-registers only under its build tag); dsn
	// is the connection string for that dialect. Both are MANDATORY: the
	// framework refuses to assume a backend, so an absent dialect or dsn
	// aborts boot — there is no default. A dialect with no registered engine
	// (e.g. mysql in a binary built without -tags mysql) aborts in
	// core.NewEngine with an actionable message.
	Relational struct {
		Dialect string `yaml:"dialect"`
		DSN     string `yaml:"dsn"`

		// Pool bounds the relational connection pool, applied uniformly to
		// whichever engine is selected. Each field is a pointer so "unset"
		// (nil → framework default) is distinct from an explicit 0. Omit the
		// whole block for the defaults.
		//   maxOpenConns           — cap on total open connections. nil →
		//     max(4, NumCPU) (mirrors pgxpool's own default, so Postgres is
		//     unchanged; MySQL, unbounded by default, is bounded). An explicit 0
		//     opts into unlimited (Postgres keeps its driver default instead).
		//   maxIdleConns           — cap on retained idle connections. nil →
		//     maxOpenConns (keep the pool warm; avoids MySQL's idle=2 churn).
		//     database/sql-only; a no-op on Postgres.
		//   connMaxLifetimeSeconds — recycle a connection after this age.
		//     nil/0 → the engine's own default.
		Pool struct {
			MaxOpenConns           *int `yaml:"maxOpenConns"`
			MaxIdleConns           *int `yaml:"maxIdleConns"`
			ConnMaxLifetimeSeconds *int `yaml:"connMaxLifetimeSeconds"`
		} `yaml:"pool"`
	} `yaml:"relational"`

	Mongo struct {
		URI         string                 `yaml:"uri"`
		Database    string                 `yaml:"database"`
		Rebuild     MongoRebuildConfig     `yaml:"rebuild"`
		Reconcile   MongoReconcileConfig   `yaml:"reconcile"`
		ParkedRetry MongoParkedRetryConfig `yaml:"parkedRetry"`
	} `yaml:"mongo"`

	// Transport is the async message-transport connection, neutral across the
	// linked broker adapter (Kafka/Redpanda or NATS, selected by build tag).
	Transport struct {
		// Endpoints is the connection target list: Kafka/Redpanda bootstrap
		// servers for a kafka build, NATS URL(s) for a nats build.
		Endpoints []string `yaml:"endpoints"`
		// SyncGroup is the SyncEngine consumer group (Kafka group / NATS durable
		// consumer name).
		SyncGroup string `yaml:"syncGroup"`
		// SyncWorkers controls the size of the SyncEngine worker pool that
		// processes messages in parallel (bucketed by aggregate_id so
		// per-aggregate ordering is preserved). 0 or unset → runtime.NumCPU().
		// Set to 1 to opt back into pure serial processing.
		SyncWorkers int `yaml:"syncWorkers"`
	} `yaml:"transport"`

	Migrations struct {
		Dir     string      `yaml:"dir"`
		AutoRun AutoRunMode `yaml:"autoRun"`
	} `yaml:"migrations"`

	// Query configures cross-cutting read-side behavior. Currently the
	// global page-size ceiling (`?first=`/`?last=`) for every paged GET — the per-view
	// override lives on ViewDefinition.MaxLimit, the framework fallback
	// is 100 when neither is declared.
	Query QueryConfig `yaml:"query"`

	Auth AuthConfig `yaml:"auth"`

	// Audit selects where the framework routes the AuditEvent produced by
	// every successful write. Defaults to both destinations (slog echo +
	// audit_events row in the same TX as the data row); operators can flip
	// to one or the other, or set destinations: [] to turn audit off. The
	// concrete type lives in infra/audit so the relational persister can read
	// it without crossing the dependency boundary back to bootstrap.
	Audit audit.Config `yaml:"audit"`

	// Observability carries the cross-cutting telemetry block. Currently the
	// opt-in distributed-tracing sub-block (observability.tracing); default off,
	// so an absent block installs the OTel no-op tracer and costs nothing.
	Observability ObservabilityConfig `yaml:"observability"`

	// Cache is the top-level cache: block describing the framework's
	// generic key-value cache. nil when absent — Deps.Cache stays nil
	// and the httpclient response-cache middleware bypasses. Present
	// with a Store value drives construction of the private cache;
	// the optional Shared sub-block drives the cross-service cache
	// (Deps.SharedCache).
	Cache *CacheConfig `yaml:"cache,omitempty"`

	// HttpClient is non-nil when microservice.<profile>.yaml carries an
	// httpClient: block (with or without children). bootstrap.Build forwards
	// it to httpclient.New and exposes the result on Deps.HttpClient; when
	// nil, Deps.HttpClient stays nil and features that need the client must
	// guard at composition time.
	HttpClient *httpclient.Config `yaml:"httpClient"`

	// GRPCClient is non-nil when the yaml carries a `grpcClient:` block —
	// the outbound gRPC/Connect toolbox, sibling of httpClient. bootstrap
	// forwards it to grpcclient.New and exposes the result on
	// Deps.GRPCClient; when nil, Deps.GRPCClient stays nil and features
	// that need it must guard at composition time.
	GRPCClient *grpcclient.Config `yaml:"grpcClient"`

	// OpenAPI carries the operator-tunable bits of the /docs surface — the
	// path where the Swagger UI is served, and whether GET / redirects to
	// it. The spec identity (Title, Version, Description, Servers, …) stays
	// on Wiring.OpenAPI in code because it describes WHAT the service
	// exposes, not HOW it is served. When this block is absent the
	// framework keeps the canonical defaults (/docs, no redirect); when
	// present but Wiring.OpenAPI is nil it is ignored — no spec means no UI
	// to redirect to.
	OpenAPI OpenAPIConfig `yaml:"openapi"`

	// GraphQL carries the operator-tunable bits of the GraphQL endpoint — the
	// path it is served on and whether GET / redirects to it. The schema and
	// the attached handlers (the WHAT) come from the features implementing
	// GraphQLFeature (framework-built registry on Deps.GraphQLRegistry).
	// GraphQL is its own web surface, never part of the OpenAPI/Swagger
	// document. When no feature opts into the surface this block is ignored.
	GraphQL GraphQLConfig `yaml:"graphql"`

	// GRPC carries the operator-tunable bits of the gRPC surface — the
	// dedicated listener address, TLS material, the reflection toggle and
	// the transport auth policy. The mounted services (the WHAT) come from the
	// features implementing GRPCFeature (framework-built registry on
	// Deps.GRPCRegistry), exactly like GraphQL. When no feature opts into the
	// surface this block is ignored. The surface is served with Connect: one
	// endpoint speaking the gRPC, gRPC-Web and Connect protocols; without
	// TLS the listener runs h2c so the gRPC protocol still works.
	GRPC GRPCConfig `yaml:"grpc"`

	// UpstreamSubscriptions declares the cross-service composition
	// surface — for each entry, bootstrap.Run spins a Kafka consumer
	// + worker pool that materializes A's events into a local Mongo
	// collection and triggers recompose on every B view embedding it
	// via an external query.JoinUpstream. YAML is the canonical source; Wiring
	// exposes the same slice for manual lifecycle paths
	// (bootstrap.Build + Serve) and integration tests, with the
	// merge rule documented on Wiring.UpstreamSubscriptions.
	UpstreamSubscriptions []UpstreamSubscription `yaml:"upstreamSubscriptions"`

	// Integration is the cross-service async-messaging surface — the
	// publishes/subscribes block consumed by fwintegration.Dispatch
	// (producer side) and by the Receiver registry / ConsumerPool
	// (subscriber side). nil when the YAML omits the block entirely;
	// services not yet on integration events pay zero runtime cost.
	// Configure runs from bootstrap.Run BEFORE feature mounts so the
	// first Dispatch call site can resolve eventKey lookups even from
	// a feature's constructor.
	Integration *integration.Config `yaml:"integration"`

	// Shutdown configures the coordinated drain triggered by SIGINT /
	// SIGTERM. DrainTimeoutSeconds caps how long the framework waits
	// for HTTP server drain, integration consumer pool drain,
	// UpstreamSubscriber drain, and SyncEngine drain to complete
	// before forcing infra deps closed. 0 (or absent) defers to the
	// framework's existing 30s default.
	Shutdown ShutdownConfig `yaml:"shutdown"`
}

// ShutdownConfig holds operator-tunable knobs for the coordinated
// shutdown path. Kept as a struct (not a bare seconds field) so future
// knobs (per-drain timeout overrides, drain-stage trace level, etc.)
// land here without breaking YAML grammar.
type ShutdownConfig struct {
	// DrainTimeoutSeconds is the overall budget for the coordinated drain
	// (HTTP + gRPC + integration + upstream + sync). 0/omitted → default.
	DrainTimeoutSeconds int `yaml:"drainTimeoutSeconds"`
	// TracingDrainSeconds bounds the FINAL telemetry-flush stage on its OWN
	// short budget so a dead/slow OTLP collector cannot consume the whole
	// drain window (spans are best-effort — losing a few on a dead collector
	// beats hanging shutdown). 0/omitted → default.
	TracingDrainSeconds int `yaml:"tracingDrainSeconds"`
	// HardGraceSeconds is the extra margin, ON TOP OF DrainTimeoutSeconds,
	// after which a watchdog force-exits the process even if a non-cooperative
	// stage (a hook ignoring ctx, a stuck close) never returns. 0/omitted →
	// default; a NEGATIVE value disables the watchdog (embedders that own the
	// process lifecycle themselves).
	HardGraceSeconds int `yaml:"hardGraceSeconds"`
}

// FrameworkDefaultShutdownTimeoutSeconds is the drain ceiling honored
// when the YAML omits a value or sets 0. 30s aligns with common
// kubernetes terminationGracePeriodSeconds (30s) so the pod-evicted
// drain completes inside the orchestrator's window.
const FrameworkDefaultShutdownTimeoutSeconds = 30

// FrameworkDefaultTracingDrainSeconds bounds the telemetry-flush stage. Kept
// small on purpose: telemetry is best-effort and must never dominate the drain.
const FrameworkDefaultTracingDrainSeconds = 5

// FrameworkDefaultHardGraceSeconds is the watchdog margin over the drain budget.
// At DrainTimeoutSeconds + HardGraceSeconds the process force-exits so a
// non-cooperative stage can never hang it to SIGKILL.
const FrameworkDefaultHardGraceSeconds = 5

func (s *ShutdownConfig) applyDefaults() {
	if s.DrainTimeoutSeconds <= 0 {
		s.DrainTimeoutSeconds = FrameworkDefaultShutdownTimeoutSeconds
	}
	if s.TracingDrainSeconds <= 0 {
		s.TracingDrainSeconds = FrameworkDefaultTracingDrainSeconds
	}
	// HardGraceSeconds: 0 → default; negative → disabled (kept as-is).
	if s.HardGraceSeconds == 0 {
		s.HardGraceSeconds = FrameworkDefaultHardGraceSeconds
	}
}

// OpenAPIConfig configures HOW the OpenAPI spec is served. WHAT the spec
// describes (Title/Version/Description) stays on Wiring.OpenAPI — that is
// code-time identity, not environment config.
type OpenAPIConfig struct {
	// UIPath is the Fiber route where the Swagger UI HTML is served.
	// Default "/docs". Must start with "/" and must not collide with
	// "/openapi.json", "/livez" or "/readyz" (all reserved by the framework).
	UIPath string `yaml:"uiPath"`

	// RootRedirect, when true, makes the framework register
	// GET / → 302 UIPath. Registered AFTER feature mounts so a service
	// that owns "/" wins; on collision the framework logs a slog.Warn and
	// skips the redirect registration.
	RootRedirect bool `yaml:"rootRedirect"`
}

// GraphQLConfig configures HOW the GraphQL endpoint is served. WHAT it exposes
// (the schema, the attached handlers) comes from the GraphQLFeatures. GraphQL
// is its own web surface — it never goes through openapi.Mount/MountRaw, never
// appears in the Swagger document, and is not policed by the REST route scans;
// the only thing shared with REST is the application-layer handlers it
// dispatches to.
type GraphQLConfig struct {
	// Path is the Fiber route the GraphQL endpoint is served on (POST).
	// Default "/graphql". Must start with "/" and must not collide with the
	// framework's reserved routes.
	Path string `yaml:"path"`

	// UIPath is the Fiber route (GET) where the GraphiQL playground is served
	// when Playground is true. Default "/graphql/ui". Must start with "/" and
	// not collide with reserved routes or Path.
	UIPath string `yaml:"uiPath"`

	// Playground, when true, serves a GraphiQL page at UIPath. On/off like the
	// Swagger UI; off by default (opt-in). Pair with Introspection so the
	// playground can populate its schema docs / autocomplete.
	Playground bool `yaml:"playground"`

	// Introspection, when true, answers `__schema` / `__type` queries. On/off,
	// off by default — an operator opts in (typically in dev). Independent of
	// Playground, though the playground's autocomplete needs it.
	Introspection bool `yaml:"introspection"`

	// RootRedirect, when true, registers GET / → 302 Path. Mutually exclusive
	// with openapi.rootRedirect — only one surface can own "/".
	RootRedirect bool `yaml:"rootRedirect"`
}

func (g *GraphQLConfig) applyDefaults() {
	if g.Path == "" {
		g.Path = defaultGraphQLPath
	}
	if g.UIPath == "" {
		g.UIPath = defaultGraphQLUIPath
	}
}

func (g *GraphQLConfig) validate() error {
	if !strings.HasPrefix(g.Path, "/") {
		return fmt.Errorf("graphql.path %q must start with %q", g.Path, "/")
	}
	if g.collidesFramework(g.Path) {
		return fmt.Errorf("graphql.path %q collides with a framework route", g.Path)
	}
	if g.Playground {
		if !strings.HasPrefix(g.UIPath, "/") {
			return fmt.Errorf("graphql.uiPath %q must start with %q", g.UIPath, "/")
		}
		if g.collidesFramework(g.UIPath) {
			return fmt.Errorf("graphql.uiPath %q collides with a framework route", g.UIPath)
		}
		if g.UIPath == g.Path {
			return fmt.Errorf("graphql.uiPath %q must differ from graphql.path", g.UIPath)
		}
	}
	return nil
}

// GRPCConfig is the yaml `grpc:` block — transport knobs for the gRPC
// surface (Deps.GRPCRegistry). Ignored when no feature implements
// GRPCFeature.
type GRPCConfig struct {
	// Addr is the dedicated listener address, e.g. ":9090". The gRPC
	// surface never shares the Fiber listener (fasthttp cannot host
	// HTTP/2 services). Default ":9090".
	Addr string `yaml:"addr"`

	// CertFile/KeyFile enable TLS on the listener. Both or neither; with
	// neither the listener serves h2c (HTTP/2 cleartext) so the gRPC
	// protocol works in dev without certificates.
	CertFile string `yaml:"certFile"`
	KeyFile  string `yaml:"keyFile"`

	// Reflection serves the gRPC server-reflection service for the mounted
	// services (grpcurl, buf curl discovery). Off by default.
	Reflection bool `yaml:"reflection"`

	// Auth selects the listener's security posture. Default "inherit".
	Auth GRPCAuthConfig `yaml:"auth"`

	// ClientCAFile is the PEM bundle of the internal CA that signs client
	// certificates — REQUIRED (with the TLS pair) when auth.mode=mtls: the
	// listener then demands and verifies a client certificate per
	// connection, and the certificate names the calling service.
	ClientCAFile string `yaml:"clientCAFile"`

	// IdleTimeoutSeconds closes keep-alive connections idle for this long —
	// the producer-side load-balancing lever: kube-proxy balances per NEW
	// connection, so recycling idle pipes forces callers to re-dial and
	// redistributes traffic (scale-up pods join). Default 120: above the Go
	// client's 90s idle (the client closes first — no reuse race), below
	// NAT/conntrack floors (~350s — no zombie pipes). 0 keeps the default;
	// negative disables.
	IdleTimeoutSeconds int `yaml:"idleTimeoutSeconds"`

	// RequestTimeoutSeconds bounds each RPC server-side — the DEADLINE_EXCEEDED
	// sibling of http.requestTimeoutSeconds. 0 disables the server-side
	// ceiling (the protocol deadline the client sends still applies).
	RequestTimeoutSeconds int `yaml:"requestTimeoutSeconds"`

	// PublicProcedures lists fully-qualified procedures that bypass
	// authentication, e.g. "/users.v1.UsersService/Health" — the gRPC
	// sibling of auth.publicRoutes.
	PublicProcedures []string `yaml:"publicProcedures"`
}

// GRPCAuthConfig is the yaml grpc.auth block.
type GRPCAuthConfig struct {
	// Mode: "inherit" (default — the global auth: block governs, exactly
	// like the HTTP surface), "internal" (the trusted plane: anonymous
	// calls pass; a forwarded bearer is ATTRIBUTION — validated locally,
	// expiry tolerated with a stale audit mark, never checked against the
	// external validator), or "mtls" (internal + required client
	// certificates; an anonymous call carries the calling service's
	// identity from its certificate).
	Mode string `yaml:"mode"`
}

func (g *GRPCConfig) applyDefaults() {
	if g.Addr == "" {
		g.Addr = ":9090"
	}
	if g.Auth.Mode == "" {
		g.Auth.Mode = "inherit"
	}
	if g.IdleTimeoutSeconds == 0 {
		g.IdleTimeoutSeconds = 120
	}
}

func (g *GRPCConfig) validate() error {
	if (g.CertFile == "") != (g.KeyFile == "") {
		return fmt.Errorf("grpc.certFile and grpc.keyFile must be set together (got certFile=%q, keyFile=%q)", g.CertFile, g.KeyFile)
	}
	if g.RequestTimeoutSeconds < 0 {
		return fmt.Errorf("grpc.requestTimeoutSeconds must be >= 0 (got %d)", g.RequestTimeoutSeconds)
	}
	switch g.Auth.Mode {
	case "", "inherit", "internal":
	case "mtls":
		if g.CertFile == "" || g.ClientCAFile == "" {
			return fmt.Errorf("grpc.auth.mode=mtls requires grpc.certFile/keyFile AND grpc.clientCAFile (the internal CA verifying client certificates)")
		}
	default:
		return fmt.Errorf("grpc.auth.mode %q (want inherit|internal|mtls)", g.Auth.Mode)
	}
	return nil
}

func (g *GraphQLConfig) collidesFramework(path string) bool {
	return collidesFrameworkPath(path)
}

// collidesFrameworkPath is the reserved-route set shared by every optional
// self-mounted surface (GraphQL, OpenAPI, the issuer's JWKS document): a
// path an operator cannot repurpose because the framework already owns it.
func collidesFrameworkPath(path string) bool {
	switch path {
	case "/openapi.json", "/livez", "/readyz", "/docs":
		return true
	}
	return false
}

// MongoRebuildConfig governs the boot-time reconciliation of Mongo
// projections against the declared ViewDefinition shape. The concurrency
// primitive is hybrid: pg_advisory_lock for mutual exclusion (auto-release
// on disconnect, no TTL math) and a status column on omnicore_mongo_views
// for forensic survivability + plain-SQL inspection. See
// tasks/mongo_schema_evolution_2.md §11.
//
// AutoRun follows the same profile-aware default as Migrations.AutoRun:
// dev resolves to AutoRunTrue, any other profile resolves to AutoRunCheck.
// Explicit yaml value wins. Under AutoRunCheck, drift detection runs but
// the framework refuses to write — every non-None decision aborts boot
// with the matching §14 diagnostic listing the manual SQL reconcile.
//
// Orphan controls what the rebuild does with documents whose source
// disappeared from the relational source (or, on DeleteOnArchive views, whose source
// is now archived). "delete" reconciles fully — Mongo becomes a pure
// function of the relational source after the rebuild. "warn" emits slog.Warn listing
// the orphan _id set and leaves the documents untouched, deferring the
// destructive decision to the operator.
//
// AllowDowngrade is the opt-in escape hatch for the DriftDowngrade case
// under AutoRunTrue. Default false — code declaring an older version than
// the registry aborts boot. Setting true treats the downgrade as a normal
// rollback flow (canary / blue-green), at the cost of erasing the v(N+1)..
// v(M) audit trail beyond the single-step previous_* snapshot. Under
// AutoRunCheck or AutoRunFalse the flag is irrelevant.
type MongoRebuildConfig struct {
	AutoRun        AutoRunMode `yaml:"autoRun"`
	Orphan         string      `yaml:"orphan"`
	AllowDowngrade bool        `yaml:"allowDowngrade"`
	// PointerLeaseSeconds is the bounded-staleness lease of the blue-green
	// activation fence: how long a rebuild driver waits for every pod to observe
	// the dual-apply flag before backfilling, and how long a post-flip settle
	// waits before reclaim. It bounds how quickly a pod observes a flip. 0
	// (unset) → the framework default (15s). Lower it on single-pod / dev to
	// shrink the boot-rebuild window; raise it if pods refresh slowly.
	PointerLeaseSeconds int `yaml:"pointerLeaseSeconds"`

	// Workers is the number of concurrent compose+write workers the boot-time
	// blue-green backfill pipeline runs: each worker composes a batch of roots
	// from the relational source and bulk-upserts it into the shadow slot, and
	// batches fan across workers so the relational scan+compose overlaps the
	// Mongo write. 0 (unset) → the framework default (4). Raise it to saturate a
	// multi-core box + a Mongo that can absorb parallel bulk writes; the
	// relational pool must carry at least Workers+1 connections (the streaming
	// root-id scan pins one).
	Workers int `yaml:"workers"`

	// BatchSize is the number of root ids composed + bulk-upserted per batch.
	// 0 (unset) → the framework default (1000). Larger batches cut the number of
	// SQL and Mongo round trips at the cost of more memory per in-flight batch;
	// keep each comfortably under Mongo's 16MB command envelope.
	BatchSize int `yaml:"batchSize"`
}

// MongoRebuildOrphan* are the closed set of values for MongoRebuildConfig.Orphan.
const (
	MongoRebuildOrphanDelete = "delete"
	MongoRebuildOrphanWarn   = "warn"
)

// MongoReconcileConfig schedules the continuous revision-parity sweep
// (SyncEngine.ReconcileAllViews): a background audit that compares every view
// document's stored revision against its relational source and recomposes what
// is missing or behind — the backstop for losses no failure ledger can see,
// because nothing reported an error (an event that never arrived, a park that
// itself failed, an acknowledged write the store lost).
//
// OFF by default, deliberately: the sweep's full-pass duration is the
// convergence bound it provides (table size / rate), and that trade belongs to
// the operator who can see both numbers — not to an upgrade. Leaving it off in
// dev is also a feature: a projection bug then SURFACES as a stale document
// instead of being quietly repaired every interval.
type MongoReconcileConfig struct {
	// Enabled turns the background sweep loop on. Off (the default), the
	// mechanism still exists (ReconcileView / ReconcileAllViews are callable)
	// but nothing runs on a cadence — and ProjectionHealth's LastReconcile
	// stays zero, which is the observable an operator can alarm on if the
	// sweep is expected to be running.
	Enabled bool `yaml:"enabled"`
	// IntervalMinutes is the pause between the END of one full pass and the
	// START of the next — a slow pass never overlaps the next one. 0 (unset)
	// → the framework default (60).
	IntervalMinutes int `yaml:"intervalMinutes"`
	// RowsPerSecond throttles the sweep; it is the ONLY cost knob, a hard
	// ceiling on instantaneous load regardless of table size. What table size
	// stretches is the pass DURATION (rows / rate), which is the detection
	// bound this backstop provides. 0 (unset) → the framework default;
	// negative → unthrottled.
	RowsPerSecond int `yaml:"rowsPerSecond"`
	// BatchSize is how many source rows are compared per round trip. 0 (unset)
	// → the framework default.
	BatchSize int `yaml:"batchSize"`
}

// MongoParkedRetryConfig governs the parked-events replay driver — the loop
// that sweeps this consumer group's `omnicore_projection_failures` rows and
// re-delivers them through the normal projection path. ON by default: the
// driver is what turns "park and advance" into DEFERRED work rather than a
// dead letter, and its idle cost is one indexed SELECT per interval. The knob
// exists because the cadence is an operator's trade (recovery latency after an
// outage heals vs. background polling), not a library constant.
type MongoParkedRetryConfig struct {
	// Enabled turns the replay driver on/off. Unset → true. Off, a parked
	// event is replayed only by the reconcile sweep (when enabled) or by a
	// manual RetryPendingProjectionFailures call.
	Enabled *bool `yaml:"enabled"`
	// IntervalMinutes is the sweep cadence. 0 (unset) → the framework default
	// (10 minutes). Dev/QA profiles typically pin 1 for tight recovery.
	IntervalMinutes int `yaml:"intervalMinutes"`
}

// knownMongoParkedRetryKeys is the strict allowlist of yaml keys under
// mongo.parkedRetry.
var knownMongoParkedRetryKeys = map[string]bool{
	"enabled":         true,
	"intervalMinutes": true,
}

// UnmarshalYAML decodes the block and rejects unknown keys.
func (m *MongoParkedRetryConfig) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.MappingNode {
		return fmt.Errorf("mongo.parkedRetry: expected mapping, got %v", value.Kind)
	}
	for i := 0; i+1 < len(value.Content); i += 2 {
		keyNode := value.Content[i]
		if !knownMongoParkedRetryKeys[keyNode.Value] {
			return fmt.Errorf(
				"mongo.parkedRetry: unknown field %q (allowed: enabled, intervalMinutes)",
				keyNode.Value,
			)
		}
	}
	type plain MongoParkedRetryConfig
	var p plain
	if err := value.Decode(&p); err != nil {
		return err
	}
	*m = MongoParkedRetryConfig(p)
	return nil
}

// applyDefaults resolves the default-ON semantics (a plain bool cannot tell
// "absent" from "false", hence the pointer).
func (m *MongoParkedRetryConfig) applyDefaults() {
	if m.Enabled == nil {
		on := true
		m.Enabled = &on
	}
}

// knownMongoReconcileKeys is the strict allowlist of yaml keys under
// mongo.reconcile — an unknown key aborts the boot instead of being silently
// ignored, mirroring mongo.rebuild.
var knownMongoReconcileKeys = map[string]bool{
	"enabled":         true,
	"intervalMinutes": true,
	"rowsPerSecond":   true,
	"batchSize":       true,
}

// UnmarshalYAML decodes the block and rejects unknown keys.
func (m *MongoReconcileConfig) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.MappingNode {
		return fmt.Errorf("mongo.reconcile: expected mapping, got %v", value.Kind)
	}
	for i := 0; i+1 < len(value.Content); i += 2 {
		keyNode := value.Content[i]
		if !knownMongoReconcileKeys[keyNode.Value] {
			return fmt.Errorf(
				"mongo.reconcile: unknown field %q (allowed: enabled, intervalMinutes, rowsPerSecond, batchSize)",
				keyNode.Value,
			)
		}
	}
	type plain MongoReconcileConfig
	var p plain
	if err := value.Decode(&p); err != nil {
		return err
	}
	*m = MongoReconcileConfig(p)
	return nil
}

// applyDefaults fills the cadence default; the rate and batch defaults live in
// the query package (they are shared with direct ReconcileAllViews callers).
func (m *MongoReconcileConfig) applyDefaults() {
	if m.IntervalMinutes == 0 {
		m.IntervalMinutes = 60
	}
}

// knownMongoRebuildKeys is the strict allowlist of yaml keys under
// mongo.rebuild. Anything else aborts the boot — surfaces removed fields
// (notably lockTTL, which the hybrid lock design eliminated) instead of
// silently ignoring them.
var knownMongoRebuildKeys = map[string]bool{
	"autoRun":             true,
	"orphan":              true,
	"allowDowngrade":      true,
	"pointerLeaseSeconds": true,
	"workers":             true,
	"batchSize":           true,
}

// UnmarshalYAML decodes the block and rejects unknown keys. lockTTL —
// removed when the framework moved off the Mongo TTL lock — lands here
// and aborts the boot with a diagnostic pointing at the cleanup. See
// tasks/mongo_schema_evolution_2.md §17.5.
func (m *MongoRebuildConfig) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.MappingNode {
		return fmt.Errorf("mongo.rebuild: expected mapping, got %v", value.Kind)
	}

	// Walk the mapping nodes to enforce the strict allowlist BEFORE we
	// decode. Pair-keyed: even indices are keys, odd indices are values.
	for i := 0; i+1 < len(value.Content); i += 2 {
		keyNode := value.Content[i]
		if !knownMongoRebuildKeys[keyNode.Value] {
			return fmt.Errorf(
				"mongo.rebuild: unknown field %q (allowed: autoRun, orphan, allowDowngrade, pointerLeaseSeconds, workers, batchSize) — "+
					"see tasks/mongo_schema_evolution_2.md §17.5",
				keyNode.Value,
			)
		}
	}

	// `type plain` avoids infinite recursion via the standard yaml.v3
	// pattern: aliasing strips the custom UnmarshalYAML method off the
	// type so the default decoder kicks in for the actual field population.
	type plain MongoRebuildConfig
	var p plain
	if err := value.Decode(&p); err != nil {
		return err
	}
	*m = MongoRebuildConfig(p)
	return nil
}

// applyDefaults sets the profile-agnostic defaults — orphan="delete" when
// yaml leaves it empty. The AutoRun default is profile-aware and lives in
// Config.applyProfileDefaults; it is NOT set here.
func (m *MongoRebuildConfig) applyDefaults() {
	if m.Orphan == "" {
		m.Orphan = MongoRebuildOrphanDelete
	}
}

// validate enforces the closed sets on AutoRun and Orphan. Empty AutoRun
// is accepted because LoadConfigFrom is profile-agnostic — Validate runs
// BEFORE applyProfileDefaults, so an unset autoRun field reaches here as
// "". LoadConfig calls applyProfileDefaults after Validate to resolve
// the empty value to the profile-aware default.
func (m *MongoRebuildConfig) validate() error {
	switch m.AutoRun {
	case "", AutoRunCheck, AutoRunTrue, AutoRunFalse:
	default:
		return fmt.Errorf("mongo.rebuild.autoRun %q invalid (want %q | %q | %q)",
			m.AutoRun, AutoRunCheck, AutoRunTrue, AutoRunFalse)
	}
	switch m.Orphan {
	case MongoRebuildOrphanDelete, MongoRebuildOrphanWarn:
	default:
		return fmt.Errorf("mongo.rebuild.orphan %q invalid (want %q | %q)",
			m.Orphan, MongoRebuildOrphanDelete, MongoRebuildOrphanWarn)
	}
	if m.PointerLeaseSeconds < 0 {
		return fmt.Errorf("mongo.rebuild.pointerLeaseSeconds %d invalid (want >= 0; 0 = framework default)", m.PointerLeaseSeconds)
	}
	if m.Workers < 0 {
		return fmt.Errorf("mongo.rebuild.workers %d invalid (want >= 0; 0 = framework default)", m.Workers)
	}
	if m.BatchSize < 0 {
		return fmt.Errorf("mongo.rebuild.batchSize %d invalid (want >= 0; 0 = framework default)", m.BatchSize)
	}
	return nil
}

// defaultOpenAPIUIPath is the canonical UI route.
const defaultOpenAPIUIPath = "/docs"

// defaultGraphQLPath is the canonical GraphQL endpoint route.
const defaultGraphQLPath = "/graphql"

// defaultGraphQLUIPath is the canonical GraphiQL playground route.
const defaultGraphQLUIPath = "/graphql/ui"

// QueryConfig collects the cross-cutting read-side knobs. Currently the
// global page-size ceiling (`?first=`/`?last=`); future fields land here without spinning a
// new yaml block per concern.
type QueryConfig struct {
	// MaxLimit is the default ceiling on `?first=N`/`?last=N` applied uniformly to
	// every paged GET that does not opt into a per-view override via
	// ViewDefinition.MaxLimit. Zero (or unset) defers to the framework
	// default 100. Negative values are rejected at boot.
	MaxLimit int64 `yaml:"maxLimit"`

	// MaxExportRows is the default ceiling on the number of rows a tabular
	// export (CSV/XLSX) streams, applied to every export route that does not
	// opt into a per-view override via ViewDefinition.MaxExportRows. Zero (or
	// unset) defers to mongo.DefaultMaxExportRows. Negative values are rejected
	// at boot. The consumer forwards it to ViewDefinition.ResolveMaxExportRows.
	MaxExportRows int64 `yaml:"maxExportRows"`

	// MaxLinkManyLimit is the default per-parent item ceiling of a composed
	// view's LinkMany segment, applied to every link that does not declare a
	// per-link override via Leg.MaxLinkManyLimit. Zero (or unset) defers to
	// the framework default (query.FrameworkDefaultMaxLinkManyLimit, 100).
	// Negative values are rejected at boot.
	MaxLinkManyLimit int64 `yaml:"maxLinkManyLimit"`
}

// FrameworkDefaultMaxLimit is the read-side page-size ceiling honored when
// neither the yaml nor any ViewDefinition declares a value. Conservative
// by design — a service expecting larger pages declares it explicitly.
const FrameworkDefaultMaxLimit int64 = 100

func (q *QueryConfig) applyDefaults() {
	if q.MaxLimit == 0 {
		q.MaxLimit = FrameworkDefaultMaxLimit
	}
}

func (q *QueryConfig) validate() error {
	if q.MaxLimit < 0 {
		return fmt.Errorf("query.maxLimit must be > 0 (got %d)", q.MaxLimit)
	}
	if q.MaxExportRows < 0 {
		return fmt.Errorf("query.maxExportRows must be >= 0 (got %d)", q.MaxExportRows)
	}
	if q.MaxLinkManyLimit < 0 {
		return fmt.Errorf("query.maxLinkManyLimit must be >= 0 (got %d)", q.MaxLinkManyLimit)
	}
	return nil
}

func (o *OpenAPIConfig) applyDefaults() {
	if o.UIPath == "" {
		o.UIPath = defaultOpenAPIUIPath
	}
}

func (o *OpenAPIConfig) validate() error {
	if !strings.HasPrefix(o.UIPath, "/") {
		return fmt.Errorf("openapi.uiPath %q must start with %q", o.UIPath, "/")
	}
	switch o.UIPath {
	case "/openapi.json":
		return fmt.Errorf("openapi.uiPath %q collides with the framework spec route", o.UIPath)
	case "/livez", "/readyz":
		return fmt.Errorf("openapi.uiPath %q collides with a framework health-probe route", o.UIPath)
	}
	return nil
}

// FrameworkDefaultRequestTimeoutSeconds is the inbound request deadline applied
// when http.requestTimeoutSeconds is unset. An explicit 0 disables the deadline.
const FrameworkDefaultRequestTimeoutSeconds = 30

// FrameworkDefaultMaxOpenConns is the relational pool ceiling applied when
// relational.pool.maxOpenConns is unset. It mirrors pgxpool's own default
// (max(4, NumCPU)) so Postgres is unchanged, and bounds MySQL — whose
// database/sql default is an unlimited pool.
func FrameworkDefaultMaxOpenConns() int {
	if n := runtime.NumCPU(); n > 4 {
		return n
	}
	return 4
}

func (c *Config) applyDefaults() {
	if c.HTTP.Addr == "" {
		c.HTTP.Addr = ":8080"
	}
	if c.HTTP.RequestTimeoutSeconds == nil {
		d := FrameworkDefaultRequestTimeoutSeconds
		c.HTTP.RequestTimeoutSeconds = &d
	}
	if c.Migrations.Dir == "" {
		c.Migrations.Dir = "./migrations"
	}
	if c.Transport.SyncWorkers < 1 {
		c.Transport.SyncWorkers = runtime.NumCPU()
	}
	// Relational pool: bound both engines by default so a write burst applies
	// backpressure instead of flooding the backend. nil (unset) → default; an
	// explicit 0 for maxOpenConns is honored as "unlimited" (Postgres keeps its
	// driver default). maxIdleConns defaults to maxOpenConns to keep the pool warm.
	if c.Relational.Pool.MaxOpenConns == nil {
		d := FrameworkDefaultMaxOpenConns()
		c.Relational.Pool.MaxOpenConns = &d
	}
	if c.Relational.Pool.MaxIdleConns == nil {
		d := *c.Relational.Pool.MaxOpenConns
		if d <= 0 {
			d = FrameworkDefaultMaxOpenConns()
		}
		c.Relational.Pool.MaxIdleConns = &d
	}
	if c.Relational.Pool.ConnMaxLifetimeSeconds == nil {
		d := 0
		c.Relational.Pool.ConnMaxLifetimeSeconds = &d
	}
	c.Mongo.Rebuild.applyDefaults()
	c.Mongo.Reconcile.applyDefaults()
	c.Mongo.ParkedRetry.applyDefaults()
	c.Query.applyDefaults()
	c.Auth.applyDefaults()
	c.Audit.ApplyDefaults()
	c.OpenAPI.applyDefaults()
	c.GraphQL.applyDefaults()
	c.GRPC.applyDefaults()
	c.Observability.applyDefaults()
	c.Shutdown.applyDefaults()
	if c.Integration != nil {
		c.Integration.ApplyDefaults(c.Service)
	}
}

// applyProfileDefaults resolves the defaults whose value depends on the
// runtime profile. Currently:
//
//   - Migrations.AutoRun: dev → AutoRunTrue, any other profile → AutoRunCheck.
//   - Mongo.Rebuild.AutoRun: same dev=true / non-dev=check rule.
//
// Both stay overridable per service: an explicit yaml value
// ("check"/"true"/"false" or bare boolean) wins, profile-aware default
// applies only when the yaml left the field empty. Called from LoadConfig
// after LoadConfigFrom returns and Profile is set; LoadConfigFrom callers
// (typically tests) get the raw zero values and can decide their own
// posture.
func (c *Config) applyProfileDefaults(profile string) {
	if c.Migrations.AutoRun == "" {
		if profile == profileDev {
			c.Migrations.AutoRun = AutoRunTrue
		} else {
			c.Migrations.AutoRun = AutoRunCheck
		}
	}
	if c.Mongo.Rebuild.AutoRun == "" {
		if profile == profileDev {
			c.Mongo.Rebuild.AutoRun = AutoRunTrue
		} else {
			c.Mongo.Rebuild.AutoRun = AutoRunCheck
		}
	}
	c.Observability.applyProfileDefaults(profile)
}

// Validate checks required fields. Returns an error listing the missing ones.
func (c *Config) Validate() error {
	var missing []string
	if c.Service == "" {
		missing = append(missing, "service")
	}
	if c.Relational.Dialect == "" {
		missing = append(missing, "relational.dialect")
	}
	if c.Relational.DSN == "" {
		missing = append(missing, "relational.dsn")
	}
	// mongo.* and transport.* are OPTIONAL — each infrastructure is opt-out by its
	// own config block (see yaml-reference.html). Omitting mongo.uri boots without
	// Mongo (relational views only); omitting transport.endpoints boots without a
	// broker (no messaging). A service that declares work needing an absent
	// infrastructure is caught by a coherence guard: a Mongo-backed/composed view
	// with no mongo.uri aborts the boot (see runWithConfig), and an integration
	// consumer / upstream subscription with no broker fails at the point of use
	// (the no-op transport). The only conditional requirement: mongo.database is
	// mandatory WHEN mongo.uri is set (a uri with no database is a real mistake).
	if c.Mongo.URI != "" && c.Mongo.Database == "" {
		missing = append(missing, "mongo.database (required when mongo.uri is set)")
	}
	if len(missing) > 0 {
		return fmt.Errorf("bootstrap: missing required config: %s", strings.Join(missing, ", "))
	}
	if err := c.Auth.validate(); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	if err := c.Audit.Validate(); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	if err := c.OpenAPI.validate(); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	if err := c.GraphQL.validate(); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	if err := c.GRPC.validate(); err != nil {
		return err
	}
	if err := c.Mongo.Rebuild.validate(); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	if c.Integration != nil {
		if err := c.Integration.Validate(); err != nil {
			return fmt.Errorf("bootstrap: %w", err)
		}
	}
	if cacheErrs := validateCache(c.Cache); len(cacheErrs) > 0 {
		return fmt.Errorf("bootstrap: %s", strings.Join(cacheErrs, "; "))
	}
	if c.Shutdown.DrainTimeoutSeconds < 0 {
		return fmt.Errorf("bootstrap: shutdown.drainTimeoutSeconds must be >= 0 (got %d)", c.Shutdown.DrainTimeoutSeconds)
	}
	if v := c.HTTP.BodyLimitBytes; v != nil && *v < 0 {
		return fmt.Errorf("bootstrap: http.bodyLimitBytes must be >= 0 (got %d)", *v)
	}
	if v := c.HTTP.ReadTimeoutSeconds; v != nil && *v < 0 {
		return fmt.Errorf("bootstrap: http.readTimeoutSeconds must be >= 0 (got %d)", *v)
	}
	if v := c.HTTP.IdleTimeoutSeconds; v != nil && *v < 0 {
		return fmt.Errorf("bootstrap: http.idleTimeoutSeconds must be >= 0 (got %d)", *v)
	}
	if err := c.Observability.validate(); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	return nil
}

// validateForProfile enforces invariants that depend on the runtime profile
// rather than on the YAML alone. Currently a single rule:
// auth.mode=disabled is rejected under any profile other than "dev", so a prd
// boot fails fast if authentication has not been wired.
func (c *Config) validateForProfile(profile string) error {
	if c.Auth.Mode == AuthModeDisabled && profile != profileDev {
		return fmt.Errorf("bootstrap: auth.mode=%q is not allowed when %s=%q", c.Auth.Mode, profileEnv, profile)
	}
	return nil
}
