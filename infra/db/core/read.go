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
