package read

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/criteria"
)

// testOracleDialect is an Oracle-flavored db.Dialect for the criteria
// translator tests — the :n-placeholder, quoted-uppercase, RAW(16) sibling of
// testPGDialect/testMySQLDialect/testSQLServerDialect. The translator is
// backend-neutral; these tests lock the fourth rendering: numbered :n
// placeholders (like PG's $n, the number ties a value to its slot),
// QUOTED-UPPERCASE identifiers (equivalent to the catalog's unquoted
// uppercase folding, and reserved-word safe), the LOWER-LIKE case-insensitive
// form, and the RAW(16) id encoding on the probe side.
type testOracleDialect struct{}

func (testOracleDialect) Placeholder(n int) string { return fmt.Sprintf(":%d", n) }
func (testOracleDialect) QuoteIdent(name string) string {
	if !SafeIdentifier(name) {
		panic(fmt.Sprintf("test: invalid SQL identifier %q", name))
	}
	return `"` + strings.ToUpper(name) + `"`
}
func (testOracleDialect) EncodeArg(val any) any {
	switch v := val.(type) {
	case domain.ID:
		if u, err := uuid.Parse(v.Value()); err == nil {
			return u[:]
		}
		return v.Value()
	case *domain.ID:
		if v == nil {
			return nil
		}
		if u, err := uuid.Parse(v.Value()); err == nil {
			return u[:]
		}
		return v.Value()
	case uuid.UUID:
		return v[:]
	default:
		return val
	}
}
func (testOracleDialect) DecodeID(raw string) (string, error) { return raw, nil }
func (testOracleDialect) ILikeClause(col, ph string) string {
	return "LOWER(" + col + ") LIKE LOWER(" + ph + ")"
}
func (testOracleDialect) LikeClause(col, ph string) string {
	return col + " LIKE " + ph
}
func (testOracleDialect) NowExpr() string { return "SYSTIMESTAMP" }
func (testOracleDialect) ApplyLimit(sql string, n int) string {
	return fmt.Sprintf("%s FETCH FIRST %d ROWS ONLY", sql, n)
}
func (testOracleDialect) ApplyLimitOffset(sql string, limit, offset int) string {
	return fmt.Sprintf("%s OFFSET %d ROWS FETCH NEXT %d ROWS ONLY", sql, offset, limit)
}
func (testOracleDialect) Savepoint(name string) string { return "SAVEPOINT " + name }
func (testOracleDialect) RollbackToSavepoint(name string) string {
	return "ROLLBACK TO SAVEPOINT " + name
}
func (testOracleDialect) ReleaseSavepoint(string) string             { return "" }
func (testOracleDialect) IsUniqueViolation(error) (string, bool)     { return "", false }
func (testOracleDialect) IsForeignKeyViolation(error) (string, bool) { return "", false }
func (testOracleDialect) BuildUpsert(table string, _, _ []string, _ []UpsertSet) string {
	return "MERGE INTO " + table
}

func TestOracleVisitor_Operators(t *testing.T) {
	r := testResolver()
	cases := []struct {
		name string
		e    criteria.Expr
		sql  string
		args int
	}{
		{"eq", criteria.Eq("Email", "a@x"), `"EMAIL" = :1`, 1},
		{"ne", criteria.Ne("Email", "a@x"), `"EMAIL" <> :1`, 1},
		{"gt", criteria.Gt("Age", 18), `"AGE" > :1`, 1},
		{"lte", criteria.Lte("Age", 18), `"AGE" <= :1`, 1},
		{"like", criteria.Like("Name", "Bob%"), `"NAME" LIKE :1`, 1},
		{"ilike", criteria.ILike("Name", "bob%"), `LOWER("NAME") LIKE LOWER(:1)`, 1},
		{"in", criteria.In("Name", "a", "b", "c"), `"NAME" IN (:1, :2, :3)`, 3},
		{"nin", criteria.Nin("Name", "a", "b"), `"NAME" NOT IN (:1, :2)`, 2},
		{"isnull", criteria.IsNull("Phone"), `"PHONE" IS NULL`, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sql, args, err := compileWhere(c.e, r, testOracleDialect{}, nil)
			if err != nil {
				t.Fatalf("compileWhere: %v", err)
			}
			if sql != c.sql {
				t.Errorf("sql = %q, want %q", sql, c.sql)
			}
			if len(args) != c.args {
				t.Errorf("args = %d, want %d", len(args), c.args)
			}
		})
	}
}

// TestOracleVisitor_PlaceholderNumbering asserts the :n numbering runs
// left-to-right across a composite expression and the args slice matches it —
// the Oracle twin of the PG $n discipline (go-ora binds the Nth arg to :n, so
// drifting numbers would misbind values to columns).
func TestOracleVisitor_PlaceholderNumbering(t *testing.T) {
	e := criteria.And(
		criteria.Eq("Name", "a"),
		criteria.In("Email", "x", "y"),
		criteria.Gt("Age", 1),
	)
	sql, args, err := compileWhere(e, testResolver(), testOracleDialect{}, nil)
	if err != nil {
		t.Fatalf("compileWhere: %v", err)
	}
	if want := `("NAME" = :1 AND "EMAIL" IN (:2, :3) AND "AGE" > :4)`; sql != want {
		t.Errorf("sql  = %q\nwant = %q", sql, want)
	}
	want := []any{"a", "x", "y", 1}
	if len(args) != len(want) {
		t.Fatalf("args = %v, want %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Errorf("args[%d] = %v, want %v — placeholder numbering drifted from arg order", i, args[i], want[i])
		}
	}
}

// TestOracleVisitor_DomainIDArgEncodedToRaw proves a domain.ID in the WHERE
// predicate reaches the args slice as its 16-byte RAW(16) form on Oracle (PG
// keeps it a string) — the read-side mirror of the write-path id encoding,
// identical to the MySQL/SQL Server contract.
func TestOracleVisitor_DomainIDArgEncodedToRaw(t *testing.T) {
	u := uuid.MustParse("018f8b2c-1d3e-7a9b-bc4d-5e6f7a8b9c0d")
	_, args, err := compileWhere(criteria.Eq("ID", domain.NewID(u.String())), testResolver(), testOracleDialect{}, nil)
	if err != nil {
		t.Fatalf("compileWhere: %v", err)
	}
	b, ok := args[0].([]byte)
	if !ok || len(b) != 16 {
		t.Fatalf("args[0] = %v (%T), want a 16-byte RAW(16) form", args[0], args[0])
	}
}
