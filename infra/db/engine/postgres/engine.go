//go:build postgres

package postgres

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/audit"
	"github.com/ClaudioSchirmer/omnicore/infra/db/command/write"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/events"
)

// Compile-time proof that the Postgres adapter satisfies the relational port
// and the neutral write-TX beginner. The backend-neutral write orchestration
// (every write verb, audit, outbox, hooks) lives on the embedded write.BaseEngine;
// this package supplies only the Postgres dialect + driver (pgx pool, pgDialect
// and the neutral adapters in read.go, the tx seal in tx_handle.go, the advisory
// rebuild lock in rebuild_lock.go).
var _ core.RelationalEngine = (*Postgres)(nil)
var _ core.WriteBeginner = (*Postgres)(nil)

// AsPostgres recovers the concrete *Postgres from a core.RelationalEngine. It is the
// PG-only escape hatch for the framework wiring that still speaks pgx directly
// (pool access, partitions, migrations, the rebuild lock). Using it pins the
// caller to Postgres — exactly like UnwrapPgxTx on the in-TX side. Panics when
// the engine is not the PG adapter, failing loudly at the composition root
// rather than producing a nil pool that would NPE deep in a query.
//
// NOTE: this returns the concrete *Postgres, which lives here; infra/db must not
// depend back on the engine.
func AsPostgres(e core.RelationalEngine) *Postgres {
	pg, ok := e.(*Postgres)
	if !ok {
		panic(fmt.Sprintf("infra.AsPostgres: engine is %T, not *Postgres — this code path is Postgres-specific", e))
	}
	return pg
}

// init registers the Postgres engine under the "postgres" dialect,
// database/sql-style. Postgres ships untagged (the default backend); the
// composition root looks it up by name via core.NewEngine.
func init() {
	core.RegisterEngine("postgres", func(ctx context.Context, cfg core.EngineConfig) (core.RelationalEngine, error) {
		return NewPostgres(ctx, cfg.DSN, WithPgxTracing(cfg.Tracing), WithPool(cfg.Pool))
	})
}

// pgExec is the minimal pgx execution surface the engine's internal helpers
// (querier, read.go) run SQL through. *pgxpool.Pool and pgx.Tx both satisfy it
// structurally. It is the PG-side twin of the Mongo-view control plane's own
// pgExec (defined in infra), kept package-local so the engine does not import
// back into infra.
type pgExec interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// pgxPool is the minimal pool surface the persister, loader, and composer
// consume. It is unexported and never leaves the infra package, so the
// abstraction does not cross a layer boundary. *pgxpool.Pool satisfies it
// in production; a fake satisfies it in unit tests, letting the
// transactional core run without a live database. It embeds pgExec (the
// Exec/Query/QueryRow trio the registry helpers already share) and adds the
// pool-only Begin/Close. The pool-only Acquire (pinned connection for the
// rebuild advisory lock) stays off the interface and is reached via the
// acquire helper, which asserts the concrete pool.
type pgxPool interface {
	pgExec
	Begin(ctx context.Context) (pgx.Tx, error)
	Close()
}

// Postgres is the persistence adapter. The cross-engine write-path state +
// orchestration (audit row, post-commit echo/publish, lifecycle-hook dispatch)
// live on the embedded write.BaseEngine; this type supplies only the pgx pool and
// the dialect-bound parts. After construction the audit surface is configured
// via WithAudit — without that call the persister runs in a fully audit-disabled
// posture (no in-TX row, no slog echo) which is correct for tests + integration
// fixtures that construct the pool directly. bootstrap.Run wires it in
// production after Build.
type Postgres struct {
	write.BaseEngine
	pool pgxPool
}

// PostgresOption tunes NewPostgres. Variadic so existing callers
// (NewPostgres(ctx, dsn)) keep compiling unchanged.
type PostgresOption func(*postgresOptions)

type postgresOptions struct {
	tracePgx bool
	pool     core.PoolConfig
}

// WithPgxTracing wires the otelpgx query tracer onto the pool so every
// statement emits a span. bootstrap passes tracing.Instruments(SubPgx); false
// (the default) leaves the pool untraced and pays nothing.
func WithPgxTracing(enabled bool) PostgresOption {
	return func(o *postgresOptions) { o.tracePgx = enabled }
}

// WithPool bounds the pgx pool. MaxOpenConns maps to pgxpool's MaxConns and
// ConnMaxLifetime to MaxConnLifetime; both are left at pgx's own default when
// zero (pgx cannot express an unlimited pool). MaxIdleConns is a database/sql
// knob with no pgxpool equivalent (pgxpool manages idleness through MinConns and
// MaxConnIdleTime) and is intentionally not mapped here.
func WithPool(p core.PoolConfig) PostgresOption {
	return func(o *postgresOptions) { o.pool = p }
}

func NewPostgres(ctx context.Context, dsn string, opts ...PostgresOption) (*Postgres, error) {
	var o postgresOptions
	for _, opt := range opts {
		opt(&o)
	}
	// ParseConfig + NewWithConfig is exactly what pgxpool.New does internally;
	// the explicit form is needed to attach the per-connection query tracer.
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	if o.tracePgx {
		config.ConnConfig.Tracer = otelpgx.NewTracer()
	}
	// Apply the pool ceiling. A zero (unlimited) MaxOpenConns leaves pgx's own
	// default MaxConns — pgx requires a positive bound and cannot run unlimited.
	if o.pool.MaxOpenConns > 0 {
		config.MaxConns = int32(o.pool.MaxOpenConns)
	}
	if o.pool.ConnMaxLifetime > 0 {
		config.MaxConnLifetime = o.pool.ConnMaxLifetime
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	p := &Postgres{pool: pool}
	p.SetBeginner(p) // the embedded BaseEngine opens write TXs through this engine
	return p, nil
}

// WithAudit configures the audit surface (delegating to the embedded
// BaseEngine) and returns the receiver as the neutral RelationalEngine so the
// composition root chains without a dialect branch. nil cfg disables audit; a
// Config with destinations: [] yields the same posture (empty list = off); nil
// logger falls back to slog.Default(). The auditClaims allowlist controls which
// JWT claims surface on the actorClaims block — see auth.auditClaims.
func (p *Postgres) WithAudit(cfg *audit.Config, logger *slog.Logger, auditClaims []string) core.RelationalEngine {
	p.ConfigureAudit(cfg, logger, auditClaims)
	return p
}

// WithEventPublisher wires the transport for domain events accumulated on
// entities via entity.RegisterEvent (delegating to the embedded BaseEngine) and
// returns the receiver so the call chains at boot. A nil publisher (the default)
// disables publishing — events stay on the ValidEntity, simply not forwarded.
// bootstrap.Run wires events.NewSlogPublisher in production; a consumer can
// inject any events.Publisher (Kafka, etc.) to override the transport.
func (p *Postgres) WithEventPublisher(pub events.Publisher) core.RelationalEngine {
	p.SetPublisher(pub)
	return p
}

func (p *Postgres) Close() {
	p.pool.Close()
}

// Begin opens a framework-owned write TX and returns it as the backend-neutral
// core.WriteTx — the Postgres side of core.WriteBeginner. The neutral write
// orchestration (outbox, audit, hooks) runs against this surface; the concrete
// pgx.Tx stays private behind pgTx.
func (p *Postgres) Begin(ctx context.Context) (core.WriteTx, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return pgTx{tx: tx}, nil
}

// Pool returns the underlying pgxpool so repositories can run custom SELECTs
// (FindByID with JOINs, paginated lookups, etc.) that don't fit the write
// API. Use only for reads — writes must go through Insert/Update/Archive/
// Delete/Unarchive to preserve the outbox + audit guarantees. In production
// the adapter always holds a *pgxpool.Pool; a unit-test fake yields nil here
// (such tests exercise the write API, never Pool()).
func (p *Postgres) Pool() *pgxpool.Pool {
	if pool, ok := p.pool.(*pgxpool.Pool); ok {
		return pool
	}
	return nil
}

// querier exposes the pool as the minimal Exec/Query/QueryRow surface for
// read helpers (the aggregate loader's SELECTs). It returns the internal
// seam so those helpers run against a unit-test fake as well as a live
// pool, without widening the public surface — Pool() stays the public,
// concrete read handle for consumer repositories.
func (p *Postgres) querier() pgExec { return p.pool }

// Acquire pins a connection from the underlying pool for the rebuild
// advisory-lock path (the Mongo-view control plane in infra recovers it via
// AsPostgres). It is pool-only (no core.Tx/Conn equivalent), so it stays off the
// pgxPool interface and is reached through this concrete assertion. Production
// always holds a real pool.
func (p *Postgres) Acquire(ctx context.Context) (*pgxpool.Conn, error) {
	pool, ok := p.pool.(*pgxpool.Pool)
	if !ok {
		return nil, fmt.Errorf("infra: connection acquire requires a live pgx pool")
	}
	return pool.Acquire(ctx)
}

// validIdentifier is the panicking Postgres-side identifier check + passthrough
// (pgDialect.QuoteIdent renders bare, validated identifiers). Delegates to
// SafeIdentifier for the allowlist.
func validIdentifier(name string) string {
	if !core.SafeIdentifier(name) {
		panic(fmt.Sprintf("infra: invalid SQL identifier %q", name))
	}
	return name
}

// normalizeArg unwraps framework value types pgx cannot bind directly. domain.ID
// exposes Value() string (not driver.Valuer), so it is converted here. Consumed
// by pgDialect.EncodeArg (the Postgres value codec) — a PG-side concern.
func normalizeArg(val any) any {
	if id, ok := val.(domain.ID); ok {
		return id.Value()
	}
	return val
}
