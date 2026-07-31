package queries

import (
	"bytes"
	"testing"
)

// TestCanonicalizeFilterValue_AllTypeBranches exercises every type branch of
// canonicalizeFilterValue directly, asserting the exact deterministic byte
// stream each shape produces. The stream is internal to HashContext and not a
// wire format, but pinning it here guards against an accidental change that
// would silently invalidate every previously-issued cursor.
func TestCanonicalizeFilterValue_AllTypeBranches(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"nil", nil, "n"},
		{"bool-true", true, "b:true"},
		{"bool-false", false, "b:false"},
		{"string", "ab", "s:2:ab"},
		{"empty-string", "", "s:0:"},
		{"int", 5, "i:5"},
		{"int-negative", -7, "i:-7"},
		{"int32", int32(5), "i:5"},
		{"int64", int64(9), "i:9"},
		{"uint", uint(5), "u:5"},
		{"uint64", uint64(42), "u:42"},
		{"float32", float32(1.5), "f:1.5"},
		{"float64", float64(1.25), "f:1.25"},
		{"slice-string", []string{"x", "yy"}, "ss:2[1:x,2:yy,]"},
		{"slice-any", []any{1, "a"}, "a:2[i:1,s:1:a,]"},
		{"map", map[string]any{"k": 1}, "m:1{1:k=i:1,}"},
		{"multiclause", MultiClause{Clauses: []any{1}}, "MC[i:1,]"},
		{"textmatch", TextMatch{Value: "Bob", Kind: TextPrefix, CaseInsensitive: true, Negate: false}, "TM:0:true:false:3:Bob"},
		{"textmatchlist", TextMatchList{Values: []string{"a"}, CaseInsensitive: false, Negate: true}, "TML:false:true:1[1:a,]"},
		{"default-unknown-type", int8(3), "?int8:3"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			canonicalizeFilterValue(&buf, tc.in)
			if got := buf.String(); got != tc.want {
				t.Fatalf("canonicalizeFilterValue(%#v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestCanonicalizeFilterMap_SortsKeysDeterministically asserts the map branch
// emits keys in sorted order regardless of insertion order, with the length
// prefix on both the map and each key (the anti-collision guard).
func TestCanonicalizeFilterMap_SortsKeysDeterministically(t *testing.T) {
	var a, b bytes.Buffer
	canonicalizeFilterMap(&a, map[string]any{"name": "x", "age": 1})
	canonicalizeFilterMap(&b, map[string]any{"age": 1, "name": "x"})
	if a.String() != b.String() {
		t.Fatalf("map canonicalization not order-stable: %q vs %q", a.String(), b.String())
	}
	want := "m:2{3:age=i:1,4:name=s:1:x,}"
	if a.String() != want {
		t.Fatalf("map canonicalization = %q, want %q", a.String(), want)
	}
}

// TestCanonicalizeFilterValue_NestedComposites covers recursion through a map
// holding a slice holding a nested map — every composite branch in one pass.
func TestCanonicalizeFilterValue_NestedComposites(t *testing.T) {
	var buf bytes.Buffer
	canonicalizeFilterValue(&buf, map[string]any{
		"items": []any{map[string]any{"id": 2}},
	})
	want := "m:1{5:items=a:1[m:1{2:id=i:2,},],}"
	if got := buf.String(); got != want {
		t.Fatalf("nested composite = %q, want %q", got, want)
	}
}

// TestHashContext_TextMatchAxesDistinct confirms the text sentinels feed
// HashContext distinctly — two filters differing only in a TextMatch flag must
// produce different context hashes (so a cursor cannot survive an operator flip
// mid-navigation).
func TestHashContext_TextMatchAxesDistinct(t *testing.T) {
	h1 := HashContext(map[string]any{"name": TextMatch{Value: "Bob", CaseInsensitive: false}}, nil, "", false)
	h2 := HashContext(map[string]any{"name": TextMatch{Value: "Bob", CaseInsensitive: true}}, nil, "", false)
	if h1 == h2 {
		t.Fatal("TextMatch CaseInsensitive flip must alter the context hash")
	}
	l1 := HashContext(map[string]any{"name": TextMatchList{Values: []string{"a"}}}, nil, "", false)
	l2 := HashContext(map[string]any{"name": TextMatchList{Values: []string{"b"}}}, nil, "", false)
	if l1 == l2 {
		t.Fatal("TextMatchList value change must alter the context hash")
	}
}
