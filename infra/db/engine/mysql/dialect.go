//go:build mysql

package mysql

import (
	"errors"
	"fmt"
	"strings"
	"time"

	driver "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// The MySQL core.Dialect implementation — every engine-specific statement
// fragment (read and write), split out of read.go once the seam grew write
// duties (upsert, savepoints, the now expression).

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

func (mysqlDialect) LikeClause(col, ph string) string {
	// The BINARY operator forces byte-exact (case-sensitive) comparison
	// regardless of the column's charset/collation — a bare LIKE would be
	// case-INSENSITIVE under a CI collation like utf8mb4_0900_ai_ci. Chosen over
	// `COLLATE utf8mb4_bin`, which errors on a non-utf8mb4 column; BINARY works
	// on any text column. Honors criteria.OpLike's case-sensitive contract.
	return "BINARY " + col + " LIKE " + ph
}

func (mysqlDialect) NowExpr() string { return "NOW()" }

// UTCNowExpr is UTC_TIMESTAMP(6) — UTC by definition (never the session time
// zone) and with the explicit (6) that NOW() lacks: a bare NOW()/UTC_TIMESTAMP
// carries ZERO fractional digits on MySQL, which would round every operation's
// stamp to the whole second.
func (mysqlDialect) UTCNowExpr() string { return "UTC_TIMESTAMP(6)" }

// ApplyLimit caps a complete SELECT at n rows — the native tail clause on
// MySQL.
func (mysqlDialect) ApplyLimit(sql string, n int) string {
	return fmt.Sprintf("%s LIMIT %d", sql, n)
}

// ApplyLimitOffset renders a windowed page onto a complete SELECT — the native
// `LIMIT n OFFSET m` tail on MySQL. Offset windowing is only defined over a
// deterministic ORDER BY; the caller guarantees it (see Dialect.ApplyLimitOffset).
func (mysqlDialect) ApplyLimitOffset(sql string, limit, offset int) string {
	return fmt.Sprintf("%s LIMIT %d OFFSET %d", sql, limit, offset)
}

func (mysqlDialect) Savepoint(name string) string           { return "SAVEPOINT " + name }
func (mysqlDialect) RollbackToSavepoint(name string) string { return "ROLLBACK TO SAVEPOINT " + name }
func (mysqlDialect) ReleaseSavepoint(name string) string    { return "RELEASE SAVEPOINT " + name }

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
	// A conflict-only value binds its own argument, numbered after the inserted
	// columns — the clause is rendered after the VALUES list, so the positions
	// line up with the arguments appended behind the insert ones.
	arg := len(cols)
	for i, s := range sets {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(d.QuoteIdent(s.Col))
		b.WriteString(" = ")
		switch s.Mode {
		case core.UpsertSetNew:
			b.WriteString("new.")
			b.WriteString(d.QuoteIdent(s.Col))
		case core.UpsertSetBump:
			// The existing row, table-qualified: with the `AS new` row alias
			// present a bare column on the right-hand side is ambiguous
			// (MySQL 8.4 errno 1052) — same trap the no-op branch above names.
			b.WriteString(d.QuoteIdent(table))
			b.WriteString(".")
			b.WriteString(d.QuoteIdent(s.Col))
			b.WriteString(" + 1")
		case core.UpsertSetArg:
			arg++
			b.WriteString(d.Placeholder(arg))
		default:
			b.WriteString(s.Expr)
		}
	}
	return b.String()
}

// EncodeArg binds TYPED id values as their 16-byte form so they match a
// BINARY(16) column: a domain.ID / *domain.ID and a uuid.UUID encode by type —
// carrying the identity type IS the declaration, the same way the scan side
// detects id fields by their Go type. A nil *domain.ID stays nil (SQL NULL). A
// plain string ALWAYS passes through as text, even in canonical uuid shape:
// whether a value is an identity is stated by its TYPE (`domain.ID` field ⇒
// BINARY(16) column, `string` field ⇒ CHAR/VARCHAR column), never guessed from
// the value's shape — the criteria translator lifts probes on id-typed fields
// into domain.ID (by the schema's reflected field type) before they reach this
// codec. (A domain.ID wrapping a non-parseable string degrades to its text
// value — the column, not the codec, rejects it.) Everything else passes
// through.
func (mysqlDialect) EncodeArg(val any) any {
	val = core.UnwrapVO(val) // a value-object criteria value binds as its underlying scalar
	switch v := val.(type) {
	case domain.ID:
		if b, err := uuidBytes(v.Value()); err == nil {
			return b
		}
		return v.Value()
	case *domain.ID:
		if v == nil {
			return nil
		}
		if b, err := uuidBytes(v.Value()); err == nil {
			return b
		}
		return v.Value()
	case uuid.UUID:
		return v[:]
	case time.Time:
		return encodeMySQLTime(v)
	case *time.Time:
		if v == nil {
			return nil
		}
		return encodeMySQLTime(*v)
	default:
		return val
	}
}

// zeroDatetimeLayout is how the ZERO instant is spelled for this backend. It is
// the ordinary DATETIME(6) literal shape — nothing special about it except that
// the driver never gets to see a zero time.Time and apply its own rule.
const zeroDatetimeLayout = "2006-01-02 15:04:05.000000"

// encodeMySQLTime binds an instant, with ONE value handled by hand: the zero
// time.Time.
//
// go-sql-driver serializes a zero time.Time as the literal '0000-00-00' rather
// than as year 1, and a server running NO_ZERO_DATE + STRICT_TRANS_TABLES — the
// default on MySQL 8 — rejects that outright:
//
//	Error 1292 (22007): Incorrect datetime value: '0000-00-00'
//
// The column is not the problem: DATETIME(6) stores '0001-01-01 00:00:00.000000'
// without complaint, and reads it back as a time.Time whose IsZero() is true. So
// the zero binds as its formatted text, which the driver passes through
// untouched, and every other instant keeps the path it always had.
//
// This is what TableSchema.StampEmpty writes into a stamped TIME column, and it
// is the only backend that needs the detour — Postgres, SQL Server (DATETIME2),
// Oracle and SQLite all take year 1 directly.
func encodeMySQLTime(t time.Time) any {
	if t.IsZero() {
		return t.Format(zeroDatetimeLayout)
	}
	return t
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
