package responses

import (
	"encoding/json"
	"testing"
)

// These exercise normalizeSlices' pointer-handling branches: a *struct field
// (present → recurse; nil-pointer break) and a []*struct field whose elements
// are pointers (deref + recurse into each, normalizing nested slices).

type innerNormResult struct {
	Tags []string
}

type ptrStructResult struct {
	ID    string
	Inner *innerNormResult
}

type innerNorm struct {
	Tags []string `json:"tags"`
}

type ptrStructResponse struct {
	Auto
	ID    string     `json:"id"`
	Inner *innerNorm `json:"inner,omitempty"`
}

func TestNormalizeSlices_PointerStructPresent_NestedSliceNormalized(t *testing.T) {
	// Inner present but its Tags slice nil → nested nil slice → []
	got := AutoFromResult[ptrStructResponse](ptrStructResult{ID: "u1", Inner: &innerNormResult{}})
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

func TestNormalizeSlices_PointerStructNil_StaysNil(t *testing.T) {
	// Inner nil → the *struct field stays a nil pointer; normalizeSlices must
	// hit the nil-pointer break and leave it nil without panicking.
	got := AutoFromResult[ptrStructResponse](ptrStructResult{ID: "u1"})
	if got.Inner != nil {
		t.Errorf("nil *struct must stay nil, got %+v", got.Inner)
	}
}

type sliceOfPtrResult struct {
	Items []*innerNormResult
}

type sliceOfPtrResponse struct {
	Auto
	Items []*innerNorm `json:"items"`
}

func TestNormalizeSlices_SliceOfPointerStruct_NormalizesEachElement(t *testing.T) {
	got := AutoFromResult[sliceOfPtrResponse](sliceOfPtrResult{Items: []*innerNormResult{
		{},                    // nil Tags → becomes []
		{Tags: []string{"a"}}, // populated
	}})
	if len(got.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(got.Items))
	}
	if got.Items[0] == nil || got.Items[0].Tags == nil {
		t.Errorf("first element's nested slice must be normalized to empty: %+v", got.Items[0])
	}
	if got.Items[1] == nil || len(got.Items[1].Tags) != 1 || got.Items[1].Tags[0] != "a" {
		t.Errorf("second element's populated slice must survive: %+v", got.Items[1])
	}
	raw, _ := json.Marshal(got)
	if !contains(string(raw), `"tags":[]`) {
		t.Errorf("wire shape should carry tags:[] for the empty element: %s", raw)
	}
}

func TestNormalizeSlices_SliceOfPointerStruct_NilSliceNormalizedToEmpty(t *testing.T) {
	got := AutoFromResult[sliceOfPtrResponse](sliceOfPtrResult{})
	if got.Items == nil {
		t.Error("nil []*struct must be normalized to an empty slice")
	}
	if len(got.Items) != 0 {
		t.Errorf("expected empty slice, got len=%d", len(got.Items))
	}
}
