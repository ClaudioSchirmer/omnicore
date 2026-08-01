//go:build sqlite

// Package sqlite is the SQLite implementation of core.RelationalEngine, built on
// the pure-Go, cgo-free modernc.org/sqlite driver — the enabler of the
// self-executable, zero-infra MVP: CGO_ENABLED=0 go build -tags sqlite yields a
// single static binary that boots against a plain app.db file (tasks/sql_mvp.md).
// It ships behind the `sqlite` build tag so a build without it never compiles
// this package nor links modernc. The composition root selects it via
// relational.dialect: sqlite; the engine self-registers in init() below.
//
// Scope: this package is the SQLite DIALECT + DRIVER adapter only. The
// backend-neutral persistence orchestration (write verbs, child state machine,
// cascade, outbox/audit rows, lifecycle hooks) lives once on the embedded
// write.BaseEngine and runs against the neutral core.WriteTx this engine opens
// via Begin. What stays here is dialect-bound: the *sql.DB pool + registration
// (this file), the sqliteDialect (dialect.go), the time-decoding cursor (read.go),
// the tx seal (tx_handle.go), the degenerate rebuild lock (rebuild_lock.go), and
// the DSN/path resolution + pragma forcing (dsn.go).
//
// MVP / single-node posture (see §A.5): SQLite is single-writer, so the pool is
// pinned to ONE perennial connection — MaxOpenConns=1 (concurrent writes would
// surface SQLITE_BUSY, softened by busy_timeout) plus a non-recyclable connection
// (ConnMaxLifetime=0) so an in-memory database does not evaporate and a file
// database does not re-pay the forced pragmas on every reconnect.
package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"

	"github.com/XSAM/otelsql"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	_ "modernc.org/sqlite"

	"github.com/ClaudioSchirmer/omnicore/infra/audit"
	"github.com/ClaudioSchirmer/omnicore/infra/db/command/write"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/events"
)

// dbSystemSQLite tags traced statements with db.system=sqlite (semconv v1.26.0
// has no dedicated DBSystemSqlite constant, so it is built from DBSystemKey).
var dbSystemSQLite = semconv.DBSystemKey.String("sqlite")

// init registers the SQLite engine under the "sqlite" dialect,
// database/sql-style. Because this file is behind the `sqlite` build tag, a
// build without it never runs it — the dialect is simply absent and
// core.NewEngine("sqlite", …) returns the "no engine registered" error.
func init() {
	core.RegisterEngine("sqlite", New)
}

// Engine is the SQLite RelationalEngine. It holds a *sql.DB pool pinned to one
// perennial connection. The cross-engine write-path state + orchestration live
// on the embedded write.BaseEngine — configured once at boot via WithAudit/
// WithEventPublisher; zero values disable audit/publishing.
type Engine struct {
	write.BaseEngine
	db *sql.DB
}

// Compile-time proof the engine satisfies the framework port and the neutral
// write-TX beginner.
var _ core.RelationalEngine = (*Engine)(nil)
var _ core.WriteBeginner = (*Engine)(nil)

// sqlExecutor is the minimal database/sql execution surface the querier
// (read.go) and the rebuild lock run statements through. *sql.DB satisfies it.
type sqlExecutor interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// New opens the SQLite database and verifies it with a Ping. cfg.DSN is the
// relational.dsn value — a file path (resolved next to the binary and created if
// absent) or ":memory:", with the correctness pragmas forced in (see dsn.go).
// When tracing is on, the pool is opened through otelsql so every statement
// emits a span; when off, a plain sql.Open keeps it untraced.
func New(ctx context.Context, cfg core.EngineConfig) (core.RelationalEngine, error) {
	dsn, err := resolveDSN(cfg.DSN)
	if err != nil {
		return nil, err
	}
	var sqlDB *sql.DB
	if cfg.Tracing {
		sqlDB, err = otelsql.Open("sqlite", dsn, otelsql.WithAttributes(dbSystemSQLite))
	} else {
		sqlDB, err = sql.Open("sqlite", dsn)
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: open: %w", err)
	}
	// MVP / single-node posture: pin ONE perennial connection (see §A.5). The
	// cfg.Pool knobs from bootstrap are deliberately ignored here — SQLite is
	// single-writer, so a wider pool only manufactures SQLITE_BUSY, and a
	// recyclable connection would drop an in-memory database mid-process.
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	sqlDB.SetConnMaxLifetime(0)
	sqlDB.SetConnMaxIdleTime(0)
	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("sqlite: ping: %w", err)
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
// core.WriteTx — the SQLite side of core.WriteBeginner. The concrete *sql.Tx
// stays private behind sqliteTx.
func (e *Engine) Begin(ctx context.Context) (core.WriteTx, error) {
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	return sqliteTx{tx: tx}, nil
}

// WithAudit configures the audit surface (delegating to the embedded
// BaseEngine). nil cfg disables audit; nil logger falls back to slog.Default().
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

// quoteIdent renders a SQL identifier double-quoted ("users", "created_at")
// after validating it against the framework's allowlist (identifiers come from
// schema declarations, never user input). Double-quote is SQL-standard on
// SQLite. Panics on a bad identifier — a programming error in the schema,
// surfaced loudly. Consumed by sqliteDialect (dialect.go).
func quoteIdent(name string) string {
	if !core.SafeIdentifier(name) {
		panic(fmt.Sprintf("sqlite: invalid SQL identifier %q", name))
	}
	return `"` + name + `"`
}
