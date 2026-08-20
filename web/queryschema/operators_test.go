package queryschema

import (
	"reflect"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/queries"
)

// ─── coerceValue: every kind branch ─────────────────────────────────────────

func TestCoerceValue_AllKinds(t *testing.T) {
	cases := []struct {
		name string
		in   string
		kind reflect.Kind
		want any
	}{
		{"string", "95014", reflect.String, "95014"},
		{"int", "25", reflect.Int, int64(25)},
		{"int-bad", "x", reflect.Int, "x"},
		{"uint", "7", reflect.Uint, uint64(7)},
		{"uint8", "9", reflect.Uint8, uint64(9)},
		{"uint-bad", "-1", reflect.Uint, "-1"},
		{"float32", "1.5", reflect.Float32, 1.5},
		{"float-bad", "nope", reflect.Float64, "nope"},
		{"bool-true", "true", reflect.Bool, true},
		{"bool-false", "false", reflect.Bool, false},
		{"bool-fallback", "maybe", reflect.Bool, "maybe"},
		{"default-kind", "raw", reflect.Slice, "raw"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := coerceValue(c.in, c.kind); got != c.want {
				t.Errorf("coerceValue(%q,%v) = %v (%T), want %v (%T)", c.in, c.kind, got, got, c.want, c.want)
			}
		})
	}
}

// ─── ApplyFilterValues: every operator branch + MultiClause folding ─────────

func TestApplyFilterValues_Operators(t *testing.T) {
	spec := FilterSpec{DocPath: "name", GoKind: reflect.String}

	check := func(op string, assert func(t *testing.T, clause any)) {
		t.Run(op, func(t *testing.T) {
			f := map[string]any{}
			ApplyFilterValues(f, spec, op, []string{"Bob"})
			assert(t, f["name"])
		})
	}

	// clauseVal asserts the value is a queries.Clause with the given neutral op
	// carrying a single scalar operand, and returns that operand.
	clauseVal := func(t *testing.T, c any, want queries.FilterOp) any {
		cl, ok := c.(queries.Clause)
		if !ok || cl.Op != want || len(cl.Values) != 1 {
			t.Fatalf("%s = %#v", want, c)
		}
		return cl.Values[0]
	}
	text := func(clause any) queries.TextMatch {
		tm, _ := clause.(queries.TextMatch)
		return tm
	}

	check("ne", func(t *testing.T, c any) {
		if clauseVal(t, c, queries.FilterNe) != "Bob" {
			t.Fatalf("ne = %v", c)
		}
	})
	check("gte", func(t *testing.T, c any) {
		if clauseVal(t, c, queries.FilterGte) != "Bob" {
			t.Fatalf("gte = %v", c)
		}
	})
	check("lte", func(t *testing.T, c any) {
		if clauseVal(t, c, queries.FilterLte) != "Bob" {
			t.Fatalf("lte = %v", c)
		}
	})
	check("gt", func(t *testing.T, c any) {
		if clauseVal(t, c, queries.FilterGt) != "Bob" {
			t.Fatalf("gt = %v", c)
		}
	})
	check("lt", func(t *testing.T, c any) {
		if clauseVal(t, c, queries.FilterLt) != "Bob" {
			t.Fatalf("lt = %v", c)
		}
	})
	check("startswith", func(t *testing.T, c any) {
		if tm := text(c); tm.Value != "Bob" || tm.Kind != queries.TextPrefix || tm.CaseInsensitive || tm.Negate {
			t.Fatalf("startswith = %#v", c)
		}
	})
	check("contains", func(t *testing.T, c any) {
		if tm := text(c); tm.Value != "Bob" || tm.Kind != queries.TextContains || tm.CaseInsensitive || tm.Negate {
			t.Fatalf("contains = %#v", c)
		}
	})
	check("ieq", func(t *testing.T, c any) {
		if tm := text(c); tm.Value != "Bob" || tm.Kind != queries.TextExact || !tm.CaseInsensitive || tm.Negate {
			t.Fatalf("ieq = %#v", c)
		}
	})
	check("ine", func(t *testing.T, c any) {
		if tm := text(c); tm.Value != "Bob" || tm.Kind != queries.TextExact || !tm.CaseInsensitive || !tm.Negate {
			t.Fatalf("ine = %#v", c)
		}
	})
	check("istartswith", func(t *testing.T, c any) {
		if tm := text(c); tm.Value != "Bob" || tm.Kind != queries.TextPrefix || !tm.CaseInsensitive || tm.Negate {
			t.Fatalf("istartswith = %#v", c)
		}
	})
	check("icontains", func(t *testing.T, c any) {
		if tm := text(c); tm.Value != "Bob" || tm.Kind != queries.TextContains || !tm.CaseInsensitive || tm.Negate {
			t.Fatalf("icontains = %#v", c)
		}
	})
	check("iin", func(t *testing.T, c any) {
		rml, ok := c.(queries.TextMatchList)
		if !ok || !rml.CaseInsensitive || rml.Negate {
			t.Fatalf("iin = %#v", c)
		}
	})
	check("inin", func(t *testing.T, c any) {
		rml, ok := c.(queries.TextMatchList)
		if !ok || !rml.CaseInsensitive || !rml.Negate {
			t.Fatalf("inin = %#v", c)
		}
	})
}

func TestApplyFilterParam_InAndNin(t *testing.T) {
	spec := FilterSpec{DocPath: "age", GoKind: reflect.Int}
	f := map[string]any{}
	ApplyFilterValues(f, spec, "in", []string{"1", "2", "3"})
	inCl, ok := f["age"].(queries.Clause)
	if !ok || inCl.Op != queries.FilterIn || len(inCl.Values) != 3 || inCl.Values[0].(int64) != 1 {
		t.Fatalf("in = %#v", f["age"])
	}
	g := map[string]any{}
	ApplyFilterValues(g, spec, "nin", []string{"4", "5"})
	ninCl, ok := g["age"].(queries.Clause)
	if !ok || ninCl.Op != queries.FilterNin || len(ninCl.Values) != 2 {
		t.Fatalf("nin = %#v", g["age"])
	}
}

func TestApplyFilterParam_UnknownOperatorNoOp(t *testing.T) {
	spec := FilterSpec{DocPath: "name", GoKind: reflect.String}
	f := map[string]any{}
	ApplyFilterValues(f, spec, "bogus", []string{"Bob"})
	if _, present := f["name"]; present {
		t.Fatalf("unknown operator must not write a clause, got %v", f)
	}
}

func TestApplyFilterParam_MultipleOperatorsFoldIntoMultiClause(t *testing.T) {
	spec := FilterSpec{DocPath: "name", GoKind: reflect.String}
	f := map[string]any{}
	ApplyFilterValues(f, spec, "startswith", []string{"Bo"})
	ApplyFilterValues(f, spec, "icontains", []string{"ob"})
	mc, ok := f["name"].(queries.MultiClause)
	if !ok {
		t.Fatalf("expected MultiClause after two ops, got %T", f["name"])
	}
	if len(mc.Clauses) != 2 {
		t.Fatalf("expected 2 folded clauses, got %d", len(mc.Clauses))
	}
	// a third operator appends to the existing MultiClause
	ApplyFilterValues(f, spec, "contains", []string{"b"})
	mc = f["name"].(queries.MultiClause)
	if len(mc.Clauses) != 3 {
		t.Fatalf("expected 3 folded clauses, got %d", len(mc.Clauses))
	}
}

// ─── ParseKeyAgainstSchema: known-op suffix peeling rules ────────────────────

type opParseRequest struct {
	Name *string `query:"name" filter:"eq,in"`
}

func TestParseKeyAgainstSchema_UnknownPrefixWithKnownOp(t *testing.T) {
	s := ExtractRequestSchema(reflect.TypeOf(opParseRequest{}))
	wirePath, op := ParseKeyAgainstSchema("bogus.in", s)
	if wirePath != "" || op != "" {
		t.Fatalf("unknown prefix with known op must reject, got (%q,%q)", wirePath, op)
	}
}

func TestParseKeyAgainstSchema_NoDotReturnsEmpty(t *testing.T) {
	s := ExtractRequestSchema(reflect.TypeOf(opParseRequest{}))
	wirePath, op := ParseKeyAgainstSchema("totallyunknown", s)
	if wirePath != "" || op != "" {
		t.Fatalf("unknown dotless key must reject, got (%q,%q)", wirePath, op)
	}
}

func TestParseKeyAgainstSchema_WholeKeyFirstAndOpSuffix(t *testing.T) {
	s := ExtractRequestSchema(reflect.TypeOf(opParseRequest{}))
	// Whole key is a declared filter → no operator peeled.
	if wp, op := ParseKeyAgainstSchema("name", s); wp != "name" || op != "" {
		t.Fatalf("whole-key match = (%q,%q), want (name,\"\")", wp, op)
	}
	// Known op suffix on a declared filter → peeled.
	if wp, op := ParseKeyAgainstSchema("name.in", s); wp != "name" || op != "in" {
		t.Fatalf("op-suffix match = (%q,%q), want (name,in)", wp, op)
	}
}
