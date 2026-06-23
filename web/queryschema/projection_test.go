package queryschema

import (
	"reflect"
	"strings"
	"testing"
)

// ─── fixtures ────────────────────────────────────────────────────────────────

type sparseAddress struct {
	ID      *string `json:"id,omitempty"`
	City    *string `json:"city,omitempty"`
	ZipCode *string `json:"zipCode,omitempty"`
	State   *string `json:"state,omitempty"`
}

type sparseUser struct {
	ID        *string         `json:"id,omitempty"`
	Name      *string         `json:"name,omitempty"`
	Email     *string         `json:"email,omitempty"`
	Phone     *string         `json:"phone,omitempty"`
	Addresses []sparseAddress `json:"addresses,omitempty"`
}

// ─── ExtractProjectionSchema + walkProjectionLevel ──────────────────────────

func TestExtractProjectionSchema_TopLevelAndNestedPaths(t *testing.T) {
	s := ExtractProjectionSchema(reflect.TypeOf(sparseUser{}))
	cases := map[string]string{
		"id":                "ID",
		"name":              "Name",
		"addresses":         "Addresses",
		"addresses.id":      "Addresses.ID",
		"addresses.city":    "Addresses.City",
		"addresses.zipCode": "Addresses.ZipCode",
		"addresses.state":   "Addresses.State",
	}
	for wire, want := range cases {
		if got := s.Paths[wire]; got != want {
			t.Errorf("%s → %q, want %q", wire, got, want)
		}
	}
}

func TestExtractProjectionSchema_CachedByReflectType(t *testing.T) {
	s1 := ExtractProjectionSchema(reflect.TypeOf(sparseUser{}))
	s2 := ExtractProjectionSchema(reflect.TypeOf(sparseUser{}))
	if s1 != s2 {
		t.Errorf("expected the same *ProjectionSchema pointer on the second call (cache hit)")
	}
}

type projChild struct {
	City *string `json:"city,omitempty"`
}

type projEmbed struct {
	Embedded *string `json:"embedded,omitempty"`
}

type projResp struct {
	*projEmbed              // anonymous pointer-to-struct → promoted
	Name      *string       `json:"name,omitempty"`
	Hidden    *string       `json:"-"` // skipped
	NoTag     *string       // empty json tag → falls back to field name
	Self      *projChild    `json:"self,omitempty"`
	Lines     []*projChild  `json:"lines,omitempty"`
}

func TestExtractProjectionSchema_PointerAndNested(t *testing.T) {
	s := ExtractProjectionSchema(reflect.PointerTo(reflect.TypeOf(projResp{})))
	for _, wire := range []string{"name", "embedded", "NoTag", "self", "self.city", "lines", "lines.city"} {
		if _, ok := s.Paths[wire]; !ok {
			t.Errorf("expected wire path %q in %v", wire, s.Paths)
		}
	}
	if _, ok := s.Paths["Hidden"]; ok {
		t.Error("json:\"-\" field must be skipped")
	}
}

func TestExtractProjectionSchema_NonStructIsEmpty(t *testing.T) {
	s := ExtractProjectionSchema(reflect.TypeOf(0))
	if len(s.Paths) != 0 {
		t.Errorf("non-struct must yield empty schema, got %v", s.Paths)
	}
}

type embeddedBase struct {
	ID   *string `json:"id,omitempty"`
	memo string  // unexported — skipped by both walks
}

type innerPtr struct {
	City *string `json:"city,omitempty"`
}

type withEmbedAndPtrStruct struct {
	embeddedBase           // anonymous struct embed — promoted
	Name         *string   `json:"name,omitempty"`
	Profile      *innerPtr `json:"profile,omitempty"`
	Tags         *string   `json:",omitempty"` // empty wire name → falls back to Go field name "Tags"
}

func TestExtractProjectionSchema_EmbedUnexportedPtrStruct(t *testing.T) {
	_ = embeddedBase{}.memo // reference so the unexported field is not flagged unused
	s := ExtractProjectionSchema(reflect.TypeOf(withEmbedAndPtrStruct{}))
	if got := s.Paths["id"]; got != "ID" {
		t.Errorf("promoted id → %q, want ID", got)
	}
	if got := s.Paths["profile.city"]; got != "Profile.City" {
		t.Errorf("profile.city → %q, want Profile.City", got)
	}
	if got := s.Paths["Tags"]; got != "Tags" {
		t.Errorf("Tags (empty json name fallback) → %q, want Tags", got)
	}
	if _, ok := s.Paths["memo"]; ok {
		t.Error("unexported field must not appear in projection schema")
	}
}

func TestWalkProjectionLevel_PointerAndNonStructDefensive(t *testing.T) {
	s := &ProjectionSchema{Paths: map[string]string{}}
	type leaf struct {
		Name *string `json:"name,omitempty"`
	}
	// Pointer top type exercises the deref loop; the underlying struct still
	// contributes its leaf path.
	walkProjectionLevel(reflect.PointerTo(reflect.TypeOf(leaf{})), "", "", s)
	if _, ok := s.Paths["name"]; !ok {
		t.Fatalf("expected name path after pointer deref, got %v", s.Paths)
	}
	// Non-struct top type returns without panicking and adds nothing.
	before := len(s.Paths)
	walkProjectionLevel(reflect.TypeOf(0), "", "", s)
	if len(s.Paths) != before {
		t.Fatalf("non-struct walk must add nothing, paths changed: %v", s.Paths)
	}
}

// ─── ValidateFieldsResponse + walkResponseGuard + FormatFieldsResponseGuard ──

func TestValidateFieldsResponse_AcceptsPointerWithOmitempty(t *testing.T) {
	if errs := ValidateFieldsResponse(reflect.TypeOf(sparseUser{})); len(errs) != 0 {
		t.Errorf("expected no violations, got %v", errs)
	}
}

type guardMissingOmitempty struct {
	Name *string `json:"name"`
}

func TestValidateFieldsResponse_RejectsMissingOmitempty(t *testing.T) {
	errs := ValidateFieldsResponse(reflect.TypeOf(guardMissingOmitempty{}))
	if len(errs) == 0 {
		t.Fatal("expected violations for missing ,omitempty")
	}
	joined := strings.Join(errs, "\n")
	if !strings.Contains(joined, "name") || !strings.Contains(joined, "omitempty") {
		t.Errorf("expected diagnostic to mention name + omitempty, got: %s", joined)
	}
}

type guardNonPointerScalar struct {
	Name string `json:"name,omitempty"`
}

func TestValidateFieldsResponse_RejectsNonPointerScalar(t *testing.T) {
	errs := ValidateFieldsResponse(reflect.TypeOf(guardNonPointerScalar{}))
	if len(errs) == 0 {
		t.Fatal("expected violations for non-pointer scalar field")
	}
	joined := strings.Join(errs, "\n")
	if !strings.Contains(joined, "name") || !strings.Contains(joined, "must be") {
		t.Errorf("expected diagnostic to demand pointer for name, got: %s", joined)
	}
}

type guardNestedItemA struct {
	Label string `json:"label,omitempty"` // violation — non-pointer scalar at depth 2
}

type guardNestedBad struct {
	ID *string            `json:"id,omitempty"`
	A  []guardNestedItemA `json:"a,omitempty"`
}

func TestValidateFieldsResponse_RecursesIntoSliceOfStruct(t *testing.T) {
	errs := ValidateFieldsResponse(reflect.TypeOf(guardNestedBad{}))
	if len(errs) == 0 {
		t.Fatal("expected nested violation on a[].label")
	}
	if joined := strings.Join(errs, "\n"); !strings.Contains(joined, "a.label") {
		t.Errorf("expected diagnostic to mention path a.label, got: %s", joined)
	}
}

func TestValidateFieldsResponse_JSONHyphenSkipsField(t *testing.T) {
	type withSkip struct {
		Name   *string `json:"name,omitempty"`
		Hidden string  `json:"-"`
	}
	if errs := ValidateFieldsResponse(reflect.TypeOf(withSkip{})); len(errs) != 0 {
		t.Errorf("expected no violations (json:- skipped), got %v", errs)
	}
}

type guardWithMap struct {
	ID      *string           `json:"id,omitempty"`
	Meta    map[string]string `json:"meta,omitempty"`
	Profile *innerGuard       `json:"profile,omitempty"`
}

type innerGuard struct {
	City *string `json:"city,omitempty"`
}

func TestValidateFieldsResponse_MapAndPtrStructAccepted(t *testing.T) {
	if errs := ValidateFieldsResponse(reflect.TypeOf(guardWithMap{})); len(errs) != 0 {
		t.Errorf("expected no violations (map tolerated, ptr-struct recurses cleanly), got %v", errs)
	}
}

type guardEmbedInner struct {
	Bad string `json:"bad,omitempty"` // non-pointer scalar → violation
}

type guardEmbedViolation struct {
	guardEmbedInner
	Name *string `json:"name,omitempty"`
}

func TestValidateFieldsResponse_AnonymousEmbedViolationSurfaces(t *testing.T) {
	if errs := ValidateFieldsResponse(reflect.TypeOf(guardEmbedViolation{})); len(errs) == 0 {
		t.Fatal("expected the embedded non-pointer scalar to surface a violation")
	}
}

type guardEmbedBase struct {
	Name *string `json:"name,omitempty"`
}

type guardAnonPtrEmbed struct {
	*guardEmbedBase
	ID *string `json:"id,omitempty"`
}

func TestValidateFieldsResponse_AnonymousPointerEmbed(t *testing.T) {
	if errs := ValidateFieldsResponse(reflect.TypeOf(guardAnonPtrEmbed{})); len(errs) != 0 {
		t.Errorf("expected no violations for a compliant anonymous *struct embed, got %v", errs)
	}
}

type guardEmbedScalar struct {
	Promoted string `json:"promoted,omitempty"` // non-pointer scalar
}

type invalidGuardChild struct {
	City string `json:"city,omitempty"` // non-pointer scalar → violation
}

type invalidResp struct {
	guardEmbedScalar                        // anonymous struct → recurse
	Scalar           string                 `json:"scalar"`        // missing omitempty + non-pointer
	Hidden           string                 `json:"-"`             // skipped
	Bag              map[string]any         `json:"bag,omitempty"` // map → accepted
	Children         []*invalidGuardChild   `json:"children,omitempty"`
}

func TestValidateFieldsResponse_ReportsViolationsAndFormats(t *testing.T) {
	errs := ValidateFieldsResponse(reflect.TypeOf(invalidResp{}))
	joined := strings.Join(errs, "\n")
	for _, want := range []string{"scalar", "promoted", "children.city"} {
		if !strings.Contains(joined, want) {
			t.Errorf("expected a violation mentioning %q in:\n%s", want, joined)
		}
	}
	msg := FormatFieldsResponseGuard(reflect.TypeOf(invalidResp{}), errs)
	if !strings.Contains(msg, "sparse-render contract") {
		t.Errorf("unexpected guard message: %s", msg)
	}
}

func TestWalkResponseGuard_PointerAndNonStructDefensive(t *testing.T) {
	var errs []string
	walkResponseGuard(reflect.PointerTo(reflect.TypeOf(guardEmbedBase{})), "", &errs)
	walkResponseGuard(reflect.TypeOf(0), "", &errs)
	if len(errs) != 0 {
		t.Fatalf("defensive walks must not report violations, got %v", errs)
	}
}

// ─── ParseSortWithSchema / ParseProjection edge cases ────────────────────────

func TestParseSortWithSchema_EdgeCases(t *testing.T) {
	if fields, bad, ok := ParseSortWithSchema("", nil); !ok || bad != "" || fields != nil {
		t.Fatalf("empty sort = (%v,%q,%v)", fields, bad, ok)
	}
	fields, bad, ok := ParseSortWithSchema("-name,,age", nil)
	if !ok || bad != "" || len(fields) != 2 {
		t.Fatalf("nil-schema sort = (%v,%q,%v)", fields, bad, ok)
	}
	if fields[0].Field != "name" || !fields[0].Desc {
		t.Errorf("expected name desc, got %+v", fields[0])
	}
}

func TestParseSortWithSchema_UnknownTokenWithSchema(t *testing.T) {
	ps := ExtractProjectionSchema(reflect.TypeOf(sparseUser{}))
	if _, bad, ok := ParseSortWithSchema("bogus", ps); ok || bad != "bogus" {
		t.Fatalf("expected unknown sort token rejection, got bad=%q ok=%v", bad, ok)
	}
}

func TestParseSortWithSchema_KnownTokenTranslatesToDocPath(t *testing.T) {
	ps := ExtractProjectionSchema(reflect.TypeOf(sparseUser{}))
	fields, bad, ok := ParseSortWithSchema("-addresses.zipCode", ps)
	if !ok || bad != "" || len(fields) != 1 {
		t.Fatalf("sort = (%v,%q,%v)", fields, bad, ok)
	}
	if fields[0].Field != "Addresses.ZipCode" || !fields[0].Desc {
		t.Errorf("expected Addresses.ZipCode desc, got %+v", fields[0])
	}
}

func TestParseProjection_EmptyAndNilSchema(t *testing.T) {
	if proj, _, bad, ok := ParseProjection("", nil); !ok || bad != "" || proj != nil {
		t.Fatalf("empty projection = (%v,%q,%v)", proj, bad, ok)
	}
	proj, wireSet, bad, ok := ParseProjection("a,,b", nil)
	if !ok || bad != "" || len(proj) != 2 || !wireSet["a"] {
		t.Fatalf("nil-schema projection = (%v,%v,%q,%v)", proj, wireSet, bad, ok)
	}
}

func TestParseProjection_SchemaTranslatesAndRejects(t *testing.T) {
	ps := ExtractProjectionSchema(reflect.TypeOf(sparseUser{}))
	proj, wireSet, bad, ok := ParseProjection("name,addresses.zipCode", ps)
	if !ok || bad != "" {
		t.Fatalf("expected ok, got bad=%q ok=%v", bad, ok)
	}
	if proj["Name"] != 1 || proj["Addresses.ZipCode"] != 1 {
		t.Errorf("expected translated Go paths, got %v", proj)
	}
	if !wireSet["name"] {
		t.Errorf("expected wireSet to record name, got %v", wireSet)
	}
	if _, _, bad, ok := ParseProjection("bogus", ps); ok || bad != "bogus" {
		t.Fatalf("expected unknown token rejection, got bad=%q ok=%v", bad, ok)
	}
}
