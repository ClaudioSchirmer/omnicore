package queryschema

import (
	"reflect"
	"strings"
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
	First     *int64       `query:"first"`
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
	if !s.Reserved["first"] {
		t.Errorf("first must be a reserved key, reserved=%v", s.Reserved)
	}
}

// ─── ExtractRequestSchema: the closed control vocabulary (boot guard) ────────

type reqAllControlsRequest struct {
	First           *int64  `query:"first"`
	Last            *int64  `query:"last"`
	After           *string `query:"after"`
	Before          *string `query:"before"`
	OrderBy         *string `query:"orderBy"`
	Fields          *string `query:"fields"`
	Search          *string `query:"search"`
	IncludeArchived *bool   `query:"includeArchived"`
	OnlyTotal       *bool   `query:"onlyTotal"`
}

func TestExtractRequestSchema_AllCanonicalControlsAccepted(t *testing.T) {
	s := ExtractRequestSchema(reflect.TypeOf(reqAllControlsRequest{}))
	for key := range ControlKeys {
		if !s.Reserved[key] {
			t.Errorf("canonical key %q must land in Reserved, got %v", key, s.Reserved)
		}
	}
}

type reqStaleVocabularyRequest struct {
	Limit *int64 `query:"limit"` // not a canonical control — must boot-fail
}

func TestExtractRequestSchema_NonCanonicalControlPanics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("a non-canonical top-level control scalar must panic at extraction")
		}
		msg, ok := r.(string)
		if !ok || !strings.Contains(msg, `query:"limit"`) || !strings.Contains(msg, KeyOrderBy) {
			t.Fatalf("panic must name the offending tag and the canonical vocabulary, got %v", r)
		}
	}()
	ExtractRequestSchema(reflect.TypeOf(reqStaleVocabularyRequest{}))
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
		First *int64  `query:"first"` // reserved scalar
	}
	fields := WalkRequest(reflect.PointerTo(reflect.TypeOf(inner{})))
	if len(fields) != 2 {
		t.Fatalf("expected 2 query-tagged fields (name, first), got %d: %+v", len(fields), fields)
	}
	if fields[0].WirePath != "name" || fields[0].Ops == nil || !fields[0].TopLevel {
		t.Errorf("name leaf = %+v, want filter leaf at top level", fields[0])
	}
	if fields[1].WirePath != "first" || fields[1].Ops != nil || fields[1].Group {
		t.Errorf("first leaf = %+v, want reserved scalar", fields[1])
	}
}

func TestWalkRequest_EmbedGroupMarkerThenInnerLeaves(t *testing.T) {
	fields := WalkRequest(reflect.TypeOf(reqNestedEmbedRequest{}))
	// Declaration order: name (leaf), addresses (group marker), addresses.zipCode,
	// addresses.city (inner leaves), first (reserved).
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

// ─── ReadIncludeArchived ─────────────────────────────────────────────────────

type iaValueDTO struct {
	IncludeArchived bool `query:"includeArchived"`
}

type iaPointerDTO struct {
	IncludeArchived *bool `query:"includeArchived"`
}

type iaEmbeddedDTO struct {
	iaPointerDTO
	Name *string `query:"name" filter:"eq"`
}

type iaAbsentDTO struct {
	Name *string `query:"name" filter:"eq"`
}

type iaWrongKindDTO struct {
	IncludeArchived string `query:"includeArchived"`
}

// iaEmbeddedEmptyDTO embeds a struct that declares the control but leaves it
// unset — the walk must keep scanning the outer fields instead of stopping.
type iaEmbeddedEmptyDTO struct {
	iaPointerDTO
	Other *string `query:"other" filter:"eq"`
}

// iaUnexportedDTO carries an unexported field before the control — reflect
// cannot read it, so the walk skips it and finds the declared one.
type iaUnexportedDTO struct {
	hidden          bool
	IncludeArchived bool `query:"includeArchived"`
}

func TestReadIncludeArchived(t *testing.T) {
	yes := true
	no := false
	cases := []struct {
		name string
		dto  any
		want bool
	}{
		{"bool true", iaValueDTO{IncludeArchived: true}, true},
		{"bool false", iaValueDTO{}, false},
		{"pointer true", iaPointerDTO{IncludeArchived: &yes}, true},
		{"pointer false", iaPointerDTO{IncludeArchived: &no}, false},
		{"pointer nil", iaPointerDTO{}, false},
		{"promoted anonymous", iaEmbeddedDTO{iaPointerDTO: iaPointerDTO{IncludeArchived: &yes}}, true},
		{"field absent", iaAbsentDTO{}, false},
		{"non-bool field", iaWrongKindDTO{IncludeArchived: "true"}, false},
		{"addressable pointer", &iaValueDTO{IncludeArchived: true}, true},
		{"nil pointer DTO", (*iaValueDTO)(nil), false},
		{"not a struct", 42, false},
		{"embedded control unset", iaEmbeddedEmptyDTO{}, false},
		{"unexported field skipped", iaUnexportedDTO{hidden: true, IncludeArchived: true}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ReadIncludeArchived(reflect.ValueOf(tc.dto)); got != tc.want {
				t.Errorf("ReadIncludeArchived(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}
