package core

// The dialect seam: every engine-specific fragment of a generated statement —
// read AND write — is rendered through the Dialect interface below, so the
// shared builders (criteria translator, loader, composer, write path, the
// control-plane side-channels and the shared-base orphan purge) are written
// once and each engine supplies its flavor. Lives in its own file because the
// seam long outgrew the read path it started on.

// UpsertSetMode classifies one assignment in an upsert's update clause — the
// only part of an upsert that diverges by dialect (how the proposed value and
// the EXISTING row are referenced).
type UpsertSetMode int

const (
	// UpsertSetNew assigns the column to the value proposed for insertion:
	// `EXCLUDED.col` on Postgres, `new.col` on MySQL, `source.col` in the
	// MERGE dialects.
	UpsertSetNew UpsertSetMode = iota
	// UpsertSetExpr assigns the column to a verbatim SQL expression identical on
	// every engine — e.g. "NULL", or the dialect's NowExpr() for a current
	// timestamp. Emitted as-is; framework-controlled, never user input.
	//
	// The expression must NOT reference a column of the target table: how the
	// existing row is named differs by engine (Postgres requires the table
	// qualifier inside DO UPDATE — a bare column there is ambiguous against
	// EXCLUDED and fails with SQLSTATE 42702; the MERGE dialects alias the
	// table as `target`; MySQL takes it bare). A read-modify assignment rides
	// as UpsertSetBump instead.
	UpsertSetExpr
	// UpsertSetBump assigns the column to the EXISTING row's value plus one —
	// `col = <old-row-reference>.col + 1`, each dialect supplying its own way
	// of naming the old row. This is the increment the failure ledgers use to
	// count attempts on conflict; it exists because no verbatim expression can
	// spell "the existing row's column" portably.
	UpsertSetBump
	// UpsertSetArg assigns the column to a bound ARGUMENT of its own —
	// `col = <placeholder>`. It is the only assignment whose value is not
	// already in the proposed row, and that is exactly what a conflict-ONLY
	// value needs: such a column is absent from the insert half, so there is no
	// `EXCLUDED.col` for UpsertSetNew to read it from.
	//
	// Its placeholders continue the numbering the inserted columns started — the
	// first is len(cols)+1 and they follow the order these assignments appear in
	// — so the caller appends the matching arguments after the inserted ones.
	UpsertSetArg
)

// UpsertSet is one `col = <value>` assignment applied when the natural key
// already exists.
type UpsertSet struct {
	Col  string
	Mode UpsertSetMode
	Expr string // used only when Mode == UpsertSetExpr
}

// Dialect renders the engine-specific bits of a generated statement: the
// positional placeholder, identifier quoting, the value encoding for a bound
// argument, the decode of a scanned leading key, the case-insensitive LIKE
// operator, and the upsert clause. The criteria translator, the loader, the
// composer, and the control-plane side-channels are written once against this
// interface; each engine supplies its flavor (pg ⟷ mysql, side by side).
type Dialect interface {
	// Placeholder renders the n-th positional placeholder (1-based): "$1" on
	// Postgres, "?" on MySQL.
	Placeholder(n int) string
	// QuoteIdent renders a (validated) identifier: bare on Postgres,
	// backtick-quoted on MySQL.
	QuoteIdent(name string) string
	// EncodeArg converts a bound value into the form the driver binds. Notably a
	// domain.ID becomes its string value on Postgres (uuid text) and its 16-byte
	// form on MySQL (BINARY(16)); everything else passes through.
	EncodeArg(val any) any
	// DecodeID converts a scanned leading-key value back to the canonical UUID
	// string: identity on Postgres (pgx already yields the text), 16-bytes →
	// uuid on MySQL.
	DecodeID(raw string) (string, error)
	// ILikeClause renders a case-insensitive LIKE comparison over an
	// already-quoted column and a placeholder. Postgres uses its native
	// `col ILIKE ?` (unconditionally case-insensitive); MySQL renders
	// `LOWER(col) LIKE LOWER(?)` so the match is case-insensitive on ANY column
	// collation (a bare LIKE would be case-insensitive only under a CI collation).
	ILikeClause(col, placeholder string) string
	// LikeClause renders a case-SENSITIVE LIKE comparison over an already-quoted
	// column and a placeholder — the counterpart of ILikeClause, and the sole
	// renderer of criteria.OpLike. A bare `col LIKE ?` is only reliably
	// case-sensitive on Postgres (native) and Oracle (NLS_COMP=BINARY default);
	// on MySQL and SQL Server a bare LIKE is case-INSENSITIVE under the default
	// CI collations, so those engines force byte-exact comparison (MySQL
	// `BINARY col LIKE ?`, SQL Server `col LIKE ? COLLATE Latin1_General_BIN`).
	// SQLite is case-sensitive via the connection's case_sensitive_like pragma.
	// The framework must not depend on how the operator created the database.
	LikeClause(col, placeholder string) string
	// NowExpr renders the engine's SQL expression for the current timestamp —
	// the single source of the "now" literal in every generated statement (the
	// managed created_at/updated_at stamps, the DeletedAt archive stamp, the
	// outbox created_at, the failure registries' attempt/resolved timestamps).
	// "NOW()" on both Postgres and MySQL; each engine supplies its native form,
	// so shared code never bakes in a dialect-specific function name.
	NowExpr() string
	// UTCNowExpr renders the engine's SQL expression for the current instant in
	// UTC, at the highest sub-second precision the engine offers. It is the
	// source of the write operation's authoritative stamp under
	// relational.clock: db (core.NowFrom), read ONCE per write transaction and
	// then bound as an ordinary argument.
	//
	// It is deliberately NOT NowExpr. NowExpr is the bookkeeping stamp of the
	// framework's own control-plane rows (outbox, the failure ledgers), where
	// server-timezone parity across engines was the goal and second granularity
	// is harmless. Neither property survives contact with entity data: NowExpr
	// is MySQL's NOW(), which carries ZERO fractional digits, and SQL Server's
	// CURRENT_TIMESTAMP, which is server-local and rounds to ~3.33 ms. A managed
	// timestamp that inherited either would silently lose the precision the
	// archive/unarchive discriminator compares on, and would mix timezones
	// across a fleet. This expression fixes both axes: always UTC, always the
	// engine's finest resolution.
	UTCNowExpr() string
	// ApplyLimit caps a complete SELECT statement at n rows, rendered in the
	// dialect's native position: Postgres and MySQL append ` LIMIT n`; an
	// engine whose cap is not a tail clause (e.g. a SELECT-head TOP) rewrites
	// the statement instead — which is why the method takes the whole SELECT,
	// not a fragment. sql is always framework-generated (never user input);
	// n must be positive.
	ApplyLimit(sql string, n int) string
	// ApplyLimitOffset renders a windowed page — limit rows after skipping
	// offset — onto a complete SELECT. Unlike ApplyLimit (a bare cap usable
	// without an ORDER BY, which is why existence probes rely on it), a non-zero
	// offset is only defined over deterministic order, and SQL Server's only
	// offset form (OFFSET…FETCH) mandates an ORDER BY — so the caller guarantees
	// the statement carries one and that limit and offset are both positive. sql
	// is always framework-generated (never user input). Postgres and MySQL append
	// `LIMIT n OFFSET m`; Oracle and SQL Server append the standard
	// `OFFSET m ROWS FETCH NEXT n ROWS ONLY` tail (not the SELECT-head TOP — TOP
	// cannot express a skip).
	ApplyLimitOffset(sql string, limit, offset int) string
	// Savepoint / RollbackToSavepoint / ReleaseSavepoint render the in-TX
	// savepoint statements for a (validated, framework-owned) name. Postgres
	// and MySQL speak the standard forms; T-SQL spells them SAVE TRANSACTION /
	// ROLLBACK TRANSACTION — and has NO release statement (a savepoint is
	// discarded at COMMIT), so ReleaseSavepoint returns "" there and the
	// caller MUST skip an empty statement. The shared-base orphan purge (the
	// database-vetoable delete) is the sole consumer.
	Savepoint(name string) string
	RollbackToSavepoint(name string) string
	ReleaseSavepoint(name string) string
	// IsUniqueViolation classifies a driver error as a unique-constraint
	// violation and, when so, returns the violated constraint/index name. PG
	// reads SQLSTATE 23505 + ConstraintName; MySQL reads errno 1062 + the key
	// name from the message. (constraint, true) on a hit, ("", false) otherwise.
	IsUniqueViolation(err error) (string, bool)
	// IsForeignKeyViolation classifies a driver error as a foreign-key
	// violation (a referenced row cannot be deleted/updated, or a reference
	// points at a missing row) and, when so, returns the constraint name. PG
	// reads SQLSTATE 23503 + ConstraintName; MySQL reads errno 1451/1452 + the
	// constraint from the message. (constraint, true) on a hit, ("", false)
	// otherwise. The shared-base orphan purge uses it as the database veto: a
	// referencing table the schema registry does not know about blocks the
	// purge instead of failing the write.
	IsForeignKeyViolation(err error) (string, bool)

	// BuildUpsert renders an INSERT with an on-conflict update clause: cols are
	// the inserted columns (the dialect generates the 1-based placeholders),
	// conflictCols the natural key, sets the update assignments applied when the
	// key already exists. An empty sets renders the do-nothing form (PG
	// `ON CONFLICT (…) DO NOTHING`; MySQL a no-op `ON DUPLICATE KEY UPDATE k=k`).
	// The only divergent assignment is UpsertSetNew (`EXCLUDED.col` ⟷ `new.col`).
	// An UpsertSetArg assignment takes its own placeholder, numbered from
	// len(cols)+1 in the order those assignments appear.
	BuildUpsert(table string, cols, conflictCols []string, sets []UpsertSet) string
}
