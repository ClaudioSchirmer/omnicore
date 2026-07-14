//go:build sqlserver

// Package sqlserver is the SQL Server implementation of core.RelationalEngine.
// It ships behind the `sqlserver` build tag so a build without it never
// compiles this package nor links go-mssqldb. The composition root selects it
// via relational.dialect: sqlserver; the engine self-registers in init() below.
//
// Scope: this package is the SQL Server DIALECT + DRIVER adapter only. The
// backend-neutral persistence orchestration — the write path (flat + aggregate:
// Insert/Update/Archive/Unarchive/Delete + Batch, the child state machine, the
// cascade, the outbox/audit rows, and the A/D lifecycle hooks) and the audit/
// event wiring — lives once in infra/db on the embedded write.BaseEngine and runs
// against the neutral core.WriteTx this engine opens via Begin. What stays here
// is genuinely dialect-bound: the *sql.DB pool + registration (this file), the
// sqlserverDialect (@pN placeholders, bracket quoting, UUID ⇄ BINARY(16) codec,
// error 2627/2601/547 classification, LOWER LIKE, MERGE upsert, CURRENT_TIMESTAMP,
// TOP row cap) and the neutral cursor in read.go, the tx seal + WriteTx adapter
// in tx_handle.go, and the rebuild lock in rebuild_lock.go. The file layout
// mirrors infra/db/engine/mysql one-to-one.
//
// Design decisions locked for the third backend (see tasks/sqlserver.md):
//   - Identity is stored BINARY(16), NEVER UNIQUEIDENTIFIER: SQL Server orders
//     GUIDs last-byte-group-first, which destroys the UUIDv7 time order and
//     fragments the clustered PK; BINARY(16) compares bytewise, so the v7 ids
//     minted in Go keep the clustered index append-friendly (the InnoDB
//     rationale, verified against a live SQL Server 2022).
//   - The upsert is a single MERGE … WITH (HOLDLOCK) statement (BuildUpsert
//     returns one SQL string); HOLDLOCK closes the match-then-insert race.
//   - NowExpr is CURRENT_TIMESTAMP — server-timezone parity with NOW() on
//     PG/MySQL (SYSUTCDATETIME would make this the one UTC dialect).
//   - The row cap is a SELECT-head TOP n (ApplyLimit rewrites the head; a tail
//     OFFSET…FETCH is not usable — it requires ORDER BY, existence probes have
//     none).
//
// Per-statement OpenTelemetry tracing is wired through otelsql when tracing is
// enabled (see New). Manual loader scanners run on this engine too: they
// receive the backend-neutral core.Row/core.Rows.
package sqlserver

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"

	"github.com/XSAM/otelsql"
	"github.com/google/uuid"
	_ "github.com/microsoft/go-mssqldb"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"

	"github.com/ClaudioSchirmer/omnicore/infra/audit"
	"github.com/ClaudioSchirmer/omnicore/infra/db/command/write"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/events"
)

// init registers the SQL Server engine under the "sqlserver" dialect,
// database/sql-style. Because this file is behind the `sqlserver` build tag, a
// build without it never runs it — the dialect is simply absent and
// core.NewEngine("sqlserver", …) returns the "no engine registered (build with
// the engine's build tag?)" error.
func init() {
	core.RegisterEngine("sqlserver", New)
}

// Engine is the SQL Server RelationalEngine. It holds a *sql.DB pool;
// construction is lazy on the connection (Ping verifies reachability at New).
// The cross-engine write-path state + orchestration live on the embedded
// write.BaseEngine — configured once at boot via WithAudit/WithEventPublisher;
// zero values disable audit/publishing. This type supplies only the *sql.DB and
// the dialect bits.
type Engine struct {
	write.BaseEngine
	db *sql.DB
}

// Compile-time proof the engine satisfies the framework port and the neutral
// write-TX beginner.
var _ core.RelationalEngine = (*Engine)(nil)
var _ core.WriteBeginner = (*Engine)(nil)

// sqlExecutor is the minimal database/sql execution surface the querier
// (read.go) runs statements through. Both *sql.DB (the pool) and *sql.Conn (a
// pinned session, used by the rebuild lock so status writes share the lock's
// connection) satisfy it, so one querier serves both the pooled read path and
// the pinned-session one.
type sqlExecutor interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// New opens the SQL Server pool and verifies it with a Ping. The DSN is passed
// to go-mssqldb verbatim — the driver accepts both the URL and the ODBC-style
// (`server=…;user id=…;password=…`) forms, and unlike MySQL no flag has to be
// forced for the framework to function (time.Time scanning and rows-affected
// semantics are native). When tracing is on, the pool is opened through otelsql
// so every statement emits a span (db.system=mssql); when off, a plain sql.Open
// keeps the pool untraced and pays nothing.
func New(ctx context.Context, cfg core.EngineConfig) (core.RelationalEngine, error) {
	var (
		sqlDB *sql.DB
		err   error
	)
	if cfg.Tracing {
		sqlDB, err = otelsql.Open("sqlserver", cfg.DSN, otelsql.WithAttributes(semconv.DBSystemMSSQL))
	} else {
		sqlDB, err = sql.Open("sqlserver", cfg.DSN)
	}
	if err != nil {
		return nil, fmt.Errorf("sqlserver: open: %w", err)
	}
	// Bound the pool: database/sql defaults to an UNLIMITED open pool (and only
	// 2 idle), so under a write burst it would open connections without
	// backpressure. A zero MaxOpenConns keeps that unlimited default (explicit
	// opt-in); bootstrap defaults it to max(4, NumCPU) — the same policy as the
	// MySQL engine.
	if cfg.Pool.MaxOpenConns > 0 {
		sqlDB.SetMaxOpenConns(cfg.Pool.MaxOpenConns)
	}
	if cfg.Pool.MaxIdleConns > 0 {
		sqlDB.SetMaxIdleConns(cfg.Pool.MaxIdleConns)
	}
	if cfg.Pool.ConnMaxLifetime > 0 {
		sqlDB.SetConnMaxLifetime(cfg.Pool.ConnMaxLifetime)
	}
	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("sqlserver: ping: %w", err)
	}
	e := &Engine{db: sqlDB}
	e.SetBeginner(e) // the embedded BaseEngine opens write TXs through this engine
	return e, nil
}

func (e *Engine) Close() {
	if e.db != nil {
		_ = e.db.Close()
	}
}

// Begin opens a framework-owned write TX and returns it as the backend-neutral
// core.WriteTx — the SQL Server side of core.WriteBeginner. The neutral write
// orchestration (flat + aggregate verbs, outbox, audit, hooks) runs against
// this surface; the concrete *sql.Tx stays private behind sqlserverTx.
func (e *Engine) Begin(ctx context.Context) (core.WriteTx, error) {
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	return sqlserverTx{tx: tx}, nil
}

// WithAudit configures the audit surface (in-TX audit_events row routed by
// cfg.Destinations + post-commit slog echo), delegating to the embedded
// BaseEngine. nil cfg disables audit; nil logger falls back to slog.Default()
// in the echo path. Returns the engine as the neutral RelationalEngine so the
// composition root wires it without a dialect branch.
func (e *Engine) WithAudit(cfg *audit.Config, logger *slog.Logger, auditClaims []string) core.RelationalEngine {
	e.ConfigureAudit(cfg, logger, auditClaims)
	return e
}

// WithEventPublisher wires the transport for domain events accumulated via
// entity.RegisterEvent (delegating to the embedded BaseEngine). A nil publisher
// (the default) disables publishing.
func (e *Engine) WithEventPublisher(pub events.Publisher) core.RelationalEngine {
	e.SetPublisher(pub)
	return e
}

// quoteIdent bracket-quotes a SQL identifier after validating it against the
// framework's allowlist (the same trust model as the other engines: identifiers
// come from schema declarations, never user input). Panics on a bad identifier
// — a programming error in the schema, surfaced loudly. Consumed by
// sqlserverDialect (read.go) so the shared SQL builders render SQL
// Server-flavored identifiers.
func quoteIdent(name string) string {
	if !core.SafeIdentifier(name) {
		panic(fmt.Sprintf("sqlserver: invalid SQL identifier %q", name))
	}
	return "[" + name + "]"
}

// uuidBytes parses a UUID string into its 16-byte BINARY(16) form. The
// framework's IDs are UUIDs; a non-UUID id is a wiring error. Consumed by
// sqlserverDialect.EncodeArg (read.go) to render ids/UUID values as BINARY(16).
func uuidBytes(id string) ([]byte, error) {
	u, err := uuid.Parse(id)
	if err != nil {
		return nil, fmt.Errorf("sqlserver: id %q is not a UUID: %w", id, err)
	}
	return u[:], nil
}
