//go:build mysql

package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	driver "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"

	"github.com/ClaudioSchirmer/omnicore/domain"
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

// Dialect returns the MySQL statement flavor.
func (e *Engine) Dialect() core.Dialect { return mysqlDialect{} }

type mysqlDialect struct{}

func (mysqlDialect) Placeholder(int) string        { return "?" }
func (mysqlDialect) QuoteIdent(name string) string { return quoteIdent(name) }
func (mysqlDialect) ILikeClause(col, ph string) string {
	// LOWER both sides so the match is case-insensitive on ANY column collation
	// (a bare LIKE is case-insensitive only under a CI collation like
	// utf8mb4_0900_ai_ci, but not under utf8mb4_bin) — Postgres ILIKE parity.
	return "LOWER(" + col + ") LIKE LOWER(" + ph + ")"
}

// IsUniqueViolation reads MySQL errno 1062 and extracts the violated index name
// from the message ("Duplicate entry '…' for key '<key>'"). MySQL 8 prefixes the
// key with the table ("flat_persons.uniq_email"); the bare index name (after the
// last dot) is returned so a ConstraintBinding keyed by the index name matches
// across MySQL versions, mirroring the bare name Postgres reports.
//
// The duplicated value is user-controlled and is NOT escaped in the message, so
// it can itself contain the literal "for key '". The key name is always the
// LAST "for key '" segment (a trusted schema identifier, which never contains
// the marker), so LastIndex locks onto the real key and a crafted value can no
// longer divert the parse.
func (mysqlDialect) IsUniqueViolation(err error) (string, bool) {
	var me *driver.MySQLError
	if !errors.As(err, &me) || me.Number != 1062 {
		return "", false
	}
	const marker = "for key '"
	i := strings.LastIndex(me.Message, marker)
	if i < 0 {
		return "", true // a 1062 without a parseable key still signals a violation
	}
	key := me.Message[i+len(marker):]
	if j := strings.IndexByte(key, '\''); j >= 0 {
		key = key[:j]
	}
	if dot := strings.LastIndexByte(key, '.'); dot >= 0 {
		key = key[dot+1:]
	}
	return key, true
}

// IsForeignKeyViolation reads MySQL errno 1451 ("Cannot delete or update a
// parent row") / 1452 ("Cannot add or update a child row") and extracts the
// violated constraint name from the message segment "CONSTRAINT `<name>`".
// Every part of that message is a trusted schema identifier (no user-controlled
// text rides in it), so a plain Index suffices. A violation without a parseable
// name still classifies.
func (mysqlDialect) IsForeignKeyViolation(err error) (string, bool) {
	var me *driver.MySQLError
	if !errors.As(err, &me) || (me.Number != 1451 && me.Number != 1452) {
		return "", false
	}
	const marker = "CONSTRAINT `"
	i := strings.Index(me.Message, marker)
	if i < 0 {
		return "", true
	}
	name := me.Message[i+len(marker):]
	if j := strings.IndexByte(name, '`'); j >= 0 {
		name = name[:j]
	}
	return name, true
}

// BuildUpsert renders the MySQL upsert: `INSERT … VALUES … AS new ON DUPLICATE
// KEY UPDATE …`. The conflict columns are not named (MySQL keys off the existing
// unique index); they are used only to render the no-op update that expresses a
// do-nothing upsert. The proposed value for an UpsertSetNew assignment is
// `new.col` (the row alias, MySQL 8.0.19+); a bare column in the update clause
// refers to the existing row, identical to Postgres.
func (d mysqlDialect) BuildUpsert(table string, cols, conflictCols []string, sets []core.UpsertSet) string {
	var b strings.Builder
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
		b.WriteString("?")
	}
	b.WriteString(") AS new ON DUPLICATE KEY UPDATE ")
	if len(sets) == 0 {
		// No-op assignment makes ON DUPLICATE KEY a do-nothing (the safe,
		// precise equivalent of PG's DO NOTHING — unlike INSERT IGNORE, which
		// would also swallow unrelated errors). The target is a conflict-key
		// column set to new.<col>: with the `AS new` row alias present, a bare
		// `col = col` is ambiguous (MySQL 8.4 errno 1052), and qualifying with
		// new.<col> is a genuine no-op because the conflicting row matched on
		// that key, so its existing value already equals the proposed one.
		k := d.QuoteIdent(conflictCols[0])
		b.WriteString(k)
		b.WriteString(" = new.")
		b.WriteString(k)
		return b.String()
	}
	for i, s := range sets {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(d.QuoteIdent(s.Col))
		b.WriteString(" = ")
		if s.Mode == core.UpsertSetNew {
			b.WriteString("new.")
			b.WriteString(d.QuoteIdent(s.Col))
		} else {
			b.WriteString(s.Expr)
		}
	}
	return b.String()
}

// EncodeArg binds UUID-shaped values as their 16-byte form so they match a
// BINARY(16) column. A domain.ID and a uuid.UUID encode by type; a raw string is
// encoded only when it is the canonical 36-char hyphenated UUID form (a criteria
// value may arrive as a plain string — e.g. criteria.Eq("BuyerID", idStr) — and
// on Postgres such a string matches a uuid column directly, so MySQL must too).
// The canonical-form restriction is deliberate: uuid.Parse also accepts the
// braces / urn / 32-hex shapes, but those round trip through far fewer real
// columns, so leaving them as text avoids mis-encoding a non-uuid value that
// happens to parse. Everything else passes through.
func (mysqlDialect) EncodeArg(val any) any {
	switch v := val.(type) {
	case domain.ID:
		if b, err := uuidBytes(v.Value()); err == nil {
			return b
		}
		return v.Value()
	case uuid.UUID:
		return v[:]
	case string:
		if len(v) == 36 {
			if u, err := uuid.Parse(v); err == nil {
				return u[:]
			}
		}
		return v
	default:
		return val
	}
}

// DecodeID converts a scanned leading key back to the canonical UUID string. A
// BINARY(16) column scans into a 16-byte string; anything else passes through
// (defensive — e.g. a CHAR id or an already-decoded value).
func (mysqlDialect) DecodeID(raw string) (string, error) {
	if len(raw) == 16 {
		u, err := uuid.FromBytes([]byte(raw))
		if err != nil {
			return "", fmt.Errorf("mysql: decoding BINARY(16) id: %w", err)
		}
		return u.String(), nil
	}
	return raw, nil
}
