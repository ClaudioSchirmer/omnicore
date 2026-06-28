package relational

import (
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/criteria"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

func testResolver() core.FieldResolver {
	m := map[string]string{"ID": "id", "Email": "email", "Name": "name", "Age": "age", "Phone": "phone"}
	return func(f string) (string, bool) { c, ok := m[f]; return c, ok }
}

func TestPgVisitor_Operators(t *testing.T) {
	r := testResolver()
	cases := []struct {
		name string
		e    criteria.Expr
		sql  string
		args int
	}{
		{"eq", criteria.Eq("Email", "a@x"), "email = $1", 1},
		{"ne", criteria.Ne("Email", "a@x"), "email <> $1", 1},
		{"gt", criteria.Gt("Age", 18), "age > $1", 1},
		{"gte", criteria.Gte("Age", 18), "age >= $1", 1},
		{"lt", criteria.Lt("Age", 18), "age < $1", 1},
		{"lte", criteria.Lte("Age", 18), "age <= $1", 1},
		{"like", criteria.Like("Name", "Bob%"), "name LIKE $1", 1},
		{"ilike", criteria.ILike("Name", "bob%"), "name ILIKE $1", 1},
		{"in", criteria.In("Name", "a", "b", "c"), "name IN ($1, $2, $3)", 3},
		{"nin", criteria.Nin("Name", "a", "b"), "name NOT IN ($1, $2)", 2},
		{"isnull", criteria.IsNull("Phone"), "phone IS NULL", 0},
		{"notnull", criteria.NotNull("Phone"), "phone IS NOT NULL", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sql, args, err := compileWhere(c.e, r, testPGDialect{})
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

func TestPgVisitor_NestingAndPrecedence(t *testing.T) {
	e := criteria.And(
		criteria.Eq("Name", "Bob"),
		criteria.Or(criteria.Eq("Email", "a@x"), criteria.Eq("Email", "b@x")),
		criteria.Not(criteria.IsNull("Phone")),
	)
	sql, args, err := compileWhere(e, testResolver(), testPGDialect{})
	if err != nil {
		t.Fatalf("compileWhere: %v", err)
	}
	want := "(name = $1 AND (email = $2 OR email = $3) AND NOT (phone IS NULL))"
	if sql != want {
		t.Errorf("sql  = %q\nwant = %q", sql, want)
	}
	if len(args) != 3 {
		t.Errorf("args = %d, want 3", len(args))
	}
}

func TestPgVisitor_NilWhereIsEmpty(t *testing.T) {
	sql, args, err := compileWhere(nil, testResolver(), testPGDialect{})
	if err != nil || sql != "" || args != nil {
		t.Errorf("nil where: sql=%q args=%v err=%v", sql, args, err)
	}
}

func TestPgVisitor_UnknownFieldErrors(t *testing.T) {
	if _, _, err := compileWhere(criteria.Eq("Nope", "x"), testResolver(), testPGDialect{}); err == nil {
		t.Fatal("expected unknown-field error")
	}
}

func TestPgVisitor_ZeroValueInErrors(t *testing.T) {
	if _, _, err := compileWhere(criteria.In("Name"), testResolver(), testPGDialect{}); err == nil {
		t.Fatal("expected error on empty In (no IN ())")
	}
}

func TestPgVisitor_CardinalityErrors(t *testing.T) {
	// Hand-built Comparisons violating per-operator cardinality.
	if _, _, err := compileWhere(criteria.Comparison{Field: "Name", Op: criteria.OpEq}, testResolver(), testPGDialect{}); err == nil {
		t.Error("expected error: eq with zero values")
	}
	if _, _, err := compileWhere(criteria.Comparison{Field: "Phone", Op: criteria.OpIsNull, Values: []any{"x"}}, testResolver(), testPGDialect{}); err == nil {
		t.Error("expected error: isnull with values")
	}
}

func TestPgVisitor_EmptyLogicalErrors(t *testing.T) {
	if _, _, err := compileWhere(criteria.And(), testResolver(), testPGDialect{}); err == nil {
		t.Error("expected error: And with no operands")
	}
}

func TestPgVisitor_DomainIDArgUnwrapped(t *testing.T) {
	_, args, err := compileWhere(criteria.Eq("ID", domain.NewID("the-id")), testResolver(), testPGDialect{})
	if err != nil {
		t.Fatalf("compileWhere: %v", err)
	}
	if len(args) != 1 || args[0] != "the-id" {
		t.Errorf("args = %v, want [\"the-id\"] (domain.ID unwrapped via Value())", args)
	}
}

func TestPgVisitor_PlaceholderNumberingMonotonic(t *testing.T) {
	e := criteria.And(
		criteria.Eq("Name", "a"),
		criteria.In("Email", "x", "y"),
		criteria.Gt("Age", 1),
	)
	sql, args, err := compileWhere(e, testResolver(), testPGDialect{})
	if err != nil {
		t.Fatalf("compileWhere: %v", err)
	}
	want := "(name = $1 AND email IN ($2, $3) AND age > $4)"
	if sql != want {
		t.Errorf("sql = %q, want %q", sql, want)
	}
	if len(args) != 4 {
		t.Errorf("args = %d, want 4", len(args))
	}
}

func TestScopeGate(t *testing.T) {
	std := NewExternalSchema("t").SoftDelete("deleted_at")
	if scopeGate(criteria.ScopeActive, std, testPGDialect{}) != "deleted_at IS NULL" {
		t.Error("active")
	}
	if scopeGate(criteria.ScopeIncludeArchived, std, testPGDialect{}) != "" {
		t.Error("include")
	}
	if scopeGate(criteria.ScopeOnlyArchived, std, testPGDialect{}) != "deleted_at IS NOT NULL" {
		t.Error("only")
	}
	// Renamed soft-delete column flows through the gate.
	renamed := NewExternalSchema("t").SoftDelete("removed_at")
	if scopeGate(criteria.ScopeActive, renamed, testPGDialect{}) != "removed_at IS NULL" {
		t.Error("renamed soft-delete column")
	}
	// No soft-delete declared → no gate under any scope.
	off := NewExternalSchema("t")
	if scopeGate(criteria.ScopeActive, off, testPGDialect{}) != "" || scopeGate(criteria.ScopeOnlyArchived, off, testPGDialect{}) != "" {
		t.Error("disabled soft-delete must yield no gate")
	}
}

func TestChildScopeFilter(t *testing.T) {
	std := NewExternalSchema("t").SoftDelete("deleted_at")
	if childScopeFilter(criteria.ScopeActive, std, testPGDialect{}) != "AND deleted_at IS NULL" {
		t.Error("active children gated")
	}
	if childScopeFilter(criteria.ScopeIncludeArchived, std, testPGDialect{}) != "" {
		t.Error("include: children unfiltered")
	}
	if childScopeFilter(criteria.ScopeOnlyArchived, std, testPGDialect{}) != "" {
		t.Error("only: children unfiltered (cascade visibility)")
	}
}

func TestCompileOrder(t *testing.T) {
	r := testResolver()
	s, err := compileOrder([]criteria.OrderField{{Field: "Name"}, {Field: "Email", Desc: true}}, r, testPGDialect{})
	if err != nil {
		t.Fatalf("compileOrder: %v", err)
	}
	if s != "ORDER BY name ASC, email DESC" {
		t.Errorf("order = %q", s)
	}
	if got, _ := compileOrder(nil, r, testPGDialect{}); got != "" {
		t.Errorf("empty order = %q, want \"\"", got)
	}
	if _, err := compileOrder([]criteria.OrderField{{Field: "Nope"}}, r, testPGDialect{}); err == nil {
		t.Error("expected unknown order-field error")
	}
}

func TestBuildWhereClause(t *testing.T) {
	if got := buildWhereClause("a = $1", "deleted_at IS NULL"); got != "WHERE a = $1 AND deleted_at IS NULL" {
		t.Errorf("both = %q", got)
	}
	if got := buildWhereClause("", "deleted_at IS NULL"); got != "WHERE deleted_at IS NULL" {
		t.Errorf("gate only = %q", got)
	}
	if got := buildWhereClause("a = $1", ""); got != "WHERE a = $1" {
		t.Errorf("where only = %q", got)
	}
	if got := buildWhereClause("", ""); got != "" {
		t.Errorf("none = %q", got)
	}
}

type fieldResolverSample struct {
	domain.BaseEntity
	Name    string
	ZipCode string
}

func TestSchemaFieldResolver(t *testing.T) {
	r := NewTableSchema[fieldResolverSample]("t").
		PK("id").
		Field("Name", "full_name").
		Field("ZipCode", "zip_code").
		FieldResolver()
	if c, ok := r("ID"); !ok || c != "id" {
		t.Errorf("ID = %q,%v, want id,true", c, ok)
	}
	if c, ok := r("Name"); !ok || c != "full_name" {
		t.Errorf("Name = %q,%v, want full_name", c, ok)
	}
	if c, ok := r("ZipCode"); !ok || c != "zip_code" {
		t.Errorf("ZipCode = %q,%v, want zip_code", c, ok)
	}
	if _, ok := r("Missing"); ok {
		t.Error("Missing should not resolve")
	}
}
