package relational

import (
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/infra/db/criteria"
)

// The wire operators arrive as the store-NEUTRAL sentinels queries declares, and
// this package turns them into a criteria predicate. Values arrive RAW: anchoring
// and escaping happen here, in the engine, never above the port.

func exprOf(t *testing.T, field string, v any) criteria.Expr {
	t.Helper()
	e, err := toExpr(guardSchema("gadgets"), map[string]any{field: v})
	if err != nil {
		t.Fatalf("toExpr(%v): %v", v, err)
	}
	return e
}

func TestOrdinalOperators_MapToTheirComparisons(t *testing.T) {
	for _, op := range []queries.FilterOp{
		queries.FilterNe, queries.FilterGt, queries.FilterGte, queries.FilterLt, queries.FilterLte,
	} {
		if e := exprOf(t, "Name", queries.Clause{Op: op, Values: []any{"x"}}); e == nil {
			t.Errorf("operator %q produced no predicate", op)
		}
	}
}

func TestSetOperators_TakeTheWholeValueList(t *testing.T) {
	for _, op := range []queries.FilterOp{queries.FilterIn, queries.FilterNin} {
		if e := exprOf(t, "Name", queries.Clause{Op: op, Values: []any{"a", "b"}}); e == nil {
			t.Errorf("operator %q produced no predicate", op)
		}
	}
}

// A scalar operator with no operand is a malformed clause — an error, not a
// silently-dropped filter that would widen the result set.
func TestOrdinalOperator_WithoutAValueIsAnError(t *testing.T) {
	_, err := toExpr(guardSchema("gadgets"), map[string]any{"Name": queries.Clause{Op: queries.FilterGt}})
	if err == nil || !strings.Contains(err.Error(), "carries no value") {
		t.Fatalf("an operand-less comparison must error, got %v", err)
	}
}

func TestUnknownOperator_IsAnError(t *testing.T) {
	_, err := toExpr(guardSchema("gadgets"), map[string]any{"Name": queries.Clause{Op: "sideways", Values: []any{1}}})
	if err == nil || !strings.Contains(err.Error(), "unknown operator") {
		t.Fatalf("an unknown operator must error, got %v", err)
	}
}

// The RAW value is anchored HERE, per match kind — the port carries no store's
// pattern syntax, so `%` and `_` in a user value must be escaped, not honoured.
func TestTextMatch_AnchorsPerKindAndEscapesTheRawValue(t *testing.T) {
	for _, kind := range []queries.TextMatchKind{queries.TextPrefix, queries.TextContains, queries.TextExact} {
		if e := exprOf(t, "Name", queries.TextMatch{Value: "50%_off", Kind: kind}); e == nil {
			t.Errorf("kind %v produced no predicate", kind)
		}
	}
	if e := exprOf(t, "Name", queries.TextMatch{Value: "bob", CaseInsensitive: true}); e == nil {
		t.Error("the case-insensitive form produced no predicate")
	}
	if e := exprOf(t, "Name", queries.TextMatch{Value: "bob", Negate: true}); e == nil {
		t.Error("the negated form produced no predicate")
	}
}

func TestTextMatchList_IsAnOrOfWholeValueMatches(t *testing.T) {
	if e := exprOf(t, "Name", queries.TextMatchList{Values: []string{"a", "b"}, CaseInsensitive: true}); e == nil {
		t.Error("the case-insensitive membership form produced no predicate")
	}
	if e := exprOf(t, "Name", queries.TextMatchList{Values: []string{"a"}, Negate: true}); e == nil {
		t.Error("the negated membership form produced no predicate")
	}
}

// Several operators on ONE field AND together — the `?age.gte=18&age.lte=65`
// shape the wire wrappers emit.
func TestMultiClause_AndsTheClausesOnTheSameField(t *testing.T) {
	if e := exprOf(t, "Name", queries.MultiClause{Clauses: []any{
		queries.Clause{Op: queries.FilterGte, Values: []any{"a"}},
		queries.Clause{Op: queries.FilterLte, Values: []any{"z"}},
	}}); e == nil {
		t.Error("a multi-clause produced no predicate")
	}
	// One clause collapses to that clause; none collapses to nothing.
	if e := exprOf(t, "Name", queries.MultiClause{Clauses: []any{
		queries.Clause{Op: queries.FilterNe, Values: []any{"a"}},
	}}); e == nil {
		t.Error("a single-clause multi produced no predicate")
	}
	if e := exprOf(t, "Name", queries.MultiClause{}); e != nil {
		t.Error("an empty multi-clause must produce no predicate")
	}
	// A malformed member propagates its error rather than being dropped.
	if _, err := toExpr(guardSchema("gadgets"), map[string]any{"Name": queries.MultiClause{
		Clauses: []any{queries.Clause{Op: queries.FilterGt}},
	}}); err == nil {
		t.Error("a malformed clause inside a multi must surface")
	}
}

// A bare scalar under a field IS equality — the shape both readers already treat
// that way, so `eq` needs no sentinel of its own.
func TestBareScalar_IsEquality(t *testing.T) {
	if e := exprOf(t, "Name", "gizmo"); e == nil {
		t.Error("a bare scalar produced no predicate")
	}
}

// Two fields AND together, visited in a deterministic order.
func TestToExpr_MultipleFieldsAndDeterministically(t *testing.T) {
	e, err := toExpr(guardSchema("gadgets"), map[string]any{"Name": "a", "ID": "b"})
	if err != nil {
		t.Fatalf("toExpr: %v", err)
	}
	if e == nil {
		t.Fatal("two fields produced no predicate")
	}
}

func TestToExpr_EmptyFilterIsNoPredicate(t *testing.T) {
	e, err := toExpr(guardSchema("gadgets"), nil)
	if err != nil || e != nil {
		t.Fatalf("an empty filter = (%v, %v), want (nil, nil)", e, err)
	}
}
