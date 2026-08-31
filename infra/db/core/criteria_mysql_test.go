package core

import (
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/criteria"
)

// testMySQLDialect is a MySQL-flavored db.Dialect for the criteria translator
// tests — the `?`-placeholder, backtick-quoting, BINARY(16) twin of
// testPGDialect. The translator is backend-neutral; testPGDialect already proves
// the `$n` rendering, but the MySQL `?` form is POSITIONAL, so the bind order is
// load-bearing: nothing in the SQL ties a value to a column except its index in
// the args slice. These tests lock that ordering for the MySQL rendering, the
// gap the integration-only MySQL coverage left on the unit side.
type testMySQLDialect struct{}

func (testMySQLDialect) Placeholder(int) string { return "?" } // positional — index unused
func (testMySQLDialect) QuoteIdent(name string) string {
	if !SafeIdentifier(name) {
		panic(fmt.Sprintf("test: invalid SQL identifier %q", name))
	}
	return "`" + name + "`"
}
func (testMySQLDialect) EncodeArg(val any) any {
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
func (testMySQLDialect) DecodeID(raw string) (string, error) { return raw, nil }
func (testMySQLDialect) ILikeClause(col, ph string) string {
	return "LOWER(" + col + ") LIKE LOWER(" + ph + ")"
}
func (testMySQLDialect) LikeClause(col, ph string) string {
	return "BINARY " + col + " LIKE " + ph
}
func (testMySQLDialect) NowExpr() string    { return "NOW()" }
func (testMySQLDialect) UTCNowExpr() string { return "NOW() AT TIME ZONE 'UTC'" }
func (testMySQLDialect) ApplyLimit(sql string, n int) string {
	return fmt.Sprintf("%s LIMIT %d", sql, n)
}
func (testMySQLDialect) ApplyLimitOffset(sql string, limit, offset int) string {
	return fmt.Sprintf("%s LIMIT %d OFFSET %d", sql, limit, offset)
}
func (testMySQLDialect) Savepoint(name string) string { return "SAVEPOINT " + name }
func (testMySQLDialect) RollbackToSavepoint(name string) string {
	return "ROLLBACK TO SAVEPOINT " + name
}
func (testMySQLDialect) ReleaseSavepoint(name string) string        { return "RELEASE SAVEPOINT " + name }
func (testMySQLDialect) IsUniqueViolation(error) (string, bool)     { return "", false }
func (testMySQLDialect) IsForeignKeyViolation(error) (string, bool) { return "", false }
func (testMySQLDialect) BuildUpsert(table string, _, _ []string, _ []UpsertSet) string {
	return "INSERT " + table
}

func TestMySQLVisitor_Operators(t *testing.T) {
	r := testResolver()
	cases := []struct {
		name string
		e    criteria.Expr
		sql  string
		args int
	}{
		{"eq", criteria.Eq("Email", "a@x"), "`email` = ?", 1},
		{"ne", criteria.Ne("Email", "a@x"), "`email` <> ?", 1},
		{"gt", criteria.Gt("Age", 18), "`age` > ?", 1},
		{"lte", criteria.Lte("Age", 18), "`age` <= ?", 1},
		{"like", criteria.Like("Name", "Bob%"), "BINARY `name` LIKE ?", 1},
		{"ilike", criteria.ILike("Name", "bob%"), "LOWER(`name`) LIKE LOWER(?)", 1},
		{"in", criteria.In("Name", "a", "b", "c"), "`name` IN (?, ?, ?)", 3},
		{"nin", criteria.Nin("Name", "a", "b"), "`name` NOT IN (?, ?)", 2},
		{"isnull", criteria.IsNull("Phone"), "`phone` IS NULL", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sql, args, err := CompileWhere(c.e, r, testMySQLDialect{}, nil)
			if err != nil {
				t.Fatalf("CompileWhere: %v", err)
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

// TestMySQLVisitor_PositionalArgOrder is the load-bearing test: with `?`
// placeholders the args slice order MUST match the left-to-right column order in
// the rendered SQL, or every value binds to the wrong column. PG catches an order
// bug because the visitor numbers `$n`; MySQL would silently misbind, so the
// order is asserted explicitly here.
func TestMySQLVisitor_PositionalArgOrder(t *testing.T) {
	e := criteria.And(
		criteria.Eq("Name", "a"),
		criteria.In("Email", "x", "y"),
		criteria.Gt("Age", 1),
	)
	sql, args, err := CompileWhere(e, testResolver(), testMySQLDialect{}, nil)
	if err != nil {
		t.Fatalf("CompileWhere: %v", err)
	}
	if want := "(`name` = ? AND `email` IN (?, ?) AND `age` > ?)"; sql != want {
		t.Errorf("sql  = %q\nwant = %q", sql, want)
	}
	want := []any{"a", "x", "y", 1}
	if len(args) != len(want) {
		t.Fatalf("args = %v, want %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Errorf("args[%d] = %v, want %v — positional bind order drifted", i, args[i], want[i])
		}
	}
}

// TestMySQLVisitor_DomainIDArgEncodedToBinary proves a domain.ID in the WHERE
// predicate reaches the args slice as its 16-byte BINARY(16) form on MySQL (PG
// keeps it a string) — the read-side mirror of the write-path id encoding.
func TestMySQLVisitor_DomainIDArgEncodedToBinary(t *testing.T) {
	u := uuid.MustParse("018f8b2c-1d3e-7a9b-bc4d-5e6f7a8b9c0d")
	_, args, err := CompileWhere(criteria.Eq("ID", domain.NewID(u.String())), testResolver(), testMySQLDialect{}, nil)
	if err != nil {
		t.Fatalf("CompileWhere: %v", err)
	}
	b, ok := args[0].([]byte)
	if !ok || len(b) != 16 {
		t.Fatalf("args[0] = %v (%T), want a 16-byte BINARY(16) form", args[0], args[0])
	}
}

// AllowsSubqueryOnWriteTarget mirrors the real MySQL dialect: it is the one
// supported engine that refuses a subquery reading the statement's own write
// target (error 1093), and the fake has to say so for that refusal to be
// exercised here.
func (testMySQLDialect) AllowsSubqueryOnWriteTarget() bool { return false }
