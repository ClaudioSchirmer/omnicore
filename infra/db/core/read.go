package core

import (
	"context"
	"fmt"
)

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

// Querier is the neutral READ surface the loader, composer and the consumer's
// own custom reads run statements through. It carries no way to write: entity
// writes go through RelationalEngine's typed verbs, which only accept the
// sealed ValidEntity the domain produced, and the framework's own bookkeeping
// writes go through ExecQuerier below.
//
// The split is deliberate. What a consumer receives from
// RelationalEngine.Querier() is what the manual has always promised — "for
// custom reads" — and nothing else. Statement execution used to ride on this
// same interface, which meant the type handed out for reading also offered a
// way around the write path entirely: no sealed entity, no state signature, no
// revision guard, no outbox row, no audit trail.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) Row
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

// ExecQuerier adds statement execution to the read surface. Every engine's
// querier implements it — the separation is about what the PORT hands out, not
// about capability — and the framework reaches it through Exec below.
//
// It exists for the framework's own control plane: the outbox and audit rows,
// the dedup and failure registries, the view-slot pointers. It is NOT a path
// for entity writes, and a consumer that reaches for it is stepping outside
// every guarantee the write path makes.
type ExecQuerier interface {
	Querier
	// Exec runs a statement that returns no rows. Args are bound verbatim — the
	// caller encodes them via the Dialect. The rows-affected count is not
	// surfaced (no caller needs it); the error is.
	Exec(ctx context.Context, sql string, args ...any) error
}

// ExecCount runs a statement on the framework's open transaction and reports
// how many rows it affected.
//
// It exists for the same reason Exec below does: the neutral Tx handed to in-TX
// code carries Exec but not the count, while every engine's transaction adapter
// implements ExecCount already (WriteTx demands it). A Tx that cannot count is a
// programming error at the composition root, not a runtime condition — hence the
// error names the type rather than degrading silently.
func ExecCount(tx Tx, ctx context.Context, sql string, args ...any) (int64, error) {
	c, ok := tx.(interface {
		ExecCount(ctx context.Context, sql string, args ...any) (int64, error)
	})
	if !ok {
		return 0, fmt.Errorf("db.ExecCount: Tx %T does not report affected rows", tx)
	}
	return c.ExecCount(ctx, sql, args...)
}

// Exec runs a control-plane statement through a Querier that can execute one.
// The framework's own subsystems call this instead of holding an ExecQuerier,
// so the read port stays read-only wherever it is passed around and the widening
// happens at the single point that needs it.
//
// A Querier that cannot execute is a programming error at the composition root,
// not a runtime condition: every engine's querier satisfies ExecQuerier.
func Exec(q Querier, ctx context.Context, sql string, args ...any) error {
	e, ok := q.(ExecQuerier)
	if !ok {
		return fmt.Errorf("db: this Querier cannot execute statements (%T) — the framework's control plane needs an ExecQuerier", q)
	}
	return e.Exec(ctx, sql, args...)
}
