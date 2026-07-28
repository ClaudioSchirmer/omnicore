package mongo

import "testing"

// Sorting / keyset pagination over a MATERIALIZED view segment.
//
// A JoinView segment is ordinary document content, so a 1:1 segment field is a
// first-class sort key: the cursor value is extractable and the auto-included
// column is strippable, exactly like any root field. A field inside a 1:N
// segment is NOT: an array has no single value, so there is no keyset cursor to
// build from it — the framework's boundary, identical to a native child
// collection's, and the reason these two walkers stop at an array.

func TestSortKey_OneToOneViewSegmentIsAFirstClassPath(t *testing.T) {
	doc := map[string]any{
		"_id":     "s1",
		"product": map[string]any{"name": "Cable", "price": 10},
	}
	if got := lookupDocPath(doc, "product.name"); got != "Cable" {
		t.Fatalf("a 1:1 segment field must yield its cursor value, got %v", got)
	}
	deleteDocPath(doc, "product.name")
	seg := doc["product"].(map[string]any)
	if _, still := seg["name"]; still {
		t.Fatalf("the auto-included sort column must be stripped from the segment: %v", seg)
	}
	if seg["price"] != 10 {
		t.Fatalf("sibling fields inside the segment must survive: %v", seg)
	}
}

func TestSortKey_InsideOneToManyViewSegmentIsUnsupported(t *testing.T) {
	doc := map[string]any{
		"_id":   "c1",
		"sales": []any{map[string]any{"total": 10}, map[string]any{"total": 20}},
	}
	// No single value to key a cursor on — the lookup yields nothing rather than
	// inventing an element, and the strip leaves the array untouched.
	if got := lookupDocPath(doc, "sales.total"); got != nil {
		t.Fatalf("a path inside a 1:N segment must not resolve to a cursor value, got %v", got)
	}
	deleteDocPath(doc, "sales.total")
	if len(doc["sales"].([]any)) != 2 {
		t.Fatalf("the array must survive untouched: %v", doc["sales"])
	}
}
