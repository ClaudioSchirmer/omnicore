//go:build sqlserver

package sqlserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"
	mssql "github.com/microsoft/go-mssqldb"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// SQL Server implementation of the framework read seam. *sql.Rows and *sql.Row
// satisfy core.Rows / core.Row directly, so the querier is a thin pass-through;
// the dialect renders SQL Server's @pN placeholders, bracket identifiers, and
// the BINARY(16) ⇄ uuid codec. The sqlExecutor surface the querier runs through
// is the engine's driver-exec interface (engine.go), the SQL Server twin of the
// MySQL one.

type sqlserverQuerier struct{ exec sqlExecutor }

func (q sqlserverQuerier) Query(ctx context.Context, sqlText string, args ...any) (core.Rows, error) {
	rows, err := q.exec.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, err
	}
	return &binaryDecodingRows{Rows: rows}, nil
}

// binaryDecodingRows wraps *sql.Rows so the typed scan restores BINARY(16) uuid
// columns to their canonical string form. database/sql copies a BINARY(16)
// value into a *string dest as 16 raw bytes; without this a secondary uuid
// column scanned into the framework's canonical Go string (a cross-aggregate
// reference such as a BuyerID/TenantID, not the leading PK/FK the loader
// already DecodeID's) would carry garbage. Detection is by driver column type
// (BINARY) + value length 16 — the same heuristic QueryMaps /
// normalizeSQLServerValue use — so a 16-char text column (NVARCHAR/NCHAR, never
// BINARY) is never misread. Only *string dests are rewritten; a *[]byte /
// *uuid.UUID / sql.Scanner dest receives the raw form. The MySQL engine carries
// the identical wrapper for the identical reason.
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
						return fmt.Errorf("sqlserver: decoding BINARY(16) column: %w", err)
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

func (q sqlserverQuerier) QueryRow(ctx context.Context, sqlText string, args ...any) core.Row {
	return q.exec.QueryRowContext(ctx, sqlText, args...)
}

func (q sqlserverQuerier) Exec(ctx context.Context, sqlText string, args ...any) error {
	_, err := q.exec.ExecContext(ctx, sqlText, args...)
	return err
}

// QueryMaps runs a SELECT and returns each row column-keyed, the dynamic read
// the composer uses (the column set is discovered at read time). Columns are
// scanned into a []any of interface pointers and normalized per column type:
//   - a BINARY(16) value (a 16-byte uuid) → canonical uuid string, matching the
//     string ids the composer joins on and Mongo stores;
//   - any other []byte (e.g. DECIMAL, which go-mssqldb hands back raw) → string
//     — the same canonical text form the MySQL engine yields, so the read specs
//     normalize identically;
//   - strings/integers/floats/bools/time.Time/nil pass through (go-mssqldb
//     decodes NVARCHAR to string and BIT to bool natively).
func (q sqlserverQuerier) QueryMaps(ctx context.Context, sqlText string, args ...any) ([]map[string]any, error) {
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
			v, err := normalizeSQLServerValue(cells[i], colTypes[i].DatabaseTypeName())
			if err != nil {
				return nil, fmt.Errorf("sqlserver: column %q: %w", name, err)
			}
			m[name] = v
		}
		result = append(result, m)
	}
	return result, rows.Err()
}

// normalizeSQLServerValue rewrites a scanned cell into the canonical Go form
// the composer/BSON expect: BINARY(16) → uuid string, other []byte → string,
// the rest unchanged. dbType is the driver's DatabaseTypeName ("BINARY",
// "NVARCHAR", "INT", "DATETIME2", …) used to disambiguate a 16-byte uuid from a
// coincidental 16-byte binary value.
func normalizeSQLServerValue(v any, dbType string) (any, error) {
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
func (e *Engine) Querier() core.Querier { return sqlserverQuerier{exec: e.db} }

// Dialect returns the SQL Server statement flavor.
func (e *Engine) Dialect() core.Dialect { return sqlserverDialect{} }

type sqlserverDialect struct{}

func (sqlserverDialect) Placeholder(n int) string   { return "@p" + strconv.Itoa(n) }
func (sqlserverDialect) QuoteIdent(n string) string { return quoteIdent(n) }
func (sqlserverDialect) ILikeClause(col, ph string) string {
	// LOWER both sides so the match is case-insensitive on ANY column collation
	// — a bare LIKE is case-insensitive only under the server's default CI
	// collation, and the framework must not depend on how the operator created
	// the database. Postgres ILIKE / MySQL LOWER-LIKE parity.
	return "LOWER(" + col + ") LIKE LOWER(" + ph + ")"
}

func (sqlserverDialect) NowExpr() string { return "CURRENT_TIMESTAMP" }

// ApplyLimit caps a complete SELECT at n rows — on SQL Server the cap is a
// SELECT-head TOP, not a tail clause, so the statement head is rewritten (this
// is exactly why Dialect.ApplyLimit receives the whole statement). The tail
// alternative, OFFSET…FETCH, is not usable: it requires an ORDER BY, and the
// existence probes deliberately carry none. The contract guarantees sql is a
// complete framework-generated SELECT; anything else is a programming error,
// surfaced loudly.
func (sqlserverDialect) ApplyLimit(sqlText string, n int) string {
	const head = "SELECT "
	if !strings.HasPrefix(sqlText, head) {
		panic(fmt.Sprintf("sqlserver: ApplyLimit requires a complete SELECT statement, got %q", sqlText))
	}
	return "SELECT TOP " + strconv.Itoa(n) + " " + sqlText[len(head):]
}

// IsUniqueViolation reads SQL Server error 2627 ("Violation of %s constraint
// '<name>'") / 2601 ("Cannot insert duplicate key row in object '%s' with
// unique index '<name>'") and extracts the violated constraint/index name.
// Unlike MySQL — where the user-controlled duplicate value precedes the key and
// forces a LastIndex parse — SQL Server prints the value AFTER the name ("The
// duplicate key value is (…)"), so the FIRST marker is the trusted one and a
// crafted value cannot divert the parse.
func (sqlserverDialect) IsUniqueViolation(err error) (string, bool) {
	var me mssql.Error
	if !errors.As(err, &me) || (me.Number != 2627 && me.Number != 2601) {
		return "", false
	}
	marker := "constraint '"
	if me.Number == 2601 {
		marker = "unique index '"
	}
	i := strings.Index(me.Message, marker)
	if i < 0 {
		return "", true // a 2627/2601 without a parseable name still signals a violation
	}
	name := me.Message[i+len(marker):]
	if j := strings.IndexByte(name, '\''); j >= 0 {
		name = name[:j]
	}
	return name, true
}

// IsForeignKeyViolation reads SQL Server error 547 ("The %s statement
// conflicted with the %s constraint \"<name>\"") and extracts the violated
// constraint name. The name is printed before any table/column detail and no
// user-controlled text precedes it, so the first marker is trusted. A violation
// without a parseable name still classifies. Powers the shared-base
// orphan-purge veto, like 23503/1451 on the other engines.
func (sqlserverDialect) IsForeignKeyViolation(err error) (string, bool) {
	var me mssql.Error
	if !errors.As(err, &me) || me.Number != 547 {
		return "", false
	}
	const marker = "constraint \""
	i := strings.Index(me.Message, marker)
	if i < 0 {
		return "", true
	}
	name := me.Message[i+len(marker):]
	if j := strings.IndexByte(name, '"'); j >= 0 {
		name = name[:j]
	}
	return name, true
}

// BuildUpsert renders the SQL Server upsert: a single
// `MERGE <table> WITH (HOLDLOCK) … USING (SELECT @pN AS col, …) AS source`
// statement. HOLDLOCK is mandatory — without it MERGE races between the match
// probe and the insert, exactly the window ON CONFLICT/ON DUPLICATE KEY close
// natively on the other engines. The proposed value for an UpsertSetNew
// assignment is `source.col`; a bare column in the UPDATE clause refers to the
// existing row (target scope), identical to the other dialects, so verbatim
// UpsertSetExpr expressions ("attempt + 1", "NULL", the dialect's NowExpr())
// carry the same semantics. Empty sets omit the WHEN MATCHED clause — a true
// do-nothing on conflict. MERGE requires the statement terminator, hence the
// trailing semicolon.
func (d sqlserverDialect) BuildUpsert(table string, cols, conflictCols []string, sets []core.UpsertSet) string {
	var b strings.Builder
	b.WriteString("MERGE ")
	b.WriteString(d.QuoteIdent(table))
	b.WriteString(" WITH (HOLDLOCK) AS target USING (SELECT ")
	for i, c := range cols {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(d.Placeholder(i + 1))
		b.WriteString(" AS ")
		b.WriteString(d.QuoteIdent(c))
	}
	b.WriteString(") AS source ON ")
	for i, k := range conflictCols {
		if i > 0 {
			b.WriteString(" AND ")
		}
		b.WriteString("target.")
		b.WriteString(d.QuoteIdent(k))
		b.WriteString(" = source.")
		b.WriteString(d.QuoteIdent(k))
	}
	if len(sets) > 0 {
		b.WriteString(" WHEN MATCHED THEN UPDATE SET ")
		for i, s := range sets {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(d.QuoteIdent(s.Col))
			b.WriteString(" = ")
			if s.Mode == core.UpsertSetNew {
				b.WriteString("source.")
				b.WriteString(d.QuoteIdent(s.Col))
			} else {
				b.WriteString(s.Expr)
			}
		}
	}
	b.WriteString(" WHEN NOT MATCHED THEN INSERT (")
	for i, c := range cols {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(d.QuoteIdent(c))
	}
	b.WriteString(") VALUES (")
	for i, c := range cols {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString("source.")
		b.WriteString(d.QuoteIdent(c))
	}
	b.WriteString(");")
	return b.String()
}

// EncodeArg binds TYPED id values as their 16-byte form so they match a
// BINARY(16) column: a domain.ID / *domain.ID and a uuid.UUID encode by type —
// carrying the identity type IS the declaration, the same way the scan side
// detects id fields by their Go type. A nil *domain.ID stays nil (SQL NULL). A
// plain string ALWAYS passes through as text, even in canonical uuid shape:
// whether a value is an identity is stated by its TYPE (`domain.ID` field ⇒
// BINARY(16) column, `string` field ⇒ NVARCHAR column), never guessed from the
// value's shape. (A domain.ID wrapping a non-parseable string degrades to its
// text value — the column, not the codec, rejects it.)
//
// One SQL Server-only case: json.RawMessage binds as a string. The JSON column
// shape here is NVARCHAR(MAX), and SQL Server does not implicitly convert a
// varbinary parameter (the driver's mapping for []byte) into NVARCHAR — the
// bind would error where MySQL's TEXT and PG's JSONB accept bytes. Plain
// []byte stays bytes (VARBINARY(MAX) is its column shape).
func (sqlserverDialect) EncodeArg(val any) any {
	switch v := val.(type) {
	case domain.ID:
		if b, err := uuidBytes(v.Value()); err == nil {
			return b
		}
		return v.Value()
	case *domain.ID:
		if v == nil {
			// A TYPED binary NULL: go-mssqldb sends an untyped nil as an
			// nvarchar NULL, which SQL Server refuses to implicitly convert
			// into a BINARY(16) column; a nil []byte binds as varbinary NULL,
			// which converts.
			return []byte(nil)
		}
		if b, err := uuidBytes(v.Value()); err == nil {
			return b
		}
		return v.Value()
	case uuid.UUID:
		return v[:]
	case json.RawMessage:
		return string(v)
	default:
		return val
	}
}

// DecodeID converts a scanned leading key back to the canonical UUID string. A
// BINARY(16) column scans into a 16-byte string; anything else passes through
// (defensive — e.g. an NCHAR id or an already-decoded value).
func (sqlserverDialect) DecodeID(raw string) (string, error) {
	if len(raw) == 16 {
		u, err := uuid.FromBytes([]byte(raw))
		if err != nil {
			return "", fmt.Errorf("sqlserver: decoding BINARY(16) id: %w", err)
		}
		return u.String(), nil
	}
	return raw, nil
}
