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

// ─── WalkRequest: pointer deref / non-struct / untagged-field skip / embed ───

func TestWalkRequest_NonStructIsEmpty(t *testing.T) {
	if fields := WalkRequest(reflect.TypeOf(0)); len(fields) != 0 {
		t.Fatalf("non-struct must yield no fields, got %v", fields)
	}
}

func TestWalkRequest_PointerDerefAndUntaggedSkip(t *testing.T) {
	type inner struct {
		Name  *string `query:"name" filter:"eq"`
		Other string  // no query tag → skipped
		Limit *int64  `query:"limit"` // reserved scalar
	}
	fields := WalkRequest(reflect.PointerTo(reflect.TypeOf(inner{})))
	if len(fields) != 2 {
		t.Fatalf("expected 2 query-tagged fields (name, limit), got %d: %+v", len(fields), fields)
	}
	if fields[0].WirePath != "name" || fields[0].Ops == nil || !fields[0].TopLevel {
		t.Errorf("name leaf = %+v, want filter leaf at top level", fields[0])
	}
	if fields[1].WirePath != "limit" || fields[1].Ops != nil || fields[1].Group {
		t.Errorf("limit leaf = %+v, want reserved scalar", fields[1])
	}
}

func TestWalkRequest_EmbedGroupMarkerThenInnerLeaves(t *testing.T) {
	fields := WalkRequest(reflect.TypeOf(reqNestedEmbedRequest{}))
	// Declaration order: name (leaf), addresses (group marker), addresses.zipCode,
	// addresses.city (inner leaves), limit (reserved).
	var sawGroup, sawInner bool
	for _, f := range fields {
		if f.WirePath == "addresses" {
			if !f.Group {
				t.Errorf("addresses must be an embed-group marker, got %+v", f)
			}
			sawGroup = true
		}
		if f.WirePath == "addresses.zipCode" {
			if f.GoPath != "Addresses.ZipCode" || f.Ops == nil {
				t.Errorf("addresses.zipCode inner leaf = %+v", f)
			}
			sawInner = true
		}
	}
	if !sawGroup || !sawInner {
		t.Fatalf("expected both the group marker and an inner leaf, got %+v", fields)
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
