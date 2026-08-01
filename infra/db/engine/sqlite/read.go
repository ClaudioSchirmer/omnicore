//go:build sqlite

package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// SQLite implementation of the framework read seam. modernc.org/sqlite honors
// context cancellation and closes the socket on expiry, so the querier is a thin
// pass-through (no goroutine-bounding like Oracle). The one genuine adaptation is
// TIME: SQLite stores timestamps as TEXT (D4) and database/sql cannot scan a
// string into a *time.Time / sql.NullTime — so the typed-scan path routes those
// dests through sqliteDecodingRows, which parses the RFC3339 (app-clock) and
// strftime (NowExpr) layouts. bool needs no adaptation: modernc converts an
// INTEGER 0/1 into a *bool / sql.NullBool natively (verified). The sqlExecutor
// surface the querier runs through is the engine's driver-exec interface
// (engine.go).

type sqliteQuerier struct{ exec sqlExecutor }

func (q sqliteQuerier) Query(ctx context.Context, sqlText string, args ...any) (core.Rows, error) {
	rows, err := q.exec.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, err
	}
	return &sqliteDecodingRows{Rows: rows}, nil
}

// QueryRow runs the statement through Query and adapts the first row to
// core.Row, so the time-decoding wrapper applies to single-row reads too
// (*sql.Row.Scan would bypass it). Mirrors the Oracle row adapter; an empty
// result is sql.ErrNoRows.
func (q sqliteQuerier) QueryRow(ctx context.Context, sqlText string, args ...any) core.Row {
	rows, err := q.Query(ctx, sqlText, args...)
	return sqliteRow{rows: rows, err: err}
}

func (q sqliteQuerier) Exec(ctx context.Context, sqlText string, args ...any) error {
	_, err := q.exec.ExecContext(ctx, sqlText, args...)
	return err
}

// sqliteRow adapts the wrapped Query result to the single-row core.Row surface.
type sqliteRow struct {
	rows core.Rows
	err  error
}

func (r sqliteRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	defer func() { _ = r.rows.Close() }()
	if !r.rows.Next() {
		if err := r.rows.Err(); err != nil {
			return err
		}
		return sql.ErrNoRows
	}
	return r.rows.Scan(dest...)
}

// sqliteDecodingRows wraps *sql.Rows so the typed scan restores TEXT timestamp
// columns into time destinations. database/sql refuses to scan a driver string
// into a *time.Time / **time.Time / *sql.NullTime; this wrapper scans those
// through an intermediate string and parses the SQLite layouts. Every other dest
// (including *bool, which modernc handles from INTEGER) passes straight through.
type sqliteDecodingRows struct {
	*sql.Rows
}

func (r *sqliteDecodingRows) Scan(dest ...any) error {
	scanDest := make([]any, len(dest))
	var fixups []func() error
	for i, d := range dest {
		switch out := d.(type) {
		case *time.Time:
			raw := new(sql.NullString)
			scanDest[i] = raw
			fixups = append(fixups, func() error {
				if !raw.Valid {
					*out = time.Time{}
					return nil
				}
				t, err := parseSQLiteTime(raw.String)
				if err != nil {
					return err
				}
				*out = t
				return nil
			})
		case **time.Time:
			raw := new(sql.NullString)
			scanDest[i] = raw
			fixups = append(fixups, func() error {
				if !raw.Valid {
					*out = nil
					return nil
				}
				t, err := parseSQLiteTime(raw.String)
				if err != nil {
					return err
				}
				*out = &t
				return nil
			})
		case *sql.NullTime:
			raw := new(sql.NullString)
			scanDest[i] = raw
			fixups = append(fixups, func() error {
				if !raw.Valid {
					*out = sql.NullTime{}
					return nil
				}
				t, err := parseSQLiteTime(raw.String)
				if err != nil {
					return err
				}
				*out = sql.NullTime{Time: t, Valid: true}
				return nil
			})
		default:
			scanDest[i] = d
		}
	}
	if err := r.Rows.Scan(scanDest...); err != nil {
		return err
	}
	for _, fn := range fixups {
		if err := fn(); err != nil {
			return err
		}
	}
	return nil
}

// sqliteTimeLayouts are the TEXT forms a timestamp column can carry: RFC3339Nano
// (Go app-clock values, written by encodeArg — with and without a fractional
// part / zone), the strftime layout NowExpr emits (space-separated, no zone, ms),
// and a date-only form. parseSQLiteTime tries each in order (D4: tolerate both
// the app-clock and the NowExpr layouts).
var sqliteTimeLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02 15:04:05.999999999",
	"2006-01-02 15:04:05",
	"2006-01-02",
}

func parseSQLiteTime(s string) (time.Time, error) {
	for _, layout := range sqliteTimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("sqlite: cannot parse timestamp %q (tried RFC3339 and strftime layouts)", s)
}

// QueryMaps runs a SELECT and returns each row column-keyed, the dynamic read the
// composer uses. modernc yields clean Go scalars (string, int64, float64, []byte
// for BLOB, nil) with no BINARY(16)/RAW(16) id encoding to undo (ids are TEXT) —
// so scanning into []any and reading the cells back is enough. A bool column is
// an INTEGER 0/1 → int64 (the composer's schema-driven bool coercion restores
// it, as on MySQL/Oracle); a timestamp column is TEXT → string (relational views,
// the SQLite read path, never use QueryMaps — the composer/Mongo projection is
// out of scope for SQLite by design).
func (q sqliteQuerier) QueryMaps(ctx context.Context, sqlText string, args ...any) ([]map[string]any, error) {
	rows, err := q.exec.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}

	var result []map[string]any
	for rows.Next() {
		cells := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		m := make(map[string]any, len(cols))
		for i, name := range cols {
			m[name] = cells[i]
		}
		result = append(result, m)
	}
	return result, rows.Err()
}

// Querier exposes the pool through the neutral read surface.
func (e *Engine) Querier() core.Querier { return sqliteQuerier{exec: e.db} }
