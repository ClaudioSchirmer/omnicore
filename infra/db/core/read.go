package core

import "context"

// The read seam: backend-neutral interfaces the AggregateLoader (and the
// criteria translator) consume so a live aggregate loads the same way on any
// engine. The Postgres adapter wraps pgx; the MySQL engine wraps database/sql.
// Both are reached through RelationalEngine.Querier() / .Dialect().

// Rows is the neutral multi-row cursor. *sql.Rows satisfies it directly; pgx.Rows
// is adapted (its Close returns nothing) by the Postgres engine.
type Rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
	Close() error
}

// Row is the neutral single-row handle (Scan only). Both pgx.Row and *sql.Row
// satisfy it directly.
type Row interface {
	Scan(dest ...any) error
}

// Querier is the neutral SQL surface the loader, composer, and the
// control-plane side-channels (dedup + failure registries) run statements
// through. Read verbs (Query/QueryRow/QueryMaps) plus a control-plane Exec for
// the framework-owned bookkeeping tables — NOT a path for entity writes (those
// go through RelationalEngine's typed write verbs in their own TX).
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) Row
	// Exec runs a statement that returns no rows (the side-channel INSERT/UPDATE
	// against the framework's bookkeeping tables). Args are bound verbatim — the
	// caller encodes them via the Dialect. The rows-affected count is not
	// surfaced (no caller needs it); the error is.
	Exec(ctx context.Context, sql string, args ...any) error
	// QueryMaps runs a SELECT and returns each row as a column-keyed map, with
	// values normalized to the canonical Go forms the composer expects (uuid
	// columns as strings on every engine, the rest passed through). It is the
	// dynamic-shape read the composer uses: it does not know the column set
	// ahead of time, so the typed Scan path (Query/QueryRow) does not fit. Each
	// engine owns its row→map extraction and value normalization (pgx exposes
	// FieldDescriptions()+Values(); database/sql uses Columns()+ColumnTypes()).
	// Args are bound verbatim — the caller encodes them via Dialect.EncodeArg,
	// exactly as the criteria translator does for the typed path. The seam
	// returns map[string]any (not bson.M) to keep the relational read surface
	// free of any Mongo dependency; the composer converts at its boundary.
	QueryMaps(ctx context.Context, sql string, args ...any) ([]map[string]any, error)
}

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
	// every engine — e.g. "NOW()", "NULL", or "attempt + 1" (a bare column name
	// in an upsert's update clause refers to the existing row on both PG and
	// MySQL). The expression is emitted as-is; framework-controlled, never user
	// input.
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
