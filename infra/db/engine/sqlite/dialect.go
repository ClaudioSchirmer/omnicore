//go:build sqlite

package sqlite

import (
	"errors"
	"strconv"
	"strings"
	"time"

	sqlitedrv "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// The SQLite core.Dialect implementation. SQLite maps closely to Postgres — ids
// stored as TEXT (identity codecs, no 16-byte round-trip), an ON CONFLICT upsert
// with excluded.* (the Postgres shape, not a MERGE), and the standard savepoint
// trio (SQLite HAS RELEASE, unlike T-SQL). What is genuinely different: the
// current-timestamp literal (strftime, millisecond precision to match the
// app-clock), the case-sensitive LIKE (backed by the case_sensitive_like pragma),
// and the constraint classifiers (modernc extended result codes + the message's
// column list, since SQLite reports the columns, not a constraint name).

// Dialect returns the SQLite statement flavor.
func (e *Engine) Dialect() core.Dialect { return sqliteDialect{} }

type sqliteDialect struct{}

func (sqliteDialect) Placeholder(int) string        { return "?" }
func (sqliteDialect) QuoteIdent(name string) string { return quoteIdent(name) }
func (sqliteDialect) EncodeArg(val any) any         { return encodeArg(val) }

// DecodeID is identity: SQLite stores ids as TEXT (the canonical uuid string),
// so the scanned leading key is already the string the framework wants — the
// Postgres posture, no BINARY(16)/RAW(16) round-trip.
func (sqliteDialect) DecodeID(raw string) (string, error) { return raw, nil }

// likeEscapeClause declares backslash as the LIKE escape character. The criteria
// pattern builder escapes %, _ and \ with a backslash (the Postgres LIKE
// default), but SQLite's LIKE has NO default escape character, so without this
// the backslash matches literally and an escaped %/_ leaks its wildcard meaning.
const likeEscapeClause = ` ESCAPE '\'`

// ILikeClause forces LOWER on both sides so the match is case-insensitive
// regardless of the case_sensitive_like pragma (which the factory turns ON for
// OpLike). NOTE: SQLite's LOWER() is ASCII-only, so case-insensitive matching
// does not fold accented/Unicode letters — an MVP-posture limitation documented
// in table-schema.html (D9). Postgres ILIKE / MySQL LOWER-LIKE parity otherwise.
func (sqliteDialect) ILikeClause(col, ph string) string {
	return "LOWER(" + col + ") LIKE LOWER(" + ph + ")" + likeEscapeClause
}

// LikeClause renders a bare LIKE — made case-SENSITIVE by the connection's
// case_sensitive_like(ON) pragma (forced in dsn.go). Honors criteria.OpLike's
// contract; the pragma is the mechanism, so no COLLATE/BINARY wrapper is needed.
func (sqliteDialect) LikeClause(col, ph string) string {
	return col + " LIKE " + ph + likeEscapeClause
}

// NowExpr is strftime with millisecond precision (%f). Second-precision
// CURRENT_TIMESTAMP would make an SQL-stamped value and a Go app-clock value
// (RFC3339, see encodeArg) scan/compare unequally; %f keeps them aligned (D1).
func (sqliteDialect) NowExpr() string { return "strftime('%Y-%m-%d %H:%M:%f','now')" }

// ApplyLimit caps a complete SELECT at n rows — the native tail clause.
func (sqliteDialect) ApplyLimit(sql string, n int) string {
	return sql + " LIMIT " + strconv.Itoa(n)
}

// ApplyLimitOffset renders a windowed page onto a complete SELECT — the native
// LIMIT n OFFSET m tail (no OFFSET…FETCH gymnastics). The caller guarantees a
// deterministic ORDER BY for a non-zero offset.
func (sqliteDialect) ApplyLimitOffset(sql string, limit, offset int) string {
	return sql + " LIMIT " + strconv.Itoa(limit) + " OFFSET " + strconv.Itoa(offset)
}

func (sqliteDialect) Savepoint(name string) string           { return "SAVEPOINT " + name }
func (sqliteDialect) RollbackToSavepoint(name string) string { return "ROLLBACK TO SAVEPOINT " + name }
func (sqliteDialect) ReleaseSavepoint(name string) string    { return "RELEASE SAVEPOINT " + name }

// IsUniqueViolation classifies a modernc error as a unique/primary-key
// violation via the extended result codes (SQLITE_CONSTRAINT_UNIQUE 2067 /
// _PRIMARYKEY 1555) and returns the violated COLUMN LIST — SQLite reports
// "UNIQUE constraint failed: t.col", not the constraint/index name the other
// engines return. Per D2 the column list IS the ConstraintBinding key on SQLite
// (the caller's "one key per engine" model already tolerates per-engine naming);
// a hit with no parseable list returns ("", true).
func (sqliteDialect) IsUniqueViolation(err error) (string, bool) {
	var se *sqlitedrv.Error
	if !errors.As(err, &se) {
		return "", false
	}
	if se.Code() != sqlite3.SQLITE_CONSTRAINT_UNIQUE && se.Code() != sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY {
		return "", false
	}
	return uniqueColumnList(se.Error()), true
}

// IsForeignKeyViolation classifies a modernc error as a foreign-key violation
// (SQLITE_CONSTRAINT_FOREIGNKEY 787). SQLite's message carries no constraint
// name ("FOREIGN KEY constraint failed"), so it returns ("", true) on a hit —
// the shared-base orphan-purge veto needs only the boolean.
func (sqliteDialect) IsForeignKeyViolation(err error) (string, bool) {
	var se *sqlitedrv.Error
	if !errors.As(err, &se) {
		return "", false
	}
	if se.Code() != sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY {
		return "", false
	}
	return "", true
}

// uniqueColumnList extracts the "t.col" (or "t.a, t.b") list SQLite names in a
// unique-violation message: "…UNIQUE constraint failed: t.col (2067)". Returns
// the text after the marker with the trailing " (code)" suffix stripped.
func uniqueColumnList(msg string) string {
	const marker = "UNIQUE constraint failed: "
	i := strings.Index(msg, marker)
	if i < 0 {
		return ""
	}
	rest := msg[i+len(marker):]
	if j := strings.LastIndex(rest, " ("); j >= 0 {
		rest = rest[:j]
	}
	return strings.TrimSpace(rest)
}

// BuildUpsert renders the SQLite upsert: INSERT … ON CONFLICT (conflictCols) DO
// UPDATE SET … (or DO NOTHING when sets is empty) — the Postgres shape, so
// UpsertSetNew reads the proposed value as excluded.col and UpsertSetBump reads
// the existing row as <table>.col + 1 (the table qualifier disambiguates against
// excluded, matching the PG dialect). No MERGE.
func (d sqliteDialect) BuildUpsert(table string, cols, conflictCols []string, sets []core.UpsertSet) string {
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
		switch s.Mode {
		case core.UpsertSetNew:
			b.WriteString("excluded.")
			b.WriteString(d.QuoteIdent(s.Col))
		case core.UpsertSetBump:
			b.WriteString(d.QuoteIdent(table))
			b.WriteString(".")
			b.WriteString(d.QuoteIdent(s.Col))
			b.WriteString(" + 1")
		default:
			b.WriteString(s.Expr)
		}
	}
	return b.String()
}

// writeInsertHead renders the "INSERT INTO t (cols) VALUES (?…)" prefix shared by
// the upsert — SQLite placeholders are "?" so the head reads "?, ?, …". Kept
// package-local so the engine mirror stays self-contained (each engine builds its
// own head).
func writeInsertHead(b *strings.Builder, d core.Dialect, table string, cols []string) {
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

// encodeArg converts framework value types into the form modernc binds. domain.ID
// becomes its uuid string (stored TEXT, the Postgres posture); a *domain.ID is
// the nullable identity — nil stays nil (SQL NULL). A time.Time is formatted as
// RFC3339Nano TEXT (D4): modernc would otherwise store a bound time.Time in Go's
// default "2006-01-02 15:04:05.999… -0700 MST" layout, which neither the scanner
// nor the strftime NowExpr agree with. Everything else passes through.
func encodeArg(val any) any {
	val = core.UnwrapVO(val) // a value-object criteria value binds as its underlying scalar
	switch v := val.(type) {
	case domain.ID:
		return v.Value()
	case *domain.ID:
		if v == nil {
			return nil
		}
		return v.Value()
	case time.Time:
		return v.UTC().Format(time.RFC3339Nano)
	case *time.Time:
		if v == nil {
			return nil
		}
		return v.UTC().Format(time.RFC3339Nano)
	default:
		return val
	}
}
