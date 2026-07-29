//go:build oracle

// Package oracle is the Oracle Database implementation of core.RelationalEngine.
// It ships behind the `oracle` build tag so a build without it never compiles
// this package nor links go-ora. The composition root selects it via
// relational.dialect: oracle; the engine self-registers in init() below.
//
// Scope: this package is the Oracle DIALECT + DRIVER adapter only. The
// backend-neutral persistence orchestration — the write path (flat + aggregate:
// Insert/Update/Archive/Unarchive/Delete + Batch, the child state machine, the
// cascade, the outbox/audit rows, and the A/D lifecycle hooks) and the audit/
// event wiring — lives once in infra/db on the embedded write.BaseEngine and runs
// against the neutral core.WriteTx this engine opens via Begin. What stays here
// is genuinely dialect-bound: the *sql.DB pool + registration (this file), the
// oracleDialect (:n placeholders, bare identifiers, UUID ⇄ RAW(16) codec,
// ORA-00001/02291/02292 classification, LOWER LIKE, MERGE upsert, SYSTIMESTAMP,
// FETCH FIRST row cap) and the neutral cursor in read.go, the tx seal + WriteTx
// adapter in tx_handle.go, the rebuild lock in rebuild_lock.go, and the DSN
// guard in dsn.go. The file layout mirrors infra/db/engine/sqlserver one-to-one.
//
// Design decisions locked for the fourth backend (see tasks/oracle.md):
//   - The compatibility floor is Oracle Database 23ai: booleans are native
//     BOOLEAN columns and JSON payloads native JSON columns — earlier releases
//     are not supported.
//   - Identity is stored RAW(16): RAW compares bytewise, so the UUIDv7 ids
//     minted in Go keep the ID index append-friendly (the BINARY(16)/InnoDB
//     rationale, verified against a live Oracle Free 23ai).
//   - Identifiers are emitted QUOTED-UPPERCASE — equivalent by construction to
//     the platform's unquoted resolution (the catalog folds unquoted names to
//     uppercase), so manual queries stay natural, while identifiers colliding
//     with Oracle reserved words (e.g. a `number` column) work with no
//     reserved-word list. The case normalization back to the declared
//     lowercase names is engine-internal: QueryMaps lowercases result-set
//     column keys and the error classifiers lowercase extracted constraint
//     names.
//   - The upsert is a single MERGE statement. Oracle has no HOLDLOCK
//     equivalent, so a concurrent MERGE can surface ORA-00001 between the match
//     probe and the insert — classified as a unique violation, the callers'
//     existing conflict path (proven against a live 23ai).
//   - NowExpr is SYSTIMESTAMP — server-timezone parity with NOW() on PG/MySQL
//     (CURRENT_TIMESTAMP is session-TZ on Oracle, which would make this the one
//     session-relative dialect).
//   - The row cap is a tail FETCH FIRST n ROWS ONLY (valid without ORDER BY).
//
// Per-statement OpenTelemetry tracing is wired through otelsql when tracing is
// enabled (see New). Manual loader scanners run on this engine too: they
// receive the backend-neutral core.Row/core.Rows.
package oracle

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"

	"github.com/XSAM/otelsql"
	"github.com/google/uuid"
	_ "github.com/sijms/go-ora/v2"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"

	"github.com/ClaudioSchirmer/omnicore/infra/audit"
	"github.com/ClaudioSchirmer/omnicore/infra/db/command/write"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/events"
)

// init registers the Oracle engine under the "oracle" dialect,
// database/sql-style. Because this file is behind the `oracle` build tag, a
// build without it never runs it — the dialect is simply absent and
// core.NewEngine("oracle", …) returns the "no engine registered (build with
// the engine's build tag?)" error.
func init() {
	core.RegisterEngine("oracle", New)
}

// Engine is the Oracle RelationalEngine. It holds a *sql.DB pool; construction
// is lazy on the connection (Ping verifies reachability at New). The
// cross-engine write-path state + orchestration live on the embedded
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

// New opens the Oracle pool and verifies it with a Ping. The DSN is go-ora's
// URL form (`oracle://user:password@host:port/service`) with one framework
// guard applied first: ensureLobFetch (dsn.go) forces the `lob fetch=post`
// option when absent — without it go-ora truncates native-JSON reads at 32 KiB
// (proven against a live 23ai; writes are unaffected). When tracing is on, the
// pool is opened through otelsql so every statement emits a span
// (db.system=oracle); when off, a plain sql.Open keeps the pool untraced and
// pays nothing.
func New(ctx context.Context, cfg core.EngineConfig) (core.RelationalEngine, error) {
	dsn := ensureLobFetch(cfg.DSN)
	var (
		sqlDB *sql.DB
		err   error
	)
	if cfg.Tracing {
		sqlDB, err = otelsql.Open("oracle", dsn, otelsql.WithAttributes(semconv.DBSystemOracle))
	} else {
		sqlDB, err = sql.Open("oracle", dsn)
	}
	if err != nil {
		return nil, fmt.Errorf("oracle: open: %w", err)
	}
	// Bound the pool: database/sql defaults to an UNLIMITED open pool (and only
	// 2 idle), so under a write burst it would open connections without
	// backpressure. A zero MaxOpenConns keeps that unlimited default (explicit
	// opt-in); bootstrap defaults it to max(4, NumCPU) — the same policy as the
	// other engines.
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
		return nil, fmt.Errorf("oracle: ping: %w", err)
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
// core.WriteTx — the Oracle side of core.WriteBeginner. The neutral write
// orchestration (flat + aggregate verbs, outbox, audit, hooks) runs against
// this surface; the concrete *sql.Tx stays private behind oracleTx.
func (e *Engine) Begin(ctx context.Context) (core.WriteTx, error) {
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	return oracleTx{tx: tx}, nil
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

// quoteIdent renders a SQL identifier QUOTED-UPPERCASE ("USERS",
// "CREATED_AT") after validating it against the framework's allowlist (the
// same trust model as the other engines: identifiers come from schema
// declarations, never user input). Quoted-uppercase is deliberately
// EQUIVALENT to the platform's unquoted resolution — an unquoted identifier
// folds to uppercase in the catalog, which is exactly what the quoted form
// names — so manual queries stay natural (`SELECT * FROM users` matches) and
// every D11 property holds; what the quotes add is coverage for identifiers
// that collide with Oracle reserved words (a `number` column is a syntax
// error unquoted, valid as "NUMBER") with NO reserved-word list: one total
// rule for every identifier, the same always-quote philosophy as MySQL's
// backticks and SQL Server's brackets. Panics on a bad identifier — a
// programming error in the schema, surfaced loudly. Consumed by oracleDialect
// (dialect.go) so the shared SQL builders render Oracle-flavored identifiers.
func quoteIdent(name string) string {
	if !core.SafeIdentifier(name) {
		panic(fmt.Sprintf("oracle: invalid SQL identifier %q", name))
	}
	return `"` + strings.ToUpper(name) + `"`
}

// uuidBytes parses a UUID string into its 16-byte RAW(16) form. The framework's
// IDs are UUIDs; a non-UUID id is a wiring error. Consumed by
// oracleDialect.EncodeArg (dialect.go) to render ids/UUID values as RAW(16).
func uuidBytes(id string) ([]byte, error) {
	u, err := uuid.Parse(id)
	if err != nil {
		return nil, fmt.Errorf("oracle: id %q is not a UUID: %w", id, err)
	}
	return u[:], nil
}
