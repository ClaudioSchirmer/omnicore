package criteria

import "testing"

func TestByID(t *testing.T) {
	q := ByID("abc")
	c, ok := q.Condition().(Comparison)
	if !ok {
		t.Fatalf("ByID condition is %T, want Comparison", q.Condition())
	}
	if c.Field != "ID" || c.Op != OpEq || len(c.Values) != 1 || c.Values[0] != "abc" {
		t.Errorf("ByID = %+v", c)
	}
}

func TestQuerySetters(t *testing.T) {
	q := Where(Eq("Name", "x")).OrderBy("A").OrderByDesc("B").Limit(5).IncludeArchived()
	if q.LimitValue() != 5 {
		t.Errorf("limit = %d, want 5", q.LimitValue())
	}
	if q.Scope() != ScopeIncludeArchived {
		t.Errorf("scope = %d, want IncludeArchived", q.Scope())
	}
	of := q.OrderFields()
	if len(of) != 2 || of[0].Field != "A" || of[0].Desc || of[1].Field != "B" || !of[1].Desc {
		t.Errorf("order fields = %+v", of)
	}
}

func TestScopeSetters(t *testing.T) {
	if Where(nil).Scope() != ScopeActive {
		t.Error("default scope should be Active")
	}
	if Where(nil).OnlyArchived().Scope() != ScopeOnlyArchived {
		t.Error("OnlyArchived")
	}
}

func TestWhereNil(t *testing.T) {
	if Where(nil).Condition() != nil {
		t.Error("Where(nil) should carry a nil condition")
	}
}

func TestContainsEscaping(t *testing.T) {
	c := Contains("Name", "50%_x").(Comparison)
	if c.Op != OpILike {
		t.Errorf("Contains op = %s, want ilike", c.Op)
	}
	got := c.Values[0].(string)
	want := `%50\%\_x%`
	if got != want {
		t.Errorf("Contains pattern = %q, want %q", got, want)
	}
}

func TestStartsWithEndsWith(t *testing.T) {
	if got := StartsWith("N", "a").(Comparison).Values[0]; got != `a%` {
		t.Errorf("StartsWith = %q, want %q", got, `a%`)
	}
	if got := EndsWith("N", "a").(Comparison).Values[0]; got != `%a` {
		t.Errorf("EndsWith = %q, want %q", got, `%a`)
	}
}

func TestBetween(t *testing.T) {
	l, ok := Between("Age", 1, 10).(Logical)
	if !ok || l.Op != LogicalAnd || len(l.Operands) != 2 {
		t.Fatalf("Between = %+v", l)
	}
}

func TestAndOrNot(t *testing.T) {
	if And(Eq("A", 1)).(Logical).Op != LogicalAnd {
		t.Error("And")
	}
	if Or(Eq("A", 1)).(Logical).Op != LogicalOr {
		t.Error("Or")
	}
	if _, ok := Not(Eq("A", 1)).(Negation); !ok {
		t.Error("Not should produce a Negation")
	}
}

func TestLeafCardinality(t *testing.T) {
	if c := In("X", "a", "b", "c").(Comparison); len(c.Values) != 3 || c.Op != OpIn {
		t.Errorf("In = %+v", c)
	}
	if c := IsNull("X").(Comparison); len(c.Values) != 0 || c.Op != OpIsNull {
		t.Errorf("IsNull = %+v", c)
	}
	if c := Eq("X", 1).(Comparison); len(c.Values) != 1 {
		t.Errorf("Eq values = %d, want 1", len(c.Values))
	}
}
