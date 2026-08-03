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

// Pool returns the underlying pgxpool for integration tests that set up fixtures
// with raw DDL/DML the write API can't express (CREATE TABLE, direct INSERT/SELECT
// COUNT). It is not a consumer path: production code holds core.RelationalEngine
// and never recovers the concrete *Postgres (there is no "AsPostgres" cast), so a
// consumer needing custom reads uses the neutral Querier() instead. In production
// the adapter always holds a *pgxpool.Pool; a unit-test fake yields nil here.
func (p *Postgres) Pool() *pgxpool.Pool {
	if pool, ok := p.pool.(*pgxpool.Pool); ok {
		return pool
	}
	return nil
}

// querier exposes the pool as the minimal Exec/Query/QueryRow surface for
// read helpers (the aggregate loader's SELECTs). It returns the internal
// seam so those helpers run against a unit-test fake as well as a live pool,
// without widening the public surface — the neutral core.Querier (Querier())
// is the read handle consumers use across every backend.
func (p *Postgres) querier() pgExec { return p.pool }

// Acquire pins a connection from the underlying pool for the rebuild
// advisory-lock path (the Mongo-view control plane reaches it through the
// neutral AcquireRebuildLock, whose PG implementation calls this). It is
// pool-only (no core.Tx/Conn equivalent), so it stays off the pgxPool interface
// and is reached through this concrete assertion. Production always holds a real
// pool.
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
// exposes Value() string (not driver.Valuer), so it is converted here — the
// text form serves the native UUID column (the server casts). A *domain.ID is
// the NULLABLE identity field: nil stays nil (SQL NULL), non-nil unwraps
// through one dereference. Consumed by pgDialect.EncodeArg (the Postgres value
// codec) — a PG-side concern.
func normalizeArg(val any) any {
	val = core.UnwrapVO(val) // a value-object criteria value binds as its underlying scalar
	switch v := val.(type) {
	case domain.ID:
		return v.Value()
	case *domain.ID:
		if v == nil {
			return nil
		}
		return v.Value()
	default:
		return val
	}
}
