package responses

import (
	"encoding/json"
	"reflect"
	"testing"
)

// planFor degenerate inputs: nil type, non-struct, pointer-to-struct deref.
func TestPlanFor_NilAndNonStruct(t *testing.T) {
	if planFor(nil) != nil {
		t.Error("planFor(nil) must be nil")
	}
	if planFor(reflect.TypeOf(0)) != nil {
		t.Error("planFor(int) must be nil for a non-struct kind")
	}
	if planFor(reflect.TypeOf("")) != nil {
		t.Error("planFor(string) must be nil for a non-struct kind")
	}
	type s struct {
		A string `json:"a"`
	}
	if planFor(reflect.TypeOf(&s{})) == nil {
		t.Error("planFor(*struct) must deref the pointer and build a plan")
	}
}

// wireName: json:"-" skips; a leading-comma tag (",omitempty") leaves the name
// empty after the cut, so it falls back to the Go field name.
type wireNameResponse struct {
	Skip   string `json:"-"`
	OmitEm string `json:",omitempty"`
	Named  string `json:"named"`
	NoTag  string
}

func TestWireName_SkipAndEmptyNameFallback(t *testing.T) {
	rt := reflect.TypeOf(wireNameResponse{})

	name, skip := wireName(rt.Field(0)) // json:"-"
	if !skip {
		t.Errorf("json:\"-\" must skip, got name=%q skip=%v", name, skip)
	}
	name, skip = wireName(rt.Field(1)) // json:",omitempty"
	if skip || name != "OmitEm" {
		t.Errorf("leading-comma tag should fall back to field name, got %q skip=%v", name, skip)
	}
	name, skip = wireName(rt.Field(2)) // json:"named"
	if skip || name != "named" {
		t.Errorf("explicit tag name, got %q skip=%v", name, skip)
	}
	name, skip = wireName(rt.Field(3)) // no tag
	if skip || name != "NoTag" {
		t.Errorf("absent tag should fall back to field name, got %q skip=%v", name, skip)
	}
}

// asMap branches: plain map[string]any (fast path), a named string-keyed map
// (reflect path; namedMap is declared in auto_from_doc_test.go), a
// non-string-keyed map (rejected), and a non-map (rejected).
func TestAsMap_Branches(t *testing.T) {
	if m, ok := asMap(map[string]any{"x": 1}); !ok || m["x"] != 1 {
		t.Error("plain map[string]any fast path failed")
	}
	if m, ok := asMap(namedMap{"y": 2}); !ok || m["y"] != 2 {
		t.Error("named string-keyed map should normalize via reflection")
	}
	if _, ok := asMap(map[int]any{1: "a"}); ok {
		t.Error("non-string-keyed map must be rejected")
	}
	if _, ok := asMap("not-a-map"); ok {
		t.Error("non-map value must be rejected")
	}
	if _, ok := asMap(nil); ok {
		t.Error("nil must be rejected")
	}
}

// asSliceOfMaps branches: non-slice rejected; slice whose element is not a map
// rejected (returns false so the caller copies verbatim); good slice accepted.
func TestAsSliceOfMaps_Branches(t *testing.T) {
	if _, ok := asSliceOfMaps("not-a-slice"); ok {
		t.Error("non-slice must be rejected")
	}
	if _, ok := asSliceOfMaps([]string{"a", "b"}); ok {
		t.Error("slice of non-maps must be rejected")
	}
	out, ok := asSliceOfMaps([]any{map[string]any{"a": 1}, namedMap{"b": 2}})
	if !ok || len(out) != 2 {
		t.Fatalf("slice of map-likes should normalize, got ok=%v out=%+v", ok, out)
	}
}

// A scalar slice field with a present value exercises remapValue's pass-through
// and normalizeSlices' fkSlice non-nil branch (no reset when already populated).
type scalarSliceResponse struct {
	Tags []string `json:"tags"`
}

func TestAutoFromDoc_ScalarSlicePresentPassesThrough(t *testing.T) {
	got := AutoFromDoc[scalarSliceResponse](map[string]any{"Tags": []any{"a", "b"}})
	if len(got.Tags) != 2 || got.Tags[0] != "a" {
		t.Fatalf("scalar slice should pass through, got %+v", got.Tags)
	}
	raw, _ := json.Marshal(got)
	if !contains(string(raw), `"tags":["a","b"]`) {
		t.Errorf("wire: %s", raw)
	}
}

// buildPlan must deref an anonymous pointer-to-struct embed and promote its
// fields, mirroring encoding/json (covers the pointer-deref loop in the
// f.Anonymous branch). The doc populates the promoted field so json allocates
// the embed pointer before normalizeSlices walks the promoted index path.
type embedWithNote struct {
	Note string `json:"note"`
}

type ptrEmbedResponse struct {
	*embedWithNote
	ID string `json:"id"`
}

func TestPlanFor_AnonymousPointerEmbed_FieldsPromoted(t *testing.T) {
	plan := planFor(reflect.TypeOf(ptrEmbedResponse{}))
	if plan == nil {
		t.Fatal("plan must be built for a struct R")
	}
	var promoted bool
	for _, f := range plan.fields {
		if f.sourceKey == "Note" && f.destKey == "note" {
			promoted = true
		}
	}
	if !promoted {
		t.Errorf("anonymous *struct embed fields must be promoted, got %+v", plan.fields)
	}
}

// buildPlan must skip an unexported, non-anonymous field (the IsExported guard).
type unexportedFieldResponse struct {
	Name   string `json:"name"`
	secret string //nolint:unused // exercised via reflection (must be skipped)
}

func TestBuildPlan_SkipsUnexportedNonAnonymousField(t *testing.T) {
	got := AutoFromDoc[unexportedFieldResponse](map[string]any{"Name": "n", "secret": "s"})
	if got.Name != "n" {
		t.Errorf("Name: want n, got %q", got.Name)
	}
	if got.secret != "" {
		t.Errorf("unexported field must not be projected, got %q", got.secret)
	}
}

// A non-struct R yields a nil plan, so normalizeSlices returns at its plan==nil
// guard and the JSON round-trip is a best-effort no-op.
func TestAutoFromDoc_NonStructType_NilPlanNoOp(t *testing.T) {
	if got := AutoFromDoc[int](map[string]any{"x": 1}); got != 0 {
		t.Errorf("non-struct R must yield the zero value, got %d", got)
	}
}

// normalizeSlices returns early on an invalid reflect.Value (the !v.IsValid()
// guard) — called directly with a non-nil plan to isolate the branch.
func TestNormalizeSlices_InvalidValueReturns(t *testing.T) {
	plan := planFor(reflect.TypeOf(scalarSliceResponse{}))
	// Must not panic on an invalid Value.
	normalizeSlices(reflect.Value{}, plan)
}

// A []*struct with a nil pointer element exercises the nil-pointer break inside
// normalizeSlices' fkSliceOfStruct deref loop. The source doc carries a null
// element which json unmarshals to a nil *innerNorm.
func TestNormalizeSlices_SliceOfPointerStruct_NilElementSkipped(t *testing.T) {
	got := AutoFromDoc[sliceOfPtrResponse](map[string]any{"Items": []any{nil}})
	if len(got.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(got.Items))
	}
	if got.Items[0] != nil {
		t.Errorf("expected nil element to survive as nil, got %+v", got.Items[0])
	}
}

// A nested struct field whose source value is NOT a map exercises remapValue's
// fkStruct fall-through (asMap returns false → value passes verbatim).
type wrapperResponse struct {
	Inner innerNorm `json:"inner"`
}

func TestAutoFromDoc_StructSourceNotAMapPassesThrough(t *testing.T) {
	// "Inner" carries a non-map value; remapValue falls through and json drops it.
	got := AutoFromDoc[wrapperResponse](map[string]any{"Inner": "scalar"})
	if got.Inner.Tags == nil {
		t.Error("nested slice should still normalize to empty even when source was malformed")
	}
}
