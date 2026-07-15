//go:build postgres

package postgres

import (
	"context"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Postgres implementation of the read seam (read.go): core.Querier wraps the pgx
// pool; core.Dialect renders the pgx flavor ($n placeholders, bare identifiers, uuid
// args as text). The criteria translator + read.AggregateLoader consume only the
// neutral interfaces; this file is the single place pgx leaks into the read path.

// pgRows adapts pgx.Rows to infra.Rows. The only mismatch is Close: pgx.Rows.Close
// returns nothing, infra.Rows.Close returns error — the adapter swallows it (pgx
// surfaces row errors via Err(), checked separately by the loader).
type pgRows struct{ pgx.Rows }

func (r pgRows) Close() error { r.Rows.Close(); return nil }

// pgQuerier adapts a pgExec (the pool) to infra.Querier.
type pgQuerier struct{ e pgExec }

func (q pgQuerier) Query(ctx context.Context, sql string, args ...any) (core.Rows, error) {
	rows, err := q.e.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgRows{rows}, nil
}

func (q pgQuerier) QueryRow(ctx context.Context, sql string, args ...any) core.Row {
	return q.e.QueryRow(ctx, sql, args...) // pgx.Row satisfies infra.Row
}

func (q pgQuerier) Exec(ctx context.Context, sql string, args ...any) error {
	_, err := q.e.Exec(ctx, sql, args...)
	return err
}

// QueryMaps runs a SELECT and returns each row column-keyed, consuming pgx's
// FieldDescriptions()+Values() (the column set is discovered at read time, so
// the typed Scan path does not fit — this is the composer's dynamic read).
// pgx hands UUID columns back as a raw [16]byte; normalizeSQLValue rewrites
// those to the canonical string form both the placeholder path and BSON expect.
func (q pgQuerier) QueryMaps(ctx context.Context, sql string, args ...any) ([]map[string]any, error) {
	rows, err := q.e.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return pgxRowsToMaps(rows)
}

func pgxRowsToMaps(rows pgx.Rows) ([]map[string]any, error) {
	fields := rows.FieldDescriptions()
	var result []map[string]any
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return nil, err
		}
		m := make(map[string]any, len(fields))
		for i, f := range fields {
			m[f.Name] = normalizeSQLValue(vals[i])
		}
		result = append(result, m)
	}
	return result, rows.Err()
}

// NormalizeSQLValue is the exported counterpart of normalizeSQLValue, used by
// the admin replay CLI.
func NormalizeSQLValue(v any) any { return normalizeSQLValue(v) }

// normalizeSQLValue rewrites pgx's [16]byte UUID representation into the
// canonical string form both the SQL placeholder path and BSON expect. Other
// types pass through unchanged.
func normalizeSQLValue(v any) any {
	switch t := v.(type) {
	case [16]byte:
		return uuid.UUID(t).String()
	default:
		return v
	}
}

// Querier exposes the pool through the neutral read surface.
func (p *Postgres) Querier() core.Querier { return pgQuerier{e: p.querier()} }
