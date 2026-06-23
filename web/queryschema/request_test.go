package queryschema

import (
	"reflect"
	"testing"
)

// ─── ExtractRequestSchema: pointer type + nested embed group ─────────────────

type reqEmbedLeaf struct {
	ZipCode *string `query:"zipCode" filter:"eq"`
	City    *string `query:"city" filter:"eq"`
}

type reqNestedEmbedRequest struct {
	Name      *string      `query:"name" filter:"eq"`
	Addresses reqEmbedLeaf `query:"addresses"` // embed group — no filter tag
	Limit     *int64       `query:"limit"`
}

func TestExtractRequestSchema_PointerTypeAndNestedEmbed(t *testing.T) {
	s := ExtractRequestSchema(reflect.PointerTo(reflect.TypeOf(reqNestedEmbedRequest{})))
	if _, ok := s.Filters["name"]; !ok {
		t.Errorf("expected top-level name filter, got %v", s.Filters)
	}
	spec, ok := s.Filters["addresses.zipCode"]
	if !ok {
		t.Fatalf("expected nested embed filter addresses.zipCode, got %v", s.Filters)
	}
	if spec.DocPath != "Addresses.ZipCode" {
		t.Errorf("nested embed DocPath = %q, want Addresses.ZipCode", spec.DocPath)
	}
	if !s.Reserved["limit"] {
		t.Errorf("limit must be a reserved key, reserved=%v", s.Reserved)
	}
}

func TestExtractRequestSchema_CachedByReflectType(t *testing.T) {
	s1 := ExtractRequestSchema(reflect.TypeOf(reqNestedEmbedRequest{}))
	s2 := ExtractRequestSchema(reflect.TypeOf(reqNestedEmbedRequest{}))
	if s1 != s2 {
		t.Errorf("expected the same *RequestSchema pointer on the second call (cache hit)")
	}
	// The cached schema carries the dotted Go field paths for nested leaves.
	if got := s1.Filters["addresses.city"].DocPath; got != "Addresses.City" {
		t.Errorf("addresses.city DocPath = %q, want Addresses.City", got)
	}
	if got := s1.Filters["name"].DocPath; got != "Name" {
		t.Errorf("name DocPath = %q, want Name", got)
	}
}

// ─── ExtractRequestSchema: the full partial-operator set on one leaf ─────────

type reqPartialRequest struct {
	Name *string `query:"name" filter:"eq,startswith,contains,ieq,ine,iin,inin,istartswith,icontains"`
}

func TestExtractRequestSchema_IncludesPartialOperators(t *testing.T) {
	s := ExtractRequestSchema(reflect.TypeOf(reqPartialRequest{}))
	for _, op := range []string{OpStartsWith, OpContains, OpIEq, OpINe, OpIIn, OpINin, OpIStartsWith, OpIContains} {
		if !s.Filters["name"].Ops[op] {
			t.Errorf("expected Filters[name].Ops[%q]=true", op)
		}
	}
}

// ─── walkSchemaLevel: pointer deref / non-struct / untagged field ────────────

func TestWalkSchemaLevel_PointerNonStructAndUntaggedField(t *testing.T) {
	s := &RequestSchema{Filters: map[string]FilterSpec{}, Reserved: map[string]bool{}}

	// Non-struct type → early return, nothing accumulated.
	walkSchemaLevel(reflect.TypeOf(0), "", "", s, true)
	if len(s.Filters) != 0 || len(s.Reserved) != 0 {
		t.Fatalf("non-struct must accumulate nothing, got filters=%v reserved=%v", s.Filters, s.Reserved)
	}

	// Pointer-to-struct → deref + walk; the untagged Other field is skipped.
	type inner struct {
		Name  *string `query:"name" filter:"eq"`
		Other string  // no query tag → skipped
	}
	walkSchemaLevel(reflect.PointerTo(reflect.TypeOf(inner{})), "", "", s, true)
	if _, ok := s.Filters["name"]; !ok {
		t.Errorf("pointer-to-struct must be deref'd and walked: %v", s.Filters)
	}
}

// ─── joinPath ────────────────────────────────────────────────────────────────

func TestJoinPath_EmptySegmentReturnsPrefix(t *testing.T) {
	if got := joinPath("a", ""); got != "a" {
		t.Errorf("joinPath(a,\"\") = %q, want a", got)
	}
	if got := joinPath("", "b"); got != "b" {
		t.Errorf("joinPath(\"\",b) = %q, want b", got)
	}
	if got := joinPath("a", "b"); got != "a.b" {
		t.Errorf("joinPath(a,b) = %q, want a.b", got)
	}
}
