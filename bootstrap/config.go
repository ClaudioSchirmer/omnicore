package bootstrap

import (
	"fmt"
	"runtime"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/ClaudioSchirmer/omnicore/infra/audit"
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
	} `yaml:"http"`

	Postgres struct {
		DSN string `yaml:"dsn"`
	} `yaml:"postgres"`

	Mongo struct {
		URI      string             `yaml:"uri"`
		Database string             `yaml:"database"`
		Rebuild  MongoRebuildConfig `yaml:"rebuild"`
	} `yaml:"mongo"`

	Kafka struct {
		Brokers     []string `yaml:"brokers"`
		SyncGroupID string   `yaml:"syncGroupId"`
		// SyncWorkers controls the size of the SyncEngine worker pool that
		// processes Kafka messages in parallel (bucketed by aggregate_id so
		// per-aggregate ordering is preserved). 0 or unset → runtime.NumCPU().
		// Set to 1 to opt back into pure serial processing.
		SyncWorkers int `yaml:"syncWorkers"`
	} `yaml:"kafka"`

	Migrations struct {
		Dir     string      `yaml:"dir"`
		AutoRun AutoRunMode `yaml:"autoRun"`
	} `yaml:"migrations"`

	// Query configures cross-cutting read-side behavior. Currently the
	// global ceiling on `?limit=` for every paged GET — the per-view
	// override lives on ViewDefinition.MaxLimit, the framework fallback
	// is 100 when neither is declared.
	Query QueryConfig `yaml:"query"`

	Auth AuthConfig `yaml:"auth"`

	// Audit selects where the framework routes the AuditEvent produced by
	// every successful write. Defaults to both destinations (slog echo +
	// audit_events row in the same TX as the data row); operators can flip
	// to one or the other, or set destinations: [] to turn audit off. The
	// concrete type lives in infra/audit so the Postgres persister can read
	// it without crossing the dependency boundary back to bootstrap.
	Audit audit.Config `yaml:"audit"`

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
	// the attached handlers (the WHAT) live on Wiring.GraphQL in code. GraphQL
	// is its own web surface, never part of the OpenAPI/Swagger document. When
	// Wiring.GraphQL is nil this block is ignored.
	GraphQL GraphQLConfig `yaml:"graphql"`

	// UpstreamSubscriptions declares the cross-service composition
	// surface — for each entry, bootstrap.Run spins a Kafka consumer
	// + worker pool that materializes A's events into a local Mongo
	// collection and triggers recompose on every B view embedding it
	// via an external fwinfra.FromSchema. YAML is the canonical source; Wiring
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
	DrainTimeoutSeconds int `yaml:"drainTimeoutSeconds"`
}

// FrameworkDefaultShutdownTimeoutSeconds is the drain ceiling honored
// when the YAML omits a value or sets 0. 30s aligns with common
// kubernetes terminationGracePeriodSeconds (30s) so the pod-evicted
// drain completes inside the orchestrator's window.
const FrameworkDefaultShutdownTimeoutSeconds = 30

func (s *ShutdownConfig) applyDefaults() {
	if s.DrainTimeoutSeconds <= 0 {
		s.DrainTimeoutSeconds = FrameworkDefaultShutdownTimeoutSeconds
	}
}

// OpenAPIConfig configures HOW the OpenAPI spec is served. WHAT the spec
// describes (Title/Version/Description) stays on Wiring.OpenAPI — that is
// code-time identity, not environment config.
type OpenAPIConfig struct {
	// UIPath is the Fiber route where the Swagger UI HTML is served.
	// Default "/docs". Must start with "/" and must not collide with
	// "/openapi.json" or "/health" (both reserved by the framework).
	UIPath string `yaml:"uiPath"`

	// RootRedirect, when true, makes the framework register
	// GET / → 302 UIPath. Registered AFTER feature mounts so a service
	// that owns "/" wins; on collision the framework logs a slog.Warn and
	// skips the redirect registration.
	RootRedirect bool `yaml:"rootRedirect"`
}

// GraphQLConfig configures HOW the GraphQL endpoint is served. WHAT it exposes
// (the schema, the attached handlers) lives on Wiring.GraphQL in code. GraphQL
// is its own web surface — it never goes through openapi.Mount/MountRaw, never
// appears in the Swagger document, and is not policed by the REST route scans;
// the only thing shared with REST is the application-layer handlers it
// dispatches to.
type GraphQLConfig struct {
	// Path is the Fiber route the GraphQL endpoint is served on (POST).
	// Default "/graphql". Must start with "/" and must not collide with the
	// framework's reserved routes.
	Path string `yaml:"path"`

	// RootRedirect, when true, registers GET / → 302 Path. Mutually exclusive
	// with openapi.rootRedirect — only one surface can own "/".
	RootRedirect bool `yaml:"rootRedirect"`
}

func (g *GraphQLConfig) applyDefaults() {
	if g.Path == "" {
		g.Path = defaultGraphQLPath
	}
}

func (g *GraphQLConfig) validate() error {
	if !strings.HasPrefix(g.Path, "/") {
		return fmt.Errorf("graphql.path %q must start with %q", g.Path, "/")
	}
	switch g.Path {
	case "/openapi.json", "/health", "/docs":
		return fmt.Errorf("graphql.path %q collides with a framework route", g.Path)
	}
	return nil
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
// disappeared from Postgres (or, on DeleteOnArchive views, whose source
// is now archived). "delete" reconciles fully — Mongo becomes a pure
// function of Postgres after the rebuild. "warn" emits slog.Warn listing
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
}

// MongoRebuildOrphan* are the closed set of values for MongoRebuildConfig.Orphan.
const (
	MongoRebuildOrphanDelete = "delete"
	MongoRebuildOrphanWarn   = "warn"
)

// knownMongoRebuildKeys is the strict allowlist of yaml keys under
// mongo.rebuild. Anything else aborts the boot — surfaces removed fields
// (notably lockTTL, which the hybrid lock design eliminated) instead of
// silently ignoring them.
var knownMongoRebuildKeys = map[string]bool{
	"autoRun":        true,
	"orphan":         true,
	"allowDowngrade": true,
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
				"mongo.rebuild: unknown field %q (allowed: autoRun, orphan, allowDowngrade) — "+
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
	return nil
}

// defaultOpenAPIUIPath is the canonical UI route.
const defaultOpenAPIUIPath = "/docs"

// defaultGraphQLPath is the canonical GraphQL endpoint route.
const defaultGraphQLPath = "/graphql"

// QueryConfig collects the cross-cutting read-side knobs. Currently the
// global ceiling on `?limit=`; future fields land here without spinning a
// new yaml block per concern.
type QueryConfig struct {
	// MaxLimit is the default ceiling on `?limit=N` applied uniformly to
	// every paged GET that does not opt into a per-view override via
	// ViewDefinition.MaxLimit. Zero (or unset) defers to the framework
	// default 100. Negative values are rejected at boot.
	MaxLimit int64 `yaml:"maxLimit"`

	// MaxExportRows is the default ceiling on the number of rows a tabular
	// export (CSV/XLSX) streams, applied to every export route that does not
	// opt into a per-view override via ViewDefinition.MaxExportRows. Zero (or
	// unset) defers to infra.DefaultMaxExportRows. Negative values are rejected
	// at boot. The consumer forwards it to ViewDefinition.ResolveMaxExportRows.
	MaxExportRows int64 `yaml:"maxExportRows"`
}

// FrameworkDefaultMaxLimit is the read-side `?limit=` ceiling honored when
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
	case "/health":
		return fmt.Errorf("openapi.uiPath %q collides with the framework health route", o.UIPath)
	}
	return nil
}

func (c *Config) applyDefaults() {
	if c.HTTP.Addr == "" {
		c.HTTP.Addr = ":8080"
	}
	if c.Migrations.Dir == "" {
		c.Migrations.Dir = "./migrations"
	}
	if c.Kafka.SyncWorkers < 1 {
		c.Kafka.SyncWorkers = runtime.NumCPU()
	}
	c.Mongo.Rebuild.applyDefaults()
	c.Query.applyDefaults()
	c.Auth.applyDefaults()
	c.Audit.ApplyDefaults()
	c.OpenAPI.applyDefaults()
	c.GraphQL.applyDefaults()
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
}

// Validate checks required fields. Returns an error listing the missing ones.
func (c *Config) Validate() error {
	var missing []string
	if c.Service == "" {
		missing = append(missing, "service")
	}
	if c.Postgres.DSN == "" {
		missing = append(missing, "postgres.dsn")
	}
	if c.Mongo.URI == "" {
		missing = append(missing, "mongo.uri")
	}
	if c.Mongo.Database == "" {
		missing = append(missing, "mongo.database")
	}
	if len(c.Kafka.Brokers) == 0 {
		missing = append(missing, "kafka.brokers")
	}
	if c.Kafka.SyncGroupID == "" {
		missing = append(missing, "kafka.syncGroupId")
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
	if c.OpenAPI.RootRedirect && c.GraphQL.RootRedirect {
		return fmt.Errorf("bootstrap: openapi.rootRedirect and graphql.rootRedirect are mutually exclusive — only one surface can own GET /")
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
