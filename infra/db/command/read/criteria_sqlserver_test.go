package read

import (
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/criteria"
)

// testSQLServerDialect is a SQL Server-flavored db.Dialect for the criteria
// translator tests — the @pN-placeholder, bracket-quoting, BINARY(16) sibling
// of testPGDialect/testMySQLDialect. The translator is backend-neutral; these
// tests lock the third rendering: numbered @pN placeholders (like PG's $n, the
// number ties a value to its slot), bracket identifiers, the LOWER-LIKE
// case-insensitive form, and the BINARY(16) id encoding on the probe side.
type testSQLServerDialect struct{}

func (testSQLServerDialect) Placeholder(n int) string { return fmt.Sprintf("@p%d", n) }
func (testSQLServerDialect) QuoteIdent(name string) string {
	if !SafeIdentifier(name) {
		panic(fmt.Sprintf("test: invalid SQL identifier %q", name))
	}
	return "[" + name + "]"
}
func (testSQLServerDialect) EncodeArg(val any) any {
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
func (testSQLServerDialect) DecodeID(raw string) (string, error) { return raw, nil }
func (testSQLServerDialect) ILikeClause(col, ph string) string {
	return "LOWER(" + col + ") LIKE LOWER(" + ph + ")"
}
func (testSQLServerDialect) NowExpr() string { return "CURRENT_TIMESTAMP" }
func (testSQLServerDialect) ApplyLimit(sql string, n int) string {
	return fmt.Sprintf("SELECT TOP %d %s", n, sql[len("SELECT "):])
}
func (testSQLServerDialect) Savepoint(name string) string { return "SAVE TRANSACTION " + name }
func (testSQLServerDialect) RollbackToSavepoint(name string) string {
	return "ROLLBACK TRANSACTION " + name
}
func (testSQLServerDialect) ReleaseSavepoint(string) string             { return "" }
func (testSQLServerDialect) IsUniqueViolation(error) (string, bool)     { return "", false }
func (testSQLServerDialect) IsForeignKeyViolation(error) (string, bool) { return "", false }
func (testSQLServerDialect) BuildUpsert(table string, _, _ []string, _ []UpsertSet) string {
	return "MERGE " + table
}

func TestSQLServerVisitor_Operators(t *testing.T) {
	r := testResolver()
	cases := []struct {
		name string
		e    criteria.Expr
		sql  string
		args int
	}{
		{"eq", criteria.Eq("Email", "a@x"), "[email] = @p1", 1},
		{"ne", criteria.Ne("Email", "a@x"), "[email] <> @p1", 1},
		{"gt", criteria.Gt("Age", 18), "[age] > @p1", 1},
		{"lte", criteria.Lte("Age", 18), "[age] <= @p1", 1},
		{"like", criteria.Like("Name", "Bob%"), "[name] LIKE @p1", 1},
		{"ilike", criteria.ILike("Name", "bob%"), "LOWER([name]) LIKE LOWER(@p1)", 1},
		{"in", criteria.In("Name", "a", "b", "c"), "[name] IN (@p1, @p2, @p3)", 3},
		{"nin", criteria.Nin("Name", "a", "b"), "[name] NOT IN (@p1, @p2)", 2},
		{"isnull", criteria.IsNull("Phone"), "[phone] IS NULL", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sql, args, err := compileWhere(c.e, r, testSQLServerDialect{}, nil)
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

// TestSQLServerVisitor_PlaceholderNumbering asserts the @pN numbering runs
// left-to-right across a composite expression and the args slice matches it —
// the SQL Server twin of the PG $n discipline (go-mssqldb binds the Nth arg to
// @pN, so drifting numbers would misbind values to columns).
func TestSQLServerVisitor_PlaceholderNumbering(t *testing.T) {
	e := criteria.And(
		criteria.Eq("Name", "a"),
		criteria.In("Email", "x", "y"),
		criteria.Gt("Age", 1),
	)
	sql, args, err := compileWhere(e, testResolver(), testSQLServerDialect{}, nil)
	if err != nil {
		t.Fatalf("compileWhere: %v", err)
	}
	if want := "([name] = @p1 AND [email] IN (@p2, @p3) AND [age] > @p4)"; sql != want {
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

// TestSQLServerVisitor_DomainIDArgEncodedToBinary proves a domain.ID in the
// WHERE predicate reaches the args slice as its 16-byte BINARY(16) form on SQL
// Server (PG keeps it a string) — the read-side mirror of the write-path id
// encoding, identical to the MySQL contract.
func TestSQLServerVisitor_DomainIDArgEncodedToBinary(t *testing.T) {
	u := uuid.MustParse("018f8b2c-1d3e-7a9b-bc4d-5e6f7a8b9c0d")
	_, args, err := compileWhere(criteria.Eq("ID", domain.NewID(u.String())), testResolver(), testSQLServerDialect{}, nil)
	if err != nil {
		t.Fatalf("compileWhere: %v", err)
	}
	b, ok := args[0].([]byte)
	if !ok || len(b) != 16 {
		t.Fatalf("args[0] = %v (%T), want a 16-byte BINARY(16) form", args[0], args[0])
	}
}
