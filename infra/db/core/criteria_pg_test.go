package core

import (
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/criteria"
)

func testResolver() FieldResolver {
	m := map[string]string{"ID": "id", "Email": "email", "Name": "name", "Age": "age", "Phone": "phone"}
	return func(f string) (ResolvedField, bool) {
		c, ok := m[f]
		return ResolvedField{Column: c}, ok
	}
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
			sql, args, err := CompileWhere(c.e, r, testPGDialect{}, nil)
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

func TestPgVisitor_NestingAndPrecedence(t *testing.T) {
	e := criteria.And(
		criteria.Eq("Name", "Bob"),
		criteria.Or(criteria.Eq("Email", "a@x"), criteria.Eq("Email", "b@x")),
		criteria.Not(criteria.IsNull("Phone")),
	)
	sql, args, err := CompileWhere(e, testResolver(), testPGDialect{}, nil)
	if err != nil {
		t.Fatalf("CompileWhere: %v", err)
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
	sql, args, err := CompileWhere(nil, testResolver(), testPGDialect{}, nil)
	if err != nil || sql != "" || args != nil {
		t.Errorf("nil where: sql=%q args=%v err=%v", sql, args, err)
	}
}

func TestPgVisitor_UnknownFieldErrors(t *testing.T) {
	if _, _, err := CompileWhere(criteria.Eq("Nope", "x"), testResolver(), testPGDialect{}, nil); err == nil {
		t.Fatal("expected unknown-field error")
	}
}

// Fix #12 (contract change): a zero-value IN is a well-defined empty-set
// predicate, not an error. SQL forbids the literal `IN ()`, so the translator
// renders IN () → match nothing (1=0) and NOT IN () → match everything (1=1),
// matching MongoDB's $in:[] / $nin:[] semantics so the relational and Mongo read
// paths agree (previously this returned an error → 500).
func TestPgVisitor_ZeroValueInMatchesNothing(t *testing.T) {
	sql, args, err := CompileWhere(criteria.In("Name"), testResolver(), testPGDialect{}, nil)
	if err != nil {
		t.Fatalf("empty In must not error, got %v", err)
	}
	if !strings.Contains(sql, "1=0") {
		t.Errorf("empty In must render match-nothing, got %q", sql)
	}
	if len(args) != 0 {
		t.Errorf("empty In takes no args, got %v", args)
	}

	sql, _, err = CompileWhere(criteria.Nin("Name"), testResolver(), testPGDialect{}, nil)
	if err != nil {
		t.Fatalf("empty Nin must not error, got %v", err)
	}
	if !strings.Contains(sql, "1=1") {
		t.Errorf("empty Nin must render match-everything, got %q", sql)
	}
}

func TestPgVisitor_CardinalityErrors(t *testing.T) {
	// Hand-built Comparisons violating per-operator cardinality.
	if _, _, err := CompileWhere(criteria.Comparison{Field: "Name", Op: criteria.OpEq}, testResolver(), testPGDialect{}, nil); err == nil {
		t.Error("expected error: eq with zero values")
	}
	if _, _, err := CompileWhere(criteria.Comparison{Field: "Phone", Op: criteria.OpIsNull, Values: []any{"x"}}, testResolver(), testPGDialect{}, nil); err == nil {
		t.Error("expected error: isnull with values")
	}
}

func TestPgVisitor_EmptyLogicalErrors(t *testing.T) {
	if _, _, err := CompileWhere(criteria.And(), testResolver(), testPGDialect{}, nil); err == nil {
		t.Error("expected error: And with no operands")
	}
}

func TestPgVisitor_DomainIDArgUnwrapped(t *testing.T) {
	_, args, err := CompileWhere(criteria.Eq("ID", domain.NewID("the-id")), testResolver(), testPGDialect{}, nil)
	if err != nil {
		t.Fatalf("CompileWhere: %v", err)
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
	sql, args, err := CompileWhere(e, testResolver(), testPGDialect{}, nil)
	if err != nil {
		t.Fatalf("CompileWhere: %v", err)
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
	std := NewExternalSchema("t").DeletedAt("deleted_at")
	if ScopeGate(criteria.ScopeActive, std, testPGDialect{}, "") != "deleted_at IS NULL" {
		t.Error("active")
	}
	if ScopeGate(criteria.ScopeIncludeArchived, std, testPGDialect{}, "") != "" {
		t.Error("include")
	}
	if ScopeGate(criteria.ScopeOnlyArchived, std, testPGDialect{}, "") != "deleted_at IS NOT NULL" {
		t.Error("only")
	}
	// Renamed DeletedAt column flows through the gate.
	renamed := NewExternalSchema("t").DeletedAt("removed_at")
	if ScopeGate(criteria.ScopeActive, renamed, testPGDialect{}, "") != "removed_at IS NULL" {
		t.Error("renamed DeletedAt column")
	}
	// No DeletedAt declared → no gate under any scope.
	off := NewExternalSchema("t")
	if ScopeGate(criteria.ScopeActive, off, testPGDialect{}, "") != "" || ScopeGate(criteria.ScopeOnlyArchived, off, testPGDialect{}, "") != "" {
		t.Error("disabled DeletedAt must yield no gate")
	}
}

func TestChildScopeFilter(t *testing.T) {
	std := NewExternalSchema("t").DeletedAt("deleted_at")
	if ChildScopeFilter(criteria.ScopeActive, std, testPGDialect{}, "") != "AND deleted_at IS NULL" {
		t.Error("active children gated")
	}
	if ChildScopeFilter(criteria.ScopeIncludeArchived, std, testPGDialect{}, "") != "" {
		t.Error("include: children unfiltered")
	}
	if ChildScopeFilter(criteria.ScopeOnlyArchived, std, testPGDialect{}, "") != "" {
		t.Error("only: children unfiltered (cascade visibility)")
	}
}

// Under a JOIN that brings a second archivable table into scope (a role's
// SharedBase in ScopeGate, or the role in the base-child loader), the
// DeletedAt column must be table-qualified so the bare reference is not
// ambiguous (SQLSTATE 42702) — the same disambiguation the leading ID already
// gets. With an empty qualifier the output stays bare (single-table path).
func TestScopeGate_QualifiedUnderJoin(t *testing.T) {
	std := NewExternalSchema("t").DeletedAt("deleted_at")
	if got := ScopeGate(criteria.ScopeActive, std, testPGDialect{}, "users"); got != "users.deleted_at IS NULL" {
		t.Errorf("qualified active gate = %q, want users.deleted_at IS NULL", got)
	}
	if got := ScopeGate(criteria.ScopeOnlyArchived, std, testPGDialect{}, "users"); got != "users.deleted_at IS NOT NULL" {
		t.Errorf("qualified archived gate = %q", got)
	}
	if got := ChildScopeFilter(criteria.ScopeActive, std, testPGDialect{}, "addresses"); got != "AND addresses.deleted_at IS NULL" {
		t.Errorf("qualified base-child filter = %q, want AND addresses.deleted_at IS NULL", got)
	}
}

func TestCompileOrder(t *testing.T) {
	r := testResolver()
	s, err := CompileOrder([]criteria.OrderField{{Field: "Name"}, {Field: "Email", Desc: true}}, r, testPGDialect{})
	if err != nil {
		t.Fatalf("CompileOrder: %v", err)
	}
	if s != "ORDER BY name ASC, email DESC" {
		t.Errorf("order = %q", s)
	}
	if got, _ := CompileOrder(nil, r, testPGDialect{}); got != "" {
		t.Errorf("empty order = %q, want \"\"", got)
	}
	if _, err := CompileOrder([]criteria.OrderField{{Field: "Nope"}}, r, testPGDialect{}); err == nil {
		t.Error("expected unknown order-field error")
	}
}

func TestBuildWhereClause(t *testing.T) {
	if got := BuildWhereClause("a = $1", "deleted_at IS NULL"); got != "WHERE a = $1 AND deleted_at IS NULL" {
		t.Errorf("both = %q", got)
	}
	if got := BuildWhereClause("", "deleted_at IS NULL"); got != "WHERE deleted_at IS NULL" {
		t.Errorf("gate only = %q", got)
	}
	if got := BuildWhereClause("a = $1", ""); got != "WHERE a = $1" {
		t.Errorf("where only = %q", got)
	}
	if got := BuildWhereClause("", ""); got != "" {
		t.Errorf("none = %q", got)
	}
}
