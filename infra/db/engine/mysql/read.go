//go:build mysql

package mysql

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/google/uuid"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// MySQL implementation of the framework read seam (infra/read.go). *sql.Rows and
// *sql.Row satisfy core.Rows / core.Row directly (Next/Scan/Err/Close error,
// Scan), so the querier is a thin pass-through; the dialect renders MySQL's `?`
// placeholders, backtick identifiers, and the BINARY(16) ⇄ uuid codec. The
// sqlExecutor surface the querier runs through is the engine's driver-exec
// interface (engine.go), the MySQL twin of pg's pgExec.

type mysqlQuerier struct{ exec sqlExecutor }

func (q mysqlQuerier) Query(ctx context.Context, sqlText string, args ...any) (core.Rows, error) {
	rows, err := q.exec.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, err
	}
	return &binaryDecodingRows{Rows: rows}, nil
}

// binaryDecodingRows wraps *sql.Rows so the typed scan restores BINARY(16) uuid
// columns to their canonical string form. database/sql copies a BINARY(16) value
// into a *string dest as 16 raw bytes; on Postgres pgx formats a uuid column as
// text, so without this a secondary uuid column scanned into the framework's
// canonical Go string (a cross-aggregate reference such as a BuyerID/TenantID,
// not the leading PK/FK the loader already DecodeID's) would carry garbage on
// MySQL. Detection is by driver column type (BINARY) + value length 16 — the same
// heuristic QueryMaps/normalizeMySQLValue use — so a 16-char text column (whose
// type is VARCHAR/CHAR, never BINARY) is never misread. Only *string dests are
// rewritten; a *[]byte / *uuid.UUID / sql.Scanner dest receives the raw form, so
// a manual scanner that wants the bytes scans into one of those.
type binaryDecodingRows struct {
	*sql.Rows
	colTypes []*sql.ColumnType
	resolved bool
}

func (r *binaryDecodingRows) Scan(dest ...any) error {
	if !r.resolved {
		r.colTypes, _ = r.ColumnTypes()
		r.resolved = true
	}
	scanDest := make([]any, len(dest))
	var fixups []func() error
	for i, d := range dest {
		if sp, ok := d.(*string); ok && i < len(r.colTypes) && r.colTypes[i].DatabaseTypeName() == "BINARY" {
			raw := new([]byte)
			out := sp
			scanDest[i] = raw
			fixups = append(fixups, func() error {
				b := *raw
				if len(b) == 16 {
					u, err := uuid.FromBytes(b)
					if err != nil {
						return fmt.Errorf("mysql: decoding BINARY(16) column: %w", err)
					}
					*out = u.String()
					return nil
				}
				*out = string(b)
				return nil
			})
			continue
		}
		scanDest[i] = d
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

func (q mysqlQuerier) QueryRow(ctx context.Context, sqlText string, args ...any) core.Row {
	return q.exec.QueryRowContext(ctx, sqlText, args...)
}

func (q mysqlQuerier) Exec(ctx context.Context, sqlText string, args ...any) error {
	_, err := q.exec.ExecContext(ctx, sqlText, args...)
	return err
}

// QueryMaps runs a SELECT and returns each row column-keyed, the dynamic read
// the composer uses (the column set is discovered at read time). database/sql
// has no Values() like pgx, so columns are scanned into a []any of interface
// pointers and normalized per column type:
//   - a BINARY(16) value (a 16-byte uuid) → canonical uuid string, matching the
//     string ids the composer joins on and Mongo stores (mirrors the PG path,
//     where pgx hands uuid back as [16]byte and normalizeSQLValue restrings it);
//   - any other []byte (text/decimal columns the driver returns raw) → string;
//   - integers/floats/bools/time.Time/nil pass through.
func (q mysqlQuerier) QueryMaps(ctx context.Context, sqlText string, args ...any) ([]map[string]any, error) {
	rows, err := q.exec.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	colTypes, err := rows.ColumnTypes()
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
			v, err := normalizeMySQLValue(cells[i], colTypes[i].DatabaseTypeName())
			if err != nil {
				return nil, fmt.Errorf("mysql: column %q: %w", name, err)
			}
			m[name] = v
		}
		result = append(result, m)
	}
	return result, rows.Err()
}

// normalizeMySQLValue rewrites a scanned cell into the canonical Go form the
// composer/BSON expect: BINARY(16) → uuid string, other []byte → string, the
// rest unchanged. dbType is the driver's DatabaseTypeName ("BINARY", "VARCHAR",
// "INT", "DATETIME", …) used to disambiguate a 16-byte uuid from a coincidental
// 16-char text value.
func normalizeMySQLValue(v any, dbType string) (any, error) {
	b, ok := v.([]byte)
	if !ok {
		return v, nil
	}
	if dbType == "BINARY" && len(b) == 16 {
		u, err := uuid.FromBytes(b)
		if err != nil {
			return nil, fmt.Errorf("decoding BINARY(16): %w", err)
		}
		return u.String(), nil
	}
	return string(b), nil
}

// Querier exposes the pool through the neutral read surface.
func (e *Engine) Querier() core.Querier { return mysqlQuerier{exec: e.db} }
