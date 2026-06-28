package pg

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ClaudioSchirmer/omnicore/infra/db"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Postgres implementation of the read seam (read.go): db.Querier wraps the pgx
// pool; db.Dialect renders the pgx flavor ($n placeholders, bare identifiers, uuid
// args as text). The criteria translator + db.AggregateLoader consume only the
// neutral interfaces; this file is the single place pgx leaks into the read path.

// pgRows adapts pgx.Rows to infra.Rows. The only mismatch is Close: pgx.Rows.Close
// returns nothing, infra.Rows.Close returns error — the adapter swallows it (pgx
// surfaces row errors via Err(), checked separately by the loader).
type pgRows struct{ pgx.Rows }

func (r pgRows) Close() error { r.Rows.Close(); return nil }

// pgQuerier adapts a pgExec (the pool) to infra.Querier.
type pgQuerier struct{ e pgExec }

func (q pgQuerier) Query(ctx context.Context, sql string, args ...any) (db.Rows, error) {
	rows, err := q.e.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgRows{rows}, nil
}

func (q pgQuerier) QueryRow(ctx context.Context, sql string, args ...any) db.Row {
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
func (p *Postgres) Querier() db.Querier { return pgQuerier{e: p.querier()} }

// Dialect returns the Postgres statement flavor.
func (p *Postgres) Dialect() db.Dialect { return pgDialect{} }

// pgDialect renders Postgres SQL: $n placeholders, bare (validated) identifiers,
// uuid args as text, and a native ILIKE.
type pgDialect struct{}

func (pgDialect) Placeholder(n int) string            { return fmt.Sprintf("$%d", n) }
func (pgDialect) QuoteIdent(name string) string       { return validIdentifier(name) }
func (pgDialect) EncodeArg(val any) any               { return normalizeArg(val) }
func (pgDialect) DecodeID(raw string) (string, error) { return raw, nil }
func (pgDialect) ILikeClause(col, ph string) string   { return col + " ILIKE " + ph }

// IsUniqueViolation reads PG SQLSTATE 23505 and returns the violated
// constraint/index name.
func (pgDialect) IsUniqueViolation(err error) (string, bool) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return pgErr.ConstraintName, true
	}
	return "", false
}

// BuildUpsert renders the Postgres upsert: `INSERT … VALUES … ON CONFLICT
// (conflictCols) DO UPDATE SET …` (or `DO NOTHING` when sets is empty). The
// proposed value for an db.UpsertSetNew assignment is `EXCLUDED.col`.
func (d pgDialect) BuildUpsert(table string, cols, conflictCols []string, sets []db.UpsertSet) string {
	var b strings.Builder
	writeInsertHead(&b, d, table, cols)
	b.WriteString(" ON CONFLICT (")
	for i, c := range conflictCols {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(d.QuoteIdent(c))
	}
	b.WriteString(")")
	if len(sets) == 0 {
		b.WriteString(" DO NOTHING")
		return b.String()
	}
	b.WriteString(" DO UPDATE SET ")
	for i, s := range sets {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(d.QuoteIdent(s.Col))
		b.WriteString(" = ")
		if s.Mode == db.UpsertSetNew {
			b.WriteString("EXCLUDED.")
			b.WriteString(d.QuoteIdent(s.Col))
		} else {
			b.WriteString(s.Expr)
		}
	}
	return b.String()
}

// writeInsertHead renders the dialect-neutral `INSERT INTO t (cols) VALUES (ph…)`
// prefix shared by every upsert (the placeholders come from the dialect, so it
// reads `$n` on PG and `?` on MySQL). Exported-package-local — the MySQL engine
// builds its own head in its own package (the mirror stays self-contained).
func writeInsertHead(b *strings.Builder, d db.Dialect, table string, cols []string) {
	b.WriteString("INSERT INTO ")
	b.WriteString(d.QuoteIdent(table))
	b.WriteString(" (")
	for i, c := range cols {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(d.QuoteIdent(c))
	}
	b.WriteString(") VALUES (")
	for i := range cols {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(d.Placeholder(i + 1))
	}
	b.WriteString(")")
}
