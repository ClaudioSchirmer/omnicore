package responses

import (
	"encoding/json"
	"testing"
)

// These exercise normalizeSlices' pointer-handling branches: a *struct field
// (present → recurse; absent → nil-pointer break) and a []*struct field whose
// elements are pointers (deref + recurse into each, normalizing nested slices).

type innerNorm struct {
	Tags []string `json:"tags"`
}

type ptrStructResponse struct {
	ID    string     `json:"id"`
	Inner *innerNorm `json:"inner,omitempty"`
}

func TestNormalizeSlices_PointerStructPresent_NestedSliceNormalized(t *testing.T) {
	doc := map[string]any{
		"ID":    "u1",
		"Inner": map[string]any{}, // present but no "Tags" → nested nil slice → []
	}
	got := AutoFromDoc[ptrStructResponse](doc)
	if got.Inner == nil {
		t.Fatal("Inner pointer struct should be populated")
	}
	if got.Inner.Tags == nil {
		t.Error("nested slice inside a *struct must be normalized to empty, not nil")
	}
	raw, _ := json.Marshal(got)
	if !contains(string(raw), `"tags":[]`) {
		t.Errorf("wire shape: want tags:[], got %s", raw)
	}
}

func TestNormalizeSlices_PointerStructAbsent_StaysNil(t *testing.T) {
	// Inner absent → the *struct field is a nil pointer; normalizeSlices must
	// hit the nil-pointer break and leave it nil without panicking.
	got := AutoFromDoc[ptrStructResponse](map[string]any{"ID": "u1"})
	if got.Inner != nil {
		t.Errorf("absent *struct must stay nil, got %+v", got.Inner)
	}
}

type sliceOfPtrResponse struct {
	Items []*innerNorm `json:"items"`
}

func TestNormalizeSlices_SliceOfPointerStruct_NormalizesEachElement(t *testing.T) {
	doc := map[string]any{
		"Items": []any{
			map[string]any{},                   // no Tags → becomes []
			map[string]any{"tags": []any{"a"}}, // wire-name key in source doc
		},
	}
	got := AutoFromDoc[sliceOfPtrResponse](doc)
	if len(got.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(got.Items))
	}
	if got.Items[0] == nil || got.Items[0].Tags == nil {
		t.Errorf("first element's nested slice must be normalized to empty: %+v", got.Items[0])
	}
	raw, _ := json.Marshal(got)
	if !contains(string(raw), `"tags":[]`) {
		t.Errorf("wire shape should carry tags:[] for the empty element: %s", raw)
	}
}

func TestNormalizeSlices_SliceOfPointerStruct_NilWhenAbsentNormalizedToEmpty(t *testing.T) {
	got := AutoFromDoc[sliceOfPtrResponse](map[string]any{})
	if got.Items == nil {
		t.Error("absent []*struct must be normalized to an empty slice")
	}
	if len(got.Items) != 0 {
		t.Errorf("expected empty slice, got len=%d", len(got.Items))
	}
}
