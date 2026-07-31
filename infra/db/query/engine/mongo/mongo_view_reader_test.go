package mongo

import (
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// translateFilterValue is the in-process bridge between the wire wrappers'
// port-level sentinels (queries.TextMatch / TextMatchList) and the
// MongoDB driver's native types (bson.Regex). Each case asserts on the
// returned BSON value so the suite stays free of a running Mongo.

func TestTranslateFilterValue_PassesThroughPlainValues(t *testing.T) {
	cases := []any{
		"Bob",
		42,
		true,
		map[string]any{"$gt": 10},
		bson.M{"$in": []any{"a", "b"}},
	}
	for _, in := range cases {
		out := translateFilterValue(in)
		// Plain values must come back identical (interface-equal). Tested
		// via fmt-sprint for the map cases since maps are not == in Go.
		switch v := in.(type) {
		case map[string]any:
			gotMap, ok := out.(map[string]any)
			if !ok || len(gotMap) != len(v) {
				t.Errorf("expected map passthrough for %v, got %T (%v)", in, out, out)
			}
		case bson.M:
			gotMap, ok := out.(bson.M)
			if !ok || len(gotMap) != len(v) {
				t.Errorf("expected bson.M passthrough for %v, got %T (%v)", in, out, out)
			}
		default:
			if out != in {
				t.Errorf("expected %v passthrough, got %v", in, out)
			}
		}
	}
}

func TestTranslateFilterValue_TextMatch_SensitiveBecomesBareRegex(t *testing.T) {
	got := translateFilterValue(queries.TextMatch{Value: "Bob", Kind: queries.TextPrefix})
	re, ok := got.(bson.Regex)
	if !ok {
		t.Fatalf("expected bson.Regex, got %T (%v)", got, got)
	}
	if re.Pattern != "^Bob" {
		t.Errorf("expected pattern '^Bob', got %q", re.Pattern)
	}
	if re.Options != "" {
		t.Errorf("expected empty options for case-sensitive match, got %q", re.Options)
	}
}

func TestTranslateFilterValue_TextMatch_InsensitiveAddsOption(t *testing.T) {
	got := translateFilterValue(queries.TextMatch{Value: "bob", Kind: queries.TextExact, CaseInsensitive: true})
	re, ok := got.(bson.Regex)
	if !ok {
		t.Fatalf("expected bson.Regex, got %T", got)
	}
	if re.Options != "i" {
		t.Errorf("expected options='i', got %q", re.Options)
	}
}

func TestTranslateFilterValue_TextMatch_NegateWrapsInNot(t *testing.T) {
	got := translateFilterValue(queries.TextMatch{Value: "bob", Kind: queries.TextExact, CaseInsensitive: true, Negate: true})
	doc, ok := got.(bson.M)
	if !ok {
		t.Fatalf("expected bson.M, got %T (%v)", got, got)
	}
	re, ok := doc["$not"].(bson.Regex)
	if !ok {
		t.Fatalf("expected $not to wrap bson.Regex, got %T", doc["$not"])
	}
	if re.Pattern != "^bob$" || re.Options != "i" {
		t.Errorf("inner regex mismatch: %+v", re)
	}
}

func TestTranslateFilterValue_TextMatchList_ExpandsIntoInWithRegexes(t *testing.T) {
	got := translateFilterValue(queries.TextMatchList{
		Values:          []string{"Bob", "Alice"},
		CaseInsensitive: true,
	})
	doc, ok := got.(bson.M)
	if !ok {
		t.Fatalf("expected bson.M, got %T (%v)", got, got)
	}
	arr, ok := doc["$in"].(bson.A)
	if !ok {
		t.Fatalf("expected $in array, got %T (%v)", doc["$in"], doc["$in"])
	}
	if len(arr) != 2 {
		t.Fatalf("expected 2 regex elements, got %d", len(arr))
	}
	for i, want := range []string{"^Bob$", "^Alice$"} {
		re, ok := arr[i].(bson.Regex)
		if !ok {
			t.Fatalf("element %d not a bson.Regex: %T", i, arr[i])
		}
		if re.Pattern != want {
			t.Errorf("element %d pattern mismatch: want %q got %q", i, want, re.Pattern)
		}
		if re.Options != "i" {
			t.Errorf("element %d options mismatch: %q", i, re.Options)
		}
	}
}

func TestApplyFilter_PlainEntriesPassThrough(t *testing.T) {
	dst := bson.M{}
	applyFilter(dst, map[string]any{
		"name":  "Bob",
		"age":   map[string]any{"$gte": 18},
		"email": queries.TextMatchList{Values: []string{"a@x"}, CaseInsensitive: true},
	})
	if dst["name"] != "Bob" {
		t.Errorf("plain scalar: expected 'Bob', got %v", dst["name"])
	}
	if sub, ok := dst["age"].(map[string]any); !ok || sub["$gte"] != 18 {
		t.Errorf("operator sub-document mistranslated: %v", dst["age"])
	}
	if _, ok := dst["email"].(bson.M); !ok {
		t.Errorf("TextMatchList should translate to bson.M, got %T", dst["email"])
	}
	if _, has := dst["$and"]; has {
		t.Errorf("no MultiClause entries: $and must not appear, got %v", dst["$and"])
	}
}

func TestApplyFilter_MultiClauseExpandsIntoAnd(t *testing.T) {
	// Four operators on `name` arrive as a MultiClause. Every clause must
	// land inside a top-level `$and` array as `{name: translated}` — none
	// of them should collide on the same key.
	dst := bson.M{}
	applyFilter(dst, map[string]any{
		"name": queries.MultiClause{Clauses: []any{
			"Bob Smith",
			map[string]any{"$regex": "^Bob"},
			map[string]any{"$regex": "smh", "$options": "i"},
			map[string]any{"$regex": "^bob", "$options": "i"},
		}},
	})
	if _, has := dst["name"]; has {
		t.Errorf("MultiClause must NOT leak as a plain `name` entry, got %v", dst["name"])
	}
	arr, ok := dst["$and"].(bson.A)
	if !ok {
		t.Fatalf("expected $and array, got %T (%v)", dst["$and"], dst["$and"])
	}
	if len(arr) != 4 {
		t.Fatalf("expected 4 $and entries, got %d (%v)", len(arr), arr)
	}
	// Every entry must be `{name: <clause>}` — no other keys, no nesting
	// at the top level.
	for i, entry := range arr {
		m, ok := entry.(bson.M)
		if !ok {
			t.Fatalf("entry %d not bson.M: %T", i, entry)
		}
		if len(m) != 1 {
			t.Errorf("entry %d should carry one key, got %d (%v)", i, len(m), m)
		}
		if _, has := m["name"]; !has {
			t.Errorf("entry %d missing 'name' key: %v", i, m)
		}
	}
	if arr[0].(bson.M)["name"] != "Bob Smith" {
		t.Errorf("entry 0 expected 'Bob Smith', got %v", arr[0].(bson.M)["name"])
	}
}

func TestApplyFilter_MultiClauseWithSentinelInsideTranslates(t *testing.T) {
	// A MultiClause may carry TextMatchList sentinels (e.g. a `.iin`
	// declared alongside another operator). The clause must be routed
	// through translateFilterValue so it reaches Mongo as bson.M{$in: ...},
	// not as the raw struct.
	dst := bson.M{}
	applyFilter(dst, map[string]any{
		"name": queries.MultiClause{Clauses: []any{
			map[string]any{"$regex": "^Bob"},
			queries.TextMatchList{Values: []string{"Bob", "Alice"}, CaseInsensitive: true},
		}},
	})
	arr, _ := dst["$and"].(bson.A)
	if len(arr) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(arr))
	}
	listEntry := arr[1].(bson.M)["name"]
	listDoc, ok := listEntry.(bson.M)
	if !ok {
		t.Fatalf("TextMatchList clause did not translate to bson.M, got %T", listEntry)
	}
	if _, has := listDoc["$in"]; !has {
		t.Errorf("translated TextMatchList missing $in: %v", listDoc)
	}
}

func TestApplyFilter_MixedPlainAndMultiClause(t *testing.T) {
	// Plain entries on other fields must stay as flat `{field: value}`
	// while MultiClause entries land in `$and`. Mongo indexes on the
	// flat fields stay usable; AND semantic is preserved across both.
	dst := bson.M{}
	applyFilter(dst, map[string]any{
		"email": "jane@x",
		"name":  queries.MultiClause{Clauses: []any{"a", "b"}},
	})
	if dst["email"] != "jane@x" {
		t.Errorf("plain field mistranslated, got %v", dst["email"])
	}
	arr, ok := dst["$and"].(bson.A)
	if !ok || len(arr) != 2 {
		t.Fatalf("expected 2-entry $and for MultiClause, got %v", dst["$and"])
	}
}

func TestTranslateFilterValue_TextMatchList_NegateProducesNin(t *testing.T) {
	got := translateFilterValue(queries.TextMatchList{
		Values: []string{"Bob"},
		Negate: true,
	})
	doc, _ := got.(bson.M)
	if _, has := doc["$in"]; has {
		t.Errorf("expected $nin, not $in: %v", doc)
	}
	arr, ok := doc["$nin"].(bson.A)
	if !ok {
		t.Fatalf("expected $nin array, got %T", doc["$nin"])
	}
	re, ok := arr[0].(bson.Regex)
	if !ok || re.Pattern != "^Bob$" {
		t.Errorf("expected bson.Regex with pattern '^Bob$', got %T (%v)", arr[0], arr[0])
	}
	if re.Options != "" {
		t.Errorf("expected empty options when CaseInsensitive=false, got %q", re.Options)
	}
}
