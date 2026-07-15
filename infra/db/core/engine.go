package core

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/ClaudioSchirmer/omnicore/application/persistence"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/audit"
	"github.com/ClaudioSchirmer/omnicore/infra/events"
)

// RelationalEngine is the backend-neutral persistence port the write binding
// (BaseRepository.boundWriter) and the composition root depend on. It is the
// seam that lets a relational backend (Postgres, MySQL, SQL Server) drop in
// without the upper layers — domain, application, web — ever naming a concrete
// driver. A dialect is selected once, at boot, through the engine registry
// (RegisterEngine / NewEngine); the concrete engines live in sibling packages
// (infra/db/engine/postgres, infra/db/engine/mysql, infra/db/engine/sqlserver),
// each compiled behind its own build tag, and implement this port.
//
// The write methods take the exported WriteHook (the type-erased lifecycle-hook
// pair the persister fires at TX positions A and D) so an engine in its own
// package can name it in the method signatures it must satisfy. The hook is built
// by AdaptWriteOptions in this package; each engine only receives it and fires it
// against its own TX.
type RelationalEngine interface {
	Insert(ctx persistence.RequestContext, entity domain.Insertable, schema *TableSchema, hook WriteHook) (domain.WriteResult, error)
	Update(ctx persistence.RequestContext, entity domain.Updatable, schema *TableSchema, hook WriteHook) (domain.WriteResult, error)
	Archive(ctx persistence.RequestContext, entity domain.Archivable, schema *TableSchema, hook WriteHook) error
	Unarchive(ctx persistence.RequestContext, entity domain.Unarchivable, schema *TableSchema, hook WriteHook) error
	Delete(ctx persistence.RequestContext, entity domain.Deletable, schema *TableSchema, hook WriteHook) error

	// Querier is the neutral read surface the AggregateLoader runs its SELECTs
	// through; Dialect renders the engine-specific statement bits. Together they
	// are the read seam that lets a live aggregate load on any backend.
	Querier() Querier
	Dialect() Dialect

	// WithAudit configures the audit surface (in-TX audit_events row +
	// post-commit slog echo, routed by cfg.Destinations) and WithEventPublisher
	// the transport for domain events accumulated on entities. Both are
	// configured once at boot on the neutral engine — each backend writes the
	// audit row through its own dialect (PG via pgx, MySQL via database/sql) and
	// fires the same post-commit echo/publish. They return the engine so the
	// composition root can chain; a nil cfg / nil publisher disables the feature.
	WithAudit(cfg *audit.Config, logger *slog.Logger, auditClaims []string) RelationalEngine
	WithEventPublisher(pub events.Publisher) RelationalEngine

	// AcquireRebuildLock takes the cluster-wide Mongo-view rebuild mutex for the
	// named view on a pinned session and returns a handle that BOTH reports
	// ownership and carries a Querier bound to that pinned session — so the
	// registry status writes (BeginRebuild/EndRebuild) co-locate on the very
	// connection that holds the lock. The lock is advisory and session-scoped on
	// every engine (Postgres pg_advisory_lock on a pinned pool conn; MySQL
	// GET_LOCK on a pinned pool conn), auto-releasing if the session drops — no
	// TTL bookkeeping. A handle is ALWAYS returned on a nil error, even when the
	// lock is already held elsewhere (Acquired() reports which), and MUST be
	// Released exactly once to free the lock and return the session to the pool.
	// A non-nil error means the session/lock query itself failed (nothing to
	// release). The Mongo-view control plane is the sole caller.
	AcquireRebuildLock(ctx context.Context, viewName string) (RebuildLock, error)

	Close()
}

// PoolConfig bounds the relational connection pool. It is applied uniformly to
// whichever engine is selected, so a write burst applies backpressure (requests
// queue for a connection) instead of flooding the backend with unbounded
// concurrent transactions.
//
//   - MaxOpenConns caps total open connections. Zero means unlimited — the
//     database/sql default; Postgres cannot express unlimited and keeps its pgx
//     driver default (a positive ceiling) instead.
//   - MaxIdleConns caps retained idle connections. This is a database/sql knob;
//     it is a no-op on Postgres, whose pgxpool governs idleness through MinConns
//     (a floor) and MaxConnIdleTime, not an idle ceiling.
//   - ConnMaxLifetime recycles a connection after this age. Zero leaves the
//     engine's own default.
//
// bootstrap resolves the values from the relational.pool config block (defaulting
// MaxOpenConns to max(4, NumCPU), which mirrors pgxpool's own default so Postgres
// is unchanged and MySQL — unbounded by default — is bounded).
type PoolConfig struct {
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
}

// EngineConfig carries everything an EngineFactory needs to open a backend. It is
// the options-struct generalization the previous positional (dsn, tracing)
// signature anticipated — a new cross-engine knob (Pool) is added as a field, not
// another positional argument.
type EngineConfig struct {
	DSN     string
	Tracing bool
	Pool    PoolConfig
}

// EngineFactory builds a RelationalEngine for one dialect. Registered by each
// engine package in init(), each behind its own build tag (postgres under
// -tags postgres, mysql under -tags mysql; a build links exactly one engine).
type EngineFactory func(ctx context.Context, cfg EngineConfig) (RelationalEngine, error)

// engineFactories is the dialect → factory registry. Mirrors the database/sql
// driver-registration pattern: an engine self-registers in init(), the
// composition root looks it up by name. Keeping the swap here (not a hardcoded
// switch in bootstrap) is what lets the MySQL engine live behind a build tag
// without bootstrap importing it.
var engineFactories = map[string]EngineFactory{}

// RegisterEngine records a factory under a dialect name. Called from an engine
// package's init(). A duplicate registration panics — two engines claiming the
// same dialect is a build-time bug.
func RegisterEngine(dialect string, f EngineFactory) {
	if _, dup := engineFactories[dialect]; dup {
		panic(fmt.Sprintf("db.RegisterEngine: dialect %q already registered", dialect))
	}
	engineFactories[dialect] = f
}

// NewEngine builds the RelationalEngine for the requested dialect. An unknown
// dialect is a clear, actionable error — typically no engine build tag was set
// (e.g. `go build -tags postgres` or `go build -tags mysql`).
func NewEngine(dialect string, ctx context.Context, cfg EngineConfig) (RelationalEngine, error) {
	f, ok := engineFactories[dialect]
	if !ok {
		return nil, fmt.Errorf("db: no relational engine registered for dialect %q (build with the engine's build tag?)", dialect)
	}
	return f(ctx, cfg)
}
