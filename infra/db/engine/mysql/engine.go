//go:build mysql

// Package mysql is the MySQL implementation of core.RelationalEngine. It ships
// behind the `mysql` build tag so a Postgres-only build never compiles it nor
// links the go-sql-driver. The composition root selects it via
// database.dialect: mysql; the engine self-registers in init() below.
//
// Scope: this package is the MySQL DIALECT + DRIVER adapter only. The
// backend-neutral persistence orchestration — the write path (flat + aggregate:
// Insert/Update/Archive/Unarchive/Delete + Batch, the child state machine, the
// cascade, the outbox/audit rows, and the A/D lifecycle hooks) and the audit/
// event wiring — lives once in infra/db on the embedded write.BaseEngine and runs
// against the neutral core.WriteTx this engine opens via Begin. What stays here is
// genuinely dialect-bound: the *sql.DB pool + DSN flags + registration (this
// file), the mysqlDialect (placeholders, backtick quoting, UUID⇄BINARY(16)
// codec, errno 1062 classification, ILIKE, upsert) and the neutral cursor in
// read.go, the tx seal + WriteTx adapter in tx_handle.go, and the rebuild lock in
// rebuild_lock.go. The file layout mirrors infra/db/pg one-to-one (engine.go /
// read.go / tx_handle.go / rebuild_lock.go).
//
// Design choices locked for the second backend (see tasks/mysql_pluggable_backend.md
// and tasks/equalizacao_db.md):
//   - PK + every new id is a UUID v7 generated in Go (no RETURNING, no
//     gen_random_uuid) and stored as BINARY(16) — time-ordered so the InnoDB
//     clustered PK stays local. The id generation + INSERT shape are now shared
//     in infra/db; this package only supplies the BINARY(16) value codec.
//   - Placeholders are `?`, identifiers are backtick-quoted.
//   - The value codec (Dialect.EncodeArg/DecodeID) converts UUIDs ⇄ BINARY(16).
//
// Per-statement OpenTelemetry tracing is wired through otelsql when tracing is
// enabled (see New). Manual loader scanners run on this engine too: they receive
// the backend-neutral core.Row/core.Rows.
package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"

	"github.com/XSAM/otelsql"
	gomysql "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"

	"github.com/ClaudioSchirmer/omnicore/infra/audit"
	"github.com/ClaudioSchirmer/omnicore/infra/db/command/write"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/events"
)

// init registers the MySQL engine under the "mysql" dialect, database/sql-style.
// Because this file is behind the `mysql` build tag, a Postgres-only build never
// runs it — the dialect is simply absent and core.NewEngine("mysql", …) returns
// the "no engine registered (build with the engine's build tag?)" error. A
// consumer enabling MySQL blank-imports this package (or builds with -tags mysql)
// so the init lands the factory before bootstrap looks it up.
func init() {
	core.RegisterEngine("mysql", New)
}

// Engine is the MySQL RelationalEngine. It holds a *sql.DB pool; construction is
// lazy on the connection (Ping verifies reachability at New, like the PG path).
// The cross-engine write-path state + orchestration (audit row, post-commit
// echo/publish, lifecycle-hook dispatch, every write verb) live on the embedded
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

// sqlExecutor is the minimal database/sql execution surface the querier (read.go)
// runs statements through — the MySQL twin of pg's pgExec. Both *sql.DB (the
// pool) and *sql.Conn (a pinned session, used by the rebuild lock so status
// writes share the lock's connection) satisfy it, so one querier serves both the
// pooled read path and the pinned-session one.
type sqlExecutor interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// New opens the MySQL pool and verifies it with a Ping. When tracing is on, the
// pool is opened through otelsql — the database/sql counterpart of the Postgres
// path's otelpgx — so every statement emits a span under the global tracer
// provider bootstrap installed (db.system=mysql on each span). When off, a plain
// sql.Open keeps the pool untraced and pays nothing.
func New(ctx context.Context, dsn string, tracing bool) (core.RelationalEngine, error) {
	dsn, err := EnsureDSNParams(dsn)
	if err != nil {
		return nil, err
	}
	var sqlDB *sql.DB
	if tracing {
		sqlDB, err = otelsql.Open("mysql", dsn, otelsql.WithAttributes(semconv.DBSystemMySQL))
	} else {
		sqlDB, err = sql.Open("mysql", dsn)
	}
	if err != nil {
		return nil, fmt.Errorf("mysql: open: %w", err)
	}
	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("mysql: ping: %w", err)
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
// core.WriteTx — the MySQL side of core.WriteBeginner. The neutral write
// orchestration (flat + aggregate verbs, outbox, audit, hooks) runs against this
// surface; the concrete *sql.Tx stays private behind mysqlTx.
func (e *Engine) Begin(ctx context.Context) (core.WriteTx, error) {
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	return mysqlTx{tx: tx}, nil
}

// WithAudit configures the audit surface (in-TX audit_events row routed by
// cfg.Destinations + post-commit slog echo), delegating to the embedded
// BaseEngine. nil cfg disables audit; nil logger falls back to slog.Default() in
// the echo path. Returns the engine as the neutral RelationalEngine so the
// composition root wires it without a dialect branch — the mirror of postgres.WithAudit.
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

// quoteIdent backtick-quotes a SQL identifier after validating it against the
// framework's allowlist (the same trust model as the PG path: identifiers come
// from schema declarations, never user input). Panics on a bad identifier — a
// programming error in the schema, surfaced loudly. Consumed by mysqlDialect
// (read.go) so the shared SQL builders render MySQL-flavored identifiers.
func quoteIdent(name string) string {
	if !core.SafeIdentifier(name) {
		panic(fmt.Sprintf("mysql: invalid SQL identifier %q", name))
	}
	return "`" + name + "`"
}

// uuidBytes parses a UUID string into its 16-byte BINARY(16) form. The
// framework's IDs are UUIDs; a non-UUID id is a wiring error. Consumed by
// mysqlDialect.EncodeArg (read.go) to render ids/UUID values as BINARY(16).
func uuidBytes(id string) ([]byte, error) {
	u, err := uuid.Parse(id)
	if err != nil {
		return nil, fmt.Errorf("mysql: id %q is not a UUID: %w", id, err)
	}
	return u[:], nil
}

// EnsureDSNParams normalizes the MySQL DSN for the live application pool so the
// framework's hard requirements hold no matter how the operator wrote the
// connection string (the same DSN is reused for every dialect; Postgres needs
// none of these, so the engine cannot depend on the operator remembering
// MySQL-specific flags):
//
//   - parseTime=true — DATETIME/TIMESTAMP columns must scan into time.Time (the
//     Mongo-view registry reads and any timestamp entity read do exactly that);
//     without it the driver yields []byte and the scan errors at runtime.
//   - clientFoundRows=true — RowsAffected must report MATCHED rows, not only
//     changed ones. The write path reads RowsAffected on UPDATE to detect a
//     missing row (RecordNotFound, mirroring Postgres's UPDATE ... RETURNING);
//     without this flag a no-op UPDATE of an existing row (same values, same
//     updated_at second) reports zero rows and is wrongly seen as not-found.
//   - multiStatements=FALSE — forced OFF on the data pool. Stacked statements are
//     only needed by the migration runner (see EnsureMigrationDSNParams); leaving
//     them enabled on the live pool would widen the SQL-injection blast radius
//     (a single injected `;DROP …` could run as a second statement) for no
//     benefit — Postgres runs its data pool without stacked execution too.
//
// Each is forced (overriding any conflicting operator value) because the engine
// cannot function correctly — or safely — otherwise.
func EnsureDSNParams(dsn string) (string, error) {
	return ensureDSNParams(dsn, false)
}

// EnsureMigrationDSNParams is the migration-runner variant: identical to
// EnsureDSNParams but with multiStatements=TRUE, because the embedded framework
// migration 0001_framework is a multi-statement script run through
// golang-migrate (without it the second statement aborts with a syntax error and
// boot fails at migration time). Scoping stacked statements to the migration
// connection keeps them out of the request-serving pool.
func EnsureMigrationDSNParams(dsn string) (string, error) {
	return ensureDSNParams(dsn, true)
}

func ensureDSNParams(dsn string, multiStatements bool) (string, error) {
	cfg, err := gomysql.ParseDSN(dsn)
	if err != nil {
		return "", fmt.Errorf("mysql: parse dsn: %w", err)
	}
	cfg.MultiStatements = multiStatements
	cfg.ParseTime = true
	cfg.ClientFoundRows = true
	return cfg.FormatDSN(), nil
}
