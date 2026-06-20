package web

import (
	"reflect"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/queries"
)

// ─── coerceValue: the branches not reached by the end-to-end coerce tests ────

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

// ─── applyFilterParam: every operator branch + MultiClause folding ───────────

func TestApplyFilterParam_Operators(t *testing.T) {
	spec := filterSpec{docPath: "name", goKind: reflect.String}

	check := func(op string, assert func(t *testing.T, clause any)) {
		t.Run(op, func(t *testing.T) {
			f := map[string]any{}
			applyFilterParam(f, spec, op, "Bob")
			assert(t, f["name"])
		})
	}

	sub := func(clause any) map[string]any {
		m, _ := clause.(map[string]any)
		return m
	}

	check("ne", func(t *testing.T, c any) {
		if sub(c)["$ne"] != "Bob" {
			t.Fatalf("$ne = %v", c)
		}
	})
	check("gte", func(t *testing.T, c any) {
		if sub(c)["$gte"] != "Bob" {
			t.Fatalf("$gte = %v", c)
		}
	})
	check("lte", func(t *testing.T, c any) {
		if sub(c)["$lte"] != "Bob" {
			t.Fatalf("$lte = %v", c)
		}
	})
	check("gt", func(t *testing.T, c any) {
		if sub(c)["$gt"] != "Bob" {
			t.Fatalf("$gt = %v", c)
		}
	})
	check("lt", func(t *testing.T, c any) {
		if sub(c)["$lt"] != "Bob" {
			t.Fatalf("$lt = %v", c)
		}
	})
	check("startswith", func(t *testing.T, c any) {
		if sub(c)["$regex"] != "^Bob" {
			t.Fatalf("startswith = %v", c)
		}
	})
	check("contains", func(t *testing.T, c any) {
		if sub(c)["$regex"] != "Bob" {
			t.Fatalf("contains = %v", c)
		}
	})
	check("ieq", func(t *testing.T, c any) {
		m := sub(c)
		if m["$regex"] != "^Bob$" || m["$options"] != "i" {
			t.Fatalf("ieq = %v", c)
		}
	})
	check("ine", func(t *testing.T, c any) {
		not, ok := sub(c)["$not"].(map[string]any)
		if !ok || not["$regex"] != "^Bob$" {
			t.Fatalf("ine = %v", c)
		}
	})
	check("istartswith", func(t *testing.T, c any) {
		m := sub(c)
		if m["$regex"] != "^Bob" || m["$options"] != "i" {
			t.Fatalf("istartswith = %v", c)
		}
	})
	check("icontains", func(t *testing.T, c any) {
		m := sub(c)
		if m["$regex"] != "Bob" || m["$options"] != "i" {
			t.Fatalf("icontains = %v", c)
		}
	})
	check("iin", func(t *testing.T, c any) {
		rml, ok := c.(queries.RegexMatchList)
		if !ok || !rml.CaseInsensitive || rml.Negate {
			t.Fatalf("iin = %#v", c)
		}
	})
	check("inin", func(t *testing.T, c any) {
		rml, ok := c.(queries.RegexMatchList)
		if !ok || !rml.CaseInsensitive || !rml.Negate {
			t.Fatalf("inin = %#v", c)
		}
	})
}

func TestApplyFilterParam_InAndNin(t *testing.T) {
	spec := filterSpec{docPath: "age", goKind: reflect.Int}
	f := map[string]any{}
	applyFilterParam(f, spec, "in", "1,2,3")
	in := f["age"].(map[string]any)["$in"].([]any)
	if len(in) != 3 || in[0].(int64) != 1 {
		t.Fatalf("$in = %v", f["age"])
	}
	g := map[string]any{}
	applyFilterParam(g, spec, "nin", "4,5")
	nin := g["age"].(map[string]any)["$nin"].([]any)
	if len(nin) != 2 {
		t.Fatalf("$nin = %v", g["age"])
	}
}

func TestApplyFilterParam_UnknownOperatorNoOp(t *testing.T) {
	spec := filterSpec{docPath: "name", goKind: reflect.String}
	f := map[string]any{}
	applyFilterParam(f, spec, "bogus", "Bob")
	if _, present := f["name"]; present {
		t.Fatalf("unknown operator must not write a clause, got %v", f)
	}
}

func TestApplyFilterParam_MultipleOperatorsFoldIntoMultiClause(t *testing.T) {
	spec := filterSpec{docPath: "name", goKind: reflect.String}
	f := map[string]any{}
	applyFilterParam(f, spec, "startswith", "Bo")
	applyFilterParam(f, spec, "icontains", "ob")
	mc, ok := f["name"].(queries.MultiClause)
	if !ok {
		t.Fatalf("expected MultiClause after two ops, got %T", f["name"])
	}
	if len(mc.Clauses) != 2 {
		t.Fatalf("expected 2 folded clauses, got %d", len(mc.Clauses))
	}
	// a third operator appends to the existing MultiClause
	applyFilterParam(f, spec, "contains", "b")
	mc = f["name"].(queries.MultiClause)
	if len(mc.Clauses) != 3 {
		t.Fatalf("expected 3 folded clauses, got %d", len(mc.Clauses))
	}
}

// ─── parseExportSort: prefix handling, empty-token skip, unknown token ───────

func TestParseExportSort(t *testing.T) {
	wireToGo := map[string]string{"name": "Name", "age": "Age"}

	sf, bad, ok := parseExportSort("name,-age", wireToGo)
	if !ok || bad != "" {
		t.Fatalf("expected ok, got bad=%q ok=%v", bad, ok)
	}
	if len(sf) != 2 || sf[0].Field != "Name" || sf[0].Desc {
		t.Fatalf("first sort = %+v", sf[0])
	}
	if sf[1].Field != "Age" || !sf[1].Desc {
		t.Fatalf("second sort (desc) = %+v", sf[1])
	}

	// leading/empty tokens are skipped
	sf, _, ok = parseExportSort("name, ,", wireToGo)
	if !ok || len(sf) != 1 {
		t.Fatalf("empty tokens not skipped: %+v ok=%v", sf, ok)
	}

	// unknown token returns it verbatim and ok=false
	_, bad, ok = parseExportSort("name,-bogus", wireToGo)
	if ok || bad != "-bogus" {
		t.Fatalf("expected unknown token '-bogus', got bad=%q ok=%v", bad, ok)
	}
}

// ─── buildKeyfunc: the PEM-parse-error branch ────────────────────────────────

func TestBuildKeyfunc_InvalidPEMReturnsError(t *testing.T) {
	_, err := buildKeyfunc(AuthOptions{PublicKeyPEM: "not a real pem"})
	if err == nil {
		t.Fatal("expected error from an unparsable PEM public key")
	}
}

// ─── formatPathIDConflict: the rendered diagnostic ───────────────────────────

func TestFormatPathIDConflict_MentionsWrapperAndRequest(t *testing.T) {
	msg := formatPathIDConflict("HandleCommandWithBodyID", reflect.TypeOf(struct{ X int }{}))
	if !strings.Contains(msg, "HandleCommandWithBodyID") {
		t.Errorf("diagnostic must name the wrapper: %s", msg)
	}
	if !strings.Contains(msg, `path:"id"`) {
		t.Errorf("diagnostic must mention the path:%q tag: %s", "id", msg)
	}
}
