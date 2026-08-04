//go:build sqlserver

package sqlserver

import (
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

// The SQL Server core.Dialect implementation — every engine-specific statement
// fragment (read and write), split out of read.go once the seam grew write
// duties (upsert, savepoints, the now expression).

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
	return "LOWER(" + col + ") LIKE LOWER(" + ph + ")" + likeEscapeClause
}

// likeEscapeClause declares backslash as the LIKE escape character — the
// pattern builder escapes %, _ and \ with a backslash (the Postgres default),
// but SQL Server LIKE has no default escape.
const likeEscapeClause = ` ESCAPE '\'`

func (sqlserverDialect) LikeClause(col, ph string) string {
	// The inline COLLATE forces a byte-exact (case-sensitive) comparison — a
	// bare LIKE is case-INSENSITIVE under the server's default CI collation
	// (e.g. SQL_Latin1_General_CP1_CI_AS). Latin1_General_BIN is valid on any
	// text column and independent of how the operator built the database.
	// Honors criteria.OpLike's case-sensitive contract. ESCAPE '\' matches the
	// backslash the criteria pattern builder uses (SQL Server LIKE has no default
	// escape, so an escaped %/_ would otherwise leak its wildcard meaning).
	return col + " LIKE " + ph + " COLLATE Latin1_General_BIN" + likeEscapeClause
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

// ApplyLimitOffset renders a windowed page onto a complete SELECT — the T-SQL
// `OFFSET m ROWS FETCH NEXT n ROWS ONLY` tail. Unlike ApplyLimit's SELECT-head
// TOP (which needs no order, so it cannot skip), OFFSET…FETCH mandates an
// ORDER BY — which the caller guarantees is present (a non-zero offset is
// rejected without one), so the tail simply appends; no head rewrite.
func (sqlserverDialect) ApplyLimitOffset(sqlText string, limit, offset int) string {
	return sqlText + " OFFSET " + strconv.Itoa(offset) + " ROWS FETCH NEXT " + strconv.Itoa(limit) + " ROWS ONLY"
}

// Savepoint statements, T-SQL flavor: SAVE TRANSACTION opens the savepoint,
// ROLLBACK TRANSACTION <name> rolls back to it — and there is NO release
// statement (a savepoint is simply discarded at COMMIT), so ReleaseSavepoint
// returns "" and the caller skips the empty statement (the documented Dialect
// contract).
func (sqlserverDialect) Savepoint(name string) string { return "SAVE TRANSACTION " + name }
func (sqlserverDialect) RollbackToSavepoint(name string) string {
	return "ROLLBACK TRANSACTION " + name
}
func (sqlserverDialect) ReleaseSavepoint(string) string { return "" }

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
			switch s.Mode {
			case core.UpsertSetNew:
				b.WriteString("source.")
				b.WriteString(d.QuoteIdent(s.Col))
			case core.UpsertSetBump:
				// The existing row is the MERGE target, and the table is
				// aliased — the alias is the only valid qualifier here.
				b.WriteString("target.")
				b.WriteString(d.QuoteIdent(s.Col))
				b.WriteString(" + 1")
			default:
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
	val = core.UnwrapVO(val) // a value-object criteria value binds as its underlying scalar
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
