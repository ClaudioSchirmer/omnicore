//go:build oracle

package oracle

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// Oracle implementation of the framework read seam. *sql.Rows satisfies
// core.Rows directly; the dialect renders Oracle's :n placeholders,
// quoted-uppercase identifiers, and the RAW(16) ⇄ uuid codec. The sqlExecutor
// surface the querier runs through is the engine's driver-exec interface
// (engine.go), the Oracle twin of the SQL Server one.
//
// Two Oracle-wide read concerns live here:
//
//  1. The D11 case normalization. Identifiers are stored UPPERCASE in the
//     catalog, so go-ora hands result-set column names back uppercase;
//     QueryMaps lowercases the map keys so the composer joins on the declared
//     lowercase names. The typed Scan path is positional and needs no name
//     normalization.
//
//  2. CONTEXT-BOUNDED WAITS. go-ora's cancellation sends the Oracle break
//     marker over the SAME connection and then keeps blocking on the socket
//     read (the driver's ctx→read-deadline plumbing is commented out
//     upstream), so against a FROZEN database — paused container, network
//     partition, hung server — a context deadline is never honored: the call
//     returns only when the network thaws. The other engines' drivers close
//     the socket on expiry and fail fast. To keep the framework's deadline
//     promises on Oracle (the 2s readiness probe, the request-timeout 504),
//     every Querier method runs the driver call in a goroutine and returns
//     ctx.Err() the moment the context expires; the abandoned call finishes
//     on its own when the network recovers and releases its pooled
//     connection then (self-healing, bounded by the pool cap — each probe or
//     request abandoned during a freeze parks one connection until the thaw).
//     The WRITE path (core.WriteTx) is deliberately NOT wrapped: abandoning a
//     statement mid-transaction without knowing whether it applied would
//     change delivery semantics (a caller retry could double-apply); go-ora's
//     `TIMEOUT=<seconds>` DSN option (a socket read deadline) is the
//     operator-level mitigation there, documented in the manual.

type oracleQuerier struct{ exec sqlExecutor }

func (q oracleQuerier) Query(ctx context.Context, sqlText string, args ...any) (core.Rows, error) {
	type result struct {
		rows *sql.Rows
		err  error
	}
	ch := make(chan result) // unbuffered: the try-send below detects abandonment
	go func() {
		rows, err := q.exec.QueryContext(ctx, sqlText, args...)
		select {
		case ch <- result{rows, err}:
		default: // abandoned — nobody will consume; release the connection
			if rows != nil {
				_ = rows.Close()
			}
		}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, r.err
		}
		return &rawDecodingRows{Rows: r.rows}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// rawDecodingRows wraps *sql.Rows so the typed scan restores RAW(16) uuid
// columns to their canonical string form. database/sql copies a RAW(16) value
// into a *string dest as 16 raw bytes; without this a secondary uuid column
// scanned into the framework's canonical Go string (a cross-aggregate
// reference such as a BuyerID/TenantID, not the leading PK/FK the loader
// already DecodeID's) would carry garbage. Detection is by driver column type
// (RAW) + value length 16 — the same guard QueryMaps / normalizeOracleValue
// use — so a 16-char text column (VARCHAR2, never RAW) is never misread. Only
// *string dests are rewritten; a *[]byte / *uuid.UUID / sql.Scanner dest
// receives the raw form. The SQL Server engine carries the identical wrapper
// for its BINARY(16).
type rawDecodingRows struct {
	*sql.Rows
	colTypes []*sql.ColumnType
	resolved bool
}

func (r *rawDecodingRows) Scan(dest ...any) error {
	if !r.resolved {
		r.colTypes, _ = r.ColumnTypes()
		r.resolved = true
	}
	scanDest := make([]any, len(dest))
	var fixups []func() error
	for i, d := range dest {
		if sp, ok := d.(*string); ok && i < len(r.colTypes) && r.colTypes[i].DatabaseTypeName() == "RAW" {
			raw := new([]byte)
			out := sp
			scanDest[i] = raw
			fixups = append(fixups, func() error {
				b := *raw
				if len(b) == 16 {
					u, err := uuid.FromBytes(b)
					if err != nil {
						return fmt.Errorf("oracle: decoding RAW(16) column: %w", err)
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

// QueryRow returns a Row whose Scan is the context-bounded leg: *sql.Row
// defers the actual execution to Scan, so wrapping QueryRowContext alone
// would bound nothing. The bounded Query above runs the statement; the
// adapter reproduces database/sql's Row semantics (first row or
// sql.ErrNoRows — the sentinel the no-rows consumers already match). The
// abandoned-goroutine path scans into PRIVATE buffers only (the caller's
// dest pointers are never written after an early return).
func (q oracleQuerier) QueryRow(ctx context.Context, sqlText string, args ...any) core.Row {
	rows, err := q.Query(ctx, sqlText, args...)
	return oracleRow{rows: rows, err: err}
}

// oracleRow adapts the bounded Query result to the single-row core.Row
// surface. Scan consumes the first row and closes the cursor; an empty result
// is sql.ErrNoRows, mirroring *sql.Row.
type oracleRow struct {
	rows core.Rows
	err  error
}

func (r oracleRow) Scan(dest ...any) error {
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

func (q oracleQuerier) Exec(ctx context.Context, sqlText string, args ...any) error {
	ch := make(chan error) // unbuffered: the try-send below detects abandonment
	go func() {
		_, err := q.exec.ExecContext(ctx, sqlText, args...)
		select {
		case ch <- err:
		default: // abandoned — the statement may still apply when the network
			// thaws; the Querier's Exec consumers are the control-plane
			// side-channels, whose statements are idempotent upserts by design.
		}
	}()
	select {
	case err := <-ch:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// QueryMaps runs a SELECT and returns each row column-keyed, the dynamic read
// the composer uses (the column set is discovered at read time). Columns are
// scanned into a []any of interface pointers and normalized per column type:
//   - the map KEY is the column name LOWERCASED (D11: the catalog folds the
//     unquoted declared names to uppercase; the composer joins on the declared
//     lowercase names);
//   - a RAW(16) value (a 16-byte uuid) → canonical uuid string, matching the
//     string ids the composer joins on and Mongo stores;
//   - a NUMBER value — go-ora yields EVERY NumericValue as a string, and its
//     metadata cannot tell an INT-shaped NUMBER(19) from a BOOLEAN or a COUNT
//     (all report NUMBER) — is parsed to int64 when it is a whole number
//     (restoring the int64 the other engines yield for integer columns and
//     COUNTs; a native-BOOLEAN "1"/"0" lands as int64 and the composer's
//     schema-driven bool coercion restores the bool), and stays the canonical
//     text form otherwise (the DECIMAL/SUM parity with MySQL/SQL Server);
//   - any other []byte (native JSON, CLOB/BLOB locators) → string — the same
//     canonical text form the other engines yield;
//   - strings/floats/time.Time/nil pass through (go-ora decodes VARCHAR2/CLOB
//     to string, BINARY_DOUBLE to float64, TIMESTAMP to time.Time natively).
func (q oracleQuerier) QueryMaps(ctx context.Context, sqlText string, args ...any) ([]map[string]any, error) {
	type result struct {
		out []map[string]any
		err error
	}
	ch := make(chan result) // unbuffered: the try-send below detects abandonment
	go func() {
		out, err := q.queryMapsBlocking(ctx, sqlText, args...)
		select {
		case ch <- result{out, err}:
		default: // abandoned — the private buffers below are simply discarded
		}
	}()
	select {
	case r := <-ch:
		return r.out, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// queryMapsBlocking is the driver-facing body of QueryMaps — everything it
// touches (rows, cells, the result slice) is private to the call, so an
// abandoned run races with nothing and its deferred Close releases the
// connection when the network thaws.
func (q oracleQuerier) queryMapsBlocking(ctx context.Context, sqlText string, args ...any) ([]map[string]any, error) {
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
			v, err := normalizeOracleValue(cells[i], colTypes[i].DatabaseTypeName())
			if err != nil {
				return nil, fmt.Errorf("oracle: column %q: %w", name, err)
			}
			m[strings.ToLower(name)] = v
		}
		result = append(result, m)
	}
	return result, rows.Err()
}

// normalizeOracleValue rewrites a scanned cell into the canonical Go form the
// composer/BSON expect: RAW(16) → uuid string, whole-number NUMBER text →
// int64, other []byte → string, the rest unchanged. dbType is the driver's
// DatabaseTypeName ("RAW", "NUMBER", "NCHAR", "OCIClobLocator", …) used to
// disambiguate a 16-byte uuid from a coincidental 16-byte value and a NUMBER
// from a genuine text column.
func normalizeOracleValue(v any, dbType string) (any, error) {
	switch c := v.(type) {
	case []byte:
		if dbType == "RAW" && len(c) == 16 {
			u, err := uuid.FromBytes(c)
			if err != nil {
				return nil, fmt.Errorf("decoding RAW(16): %w", err)
			}
			return u.String(), nil
		}
		return string(c), nil
	case string:
		if dbType == "NUMBER" {
			if n, err := strconv.ParseInt(c, 10, 64); err == nil {
				return n, nil
			}
		}
		return c, nil
	default:
		return v, nil
	}
}

// Querier exposes the pool through the neutral read surface.
func (e *Engine) Querier() core.Querier { return oracleQuerier{exec: e.db} }
