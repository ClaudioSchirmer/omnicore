package core

// The dialect seam: every engine-specific fragment of a generated statement —
// read AND write — is rendered through the Dialect interface below, so the
// shared builders (criteria translator, loader, composer, write path, the
// control-plane side-channels and the shared-base orphan purge) are written
// once and each engine supplies its flavor. Lives in its own file because the
// seam long outgrew the read path it started on.

// UpsertSetMode classifies one assignment in an upsert's update clause — the
// only part of an upsert that diverges by dialect (how the proposed value is
// referenced). Increment / NOW() / NULL are identical across engines (a bare
// column in the update clause refers to the existing row on both PG and MySQL),
// so they ride as UpsertSetExpr.
type UpsertSetMode int

const (
	// UpsertSetNew assigns the column to the value proposed for insertion:
	// `EXCLUDED.col` on Postgres, `new.col` on MySQL.
	UpsertSetNew UpsertSetMode = iota
	// UpsertSetExpr assigns the column to a verbatim SQL expression identical on
	// every engine — e.g. "NULL" or "attempt + 1" (a bare column name in an
	// upsert's update clause refers to the existing row on both PG and MySQL).
	// A current-timestamp assignment is NOT engine-identical: pass the dialect's
	// NowExpr() as the expression. Emitted as-is; framework-controlled, never
	// user input.
	UpsertSetExpr
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
	// NowExpr renders the engine's SQL expression for the current timestamp —
	// the single source of the "now" literal in every generated statement (the
	// managed created_at/updated_at stamps, the soft-delete archive stamp, the
	// outbox created_at, the failure registries' attempt/resolved timestamps).
	// "NOW()" on both Postgres and MySQL; each engine supplies its native form,
	// so shared code never bakes in a dialect-specific function name.
	NowExpr() string
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
	BuildUpsert(table string, cols, conflictCols []string, sets []UpsertSet) string
}
