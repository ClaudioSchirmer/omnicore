//go:build oracle

package oracle

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/sijms/go-ora/v2/network"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// The Oracle core.Dialect implementation — every engine-specific statement
// fragment (read and write), the fourth flavor beside pg/mysql/sqlserver.

// Dialect returns the Oracle statement flavor.
func (e *Engine) Dialect() core.Dialect { return oracleDialect{} }

type oracleDialect struct{}

func (oracleDialect) Placeholder(n int) string   { return ":" + strconv.Itoa(n) }
func (oracleDialect) QuoteIdent(n string) string { return quoteIdent(n) }
func (oracleDialect) ILikeClause(col, ph string) string {
	// LOWER both sides so the match is case-insensitive regardless of the
	// database's NLS_COMP/NLS_SORT session settings — the framework must not
	// depend on how the operator created the database. Postgres ILIKE / MySQL
	// LOWER-LIKE parity.
	return "LOWER(" + col + ") LIKE LOWER(" + ph + ")"
}

// NowExpr is SYSTIMESTAMP — the server-timezone "now", matching NOW() on
// PG/MySQL and CURRENT_TIMESTAMP on SQL Server. Oracle's own CURRENT_TIMESTAMP
// is session-timezone and would make this the one session-relative dialect.
func (oracleDialect) NowExpr() string { return "SYSTIMESTAMP" }

// ApplyLimit caps a complete SELECT at n rows with the tail
// `FETCH FIRST n ROWS ONLY` clause — valid without an ORDER BY (the existence
// probes deliberately carry none), so unlike SQL Server's SELECT-head TOP no
// statement rewrite is needed; Oracle rides the PG/MySQL append shape.
func (oracleDialect) ApplyLimit(sqlText string, n int) string {
	return sqlText + " FETCH FIRST " + strconv.Itoa(n) + " ROWS ONLY"
}

// ApplyLimitOffset renders a windowed page onto a complete SELECT — the standard
// `OFFSET m ROWS FETCH NEXT n ROWS ONLY` row-limiting tail (Oracle 12c+). Offset
// windowing is only defined over a deterministic ORDER BY; the caller guarantees
// it (see Dialect.ApplyLimitOffset).
func (oracleDialect) ApplyLimitOffset(sqlText string, limit, offset int) string {
	return sqlText + " OFFSET " + strconv.Itoa(offset) + " ROWS FETCH NEXT " + strconv.Itoa(limit) + " ROWS ONLY"
}

// Savepoint statements, Oracle flavor: the standard SAVEPOINT / ROLLBACK TO
// SAVEPOINT forms — and, like T-SQL, NO release statement (a savepoint is
// simply discarded at COMMIT), so ReleaseSavepoint returns "" and the caller
// skips the empty statement (the documented Dialect contract).
func (oracleDialect) Savepoint(name string) string { return "SAVEPOINT " + name }
func (oracleDialect) RollbackToSavepoint(name string) string {
	return "ROLLBACK TO SAVEPOINT " + name
}
func (oracleDialect) ReleaseSavepoint(string) string { return "" }

// IsUniqueViolation reads ORA-00001 ("unique constraint (SCHEMA.NAME)
// violated…") and extracts the violated constraint name. The FIRST
// `constraint (` marker is the trusted one: 23ai appends an ORA-03301 detail
// line whose parentheses carry the USER-CONTROLLED duplicate value
// ("row with column values (EMAIL:'…')"), so a last-parenthesis parse would
// lock onto user data — the exact injection window the message-parse
// discipline forbids. No user-controlled text precedes the first marker.
func (oracleDialect) IsUniqueViolation(err error) (string, bool) {
	var oe *network.OracleError
	if !errors.As(err, &oe) || oe.ErrCode != 1 {
		return "", false
	}
	return extractConstraintName(oe.ErrMsg), true
}

// IsForeignKeyViolation reads ORA-02291 ("integrity constraint (SCHEMA.NAME)
// violated - parent key not found") and ORA-02292 ("… - child record found")
// and extracts the violated constraint name — the same first-marker parse as
// IsUniqueViolation. 02292 powers the shared-base orphan-purge veto, like
// 23503/1451/547 on the other engines. A violation without a parseable name
// still classifies.
func (oracleDialect) IsForeignKeyViolation(err error) (string, bool) {
	var oe *network.OracleError
	if !errors.As(err, &oe) || (oe.ErrCode != 2291 && oe.ErrCode != 2292) {
		return "", false
	}
	return extractConstraintName(oe.ErrMsg), true
}

// extractConstraintName pulls the bare constraint name out of an Oracle
// integrity-violation message: the first `constraint (SCHEMA.NAME)` marker,
// schema qualifier dropped, surrounding quotes stripped, lowercased. The
// lowercase step is the D11 case normalization: identifiers are stored
// UPPERCASE in the catalog (unquoted DDL), while ConstraintBinding matches the
// lowercase names the migrations declare — identical across all four dialects.
func extractConstraintName(msg string) string {
	const marker = "constraint ("
	i := strings.Index(msg, marker)
	if i < 0 {
		return ""
	}
	rest := msg[i+len(marker):]
	j := strings.IndexByte(rest, ')')
	if j < 0 {
		return ""
	}
	name := rest[:j]
	if k := strings.IndexByte(name, '.'); k >= 0 {
		name = name[k+1:]
	}
	return strings.ToLower(strings.Trim(name, `"`))
}

// BuildUpsert renders the Oracle upsert: a single
// `MERGE INTO <table> target USING (SELECT :n AS col, … FROM dual) source`
// statement. Oracle has NO HOLDLOCK equivalent, so two concurrent MERGEs on the
// same absent key can race between the match probe and the insert — the loser
// surfaces ORA-00001, which IsUniqueViolation classifies into the callers'
// existing conflict path (proven against a live 23ai; see tasks/oracle.md D2).
// The proposed value for an UpsertSetNew assignment is `source.col`; a bare
// column in the UPDATE clause refers to the existing row (target scope),
// identical to the other dialects, so verbatim UpsertSetExpr expressions
// ("attempt + 1", "NULL", the dialect's NowExpr()) carry the same semantics.
// Empty sets omit the WHEN MATCHED clause — a true do-nothing on conflict.
// Callers never assign a conflict column in sets (Oracle forbids updating ON
// columns, ORA-38104). No statement terminator: the driver rejects a trailing
// semicolon on plain SQL (it belongs to PL/SQL blocks only).
//
// The ON comparison is NULL-SAFE (`=` OR both-NULL) — an Oracle-only need: an
// empty string binds as NULL here, so a conflict column that is "" on the
// other engines (omnicore_upstream_failures.local_id on the discover stage)
// arrives NULL, and a plain `=` would never match it — every retry would take
// the INSERT arm and die on ORA-00001 instead of incrementing the attempt.
// The natural-key UNIQUE still dedups those rows (an Oracle B-tree treats
// identical composite entries with a NULL in the same slot as duplicates).
func (d oracleDialect) BuildUpsert(table string, cols, conflictCols []string, sets []core.UpsertSet) string {
	var b strings.Builder
	b.WriteString("MERGE INTO ")
	b.WriteString(d.QuoteIdent(table))
	b.WriteString(" target USING (SELECT ")
	for i, c := range cols {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(d.Placeholder(i + 1))
		b.WriteString(" AS ")
		b.WriteString(d.QuoteIdent(c))
	}
	b.WriteString(" FROM dual) source ON (")
	for i, k := range conflictCols {
		if i > 0 {
			b.WriteString(" AND ")
		}
		q := d.QuoteIdent(k)
		b.WriteString("(target.")
		b.WriteString(q)
		b.WriteString(" = source.")
		b.WriteString(q)
		b.WriteString(" OR (target.")
		b.WriteString(q)
		b.WriteString(" IS NULL AND source.")
		b.WriteString(q)
		b.WriteString(" IS NULL))")
	}
	b.WriteString(")")
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
	b.WriteString(")")
	return b.String()
}

// EncodeArg binds TYPED id values as their 16-byte form so they match a
// RAW(16) column: a domain.ID / *domain.ID and a uuid.UUID encode by type —
// carrying the identity type IS the declaration, the same way the scan side
// detects id fields by their Go type. A nil *domain.ID binds []byte(nil) — a
// TYPED RAW NULL, mirroring the SQL Server engine. A plain string ALWAYS
// passes through as text, even in canonical uuid shape: whether a value is an
// identity is stated by its TYPE (`domain.ID` field ⇒ RAW(16) column, `string`
// field ⇒ VARCHAR2 column), never guessed from the value's shape. (A domain.ID
// wrapping a non-parseable string degrades to its text value — the column, not
// the codec, rejects it.)
//
// json.RawMessage binds as a string: the JSON column shape here is the native
// 23ai JSON type, which accepts a text bind (proven in the spike), while the
// driver's []byte mapping would reach it as RAW.
func (oracleDialect) EncodeArg(val any) any {
	switch v := val.(type) {
	case domain.ID:
		if b, err := uuidBytes(v.Value()); err == nil {
			return b
		}
		return v.Value()
	case *domain.ID:
		if v == nil {
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
// RAW(16) column scans into a 16-byte string; anything else passes through
// (defensive — e.g. a VARCHAR2 id or an already-decoded value).
func (oracleDialect) DecodeID(raw string) (string, error) {
	if len(raw) == 16 {
		u, err := uuid.FromBytes([]byte(raw))
		if err != nil {
			return "", errors.New("oracle: decoding RAW(16) id: " + err.Error())
		}
		return u.String(), nil
	}
	return raw, nil
}
