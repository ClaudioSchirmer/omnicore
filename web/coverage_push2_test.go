package web

import (
	"reflect"
	"testing"

	"net/http/httptest"

	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/gofiber/fiber/v3"
)

// mustCursor encodes a keyset cursor for the criteria-alignment tests.
func mustCursor(t *testing.T, k []any, hash string) string {
	t.Helper()
	c, err := queries.EncodeCursor(k, hash)
	if err != nil {
		t.Fatalf("EncodeCursor: %v", err)
	}
	return c
}

// runBuildCriteria drives buildCriteria over a single GET request, returning
// the (badField, ok) the wrapper would surface.
func runBuildCriteria(t *testing.T, schemaType reflect.Type, url string) (string, bool) {
	t.Helper()
	s := extractAllowedKeys(schemaType)
	app := fiber.New()
	var badField string
	var ok bool
	app.Get("/x", func(c fiber.Ctx) error {
		_, badField, ok = buildCriteria(c, s, nil)
		return c.SendStatus(fiber.StatusOK)
	})
	if _, err := app.Test(httptest.NewRequest("GET", url, nil)); err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	return badField, ok
}

type afterBeforeRequest struct {
	After  *string `query:"after"`
	Before *string `query:"before"`
	Sort   *string `query:"sort"`
}

func TestBuildCriteria_AfterCursorTupleLengthMismatch(t *testing.T) {
	// K has 2 elements → len(K)-1 == 1, but no ?sort (len 0) → reject on after.
	cur := mustCursor(t, []any{"a", "id"}, "")
	bad, ok := runBuildCriteria(t, reflect.TypeOf(testFindParamsRequest{}), "/x?after="+cur)
	if ok || bad != "after" {
		t.Fatalf("expected after tuple-length rejection, got bad=%q ok=%v", bad, ok)
	}
}

func TestBuildCriteria_AfterCursorHashMismatch(t *testing.T) {
	// K aligns (len 1, no sort) but H is non-empty while criteria context
	// hashes to "" → hash mismatch → reject on after.
	cur := mustCursor(t, []any{"id"}, "deadbeefcafe")
	bad, ok := runBuildCriteria(t, reflect.TypeOf(testFindParamsRequest{}), "/x?after="+cur)
	if ok || bad != "after" {
		t.Fatalf("expected after hash mismatch rejection, got bad=%q ok=%v", bad, ok)
	}
}

func TestBuildCriteria_BeforeCursorTupleLengthMismatch(t *testing.T) {
	cur := mustCursor(t, []any{"a", "id"}, "")
	bad, ok := runBuildCriteria(t, reflect.TypeOf(afterBeforeRequest{}), "/x?before="+cur)
	if ok || bad != "before" {
		t.Fatalf("expected before tuple-length rejection, got bad=%q ok=%v", bad, ok)
	}
}

func TestBuildCriteria_AfterAndBeforeTogetherRejected(t *testing.T) {
	a := mustCursor(t, []any{"id"}, "")
	b := mustCursor(t, []any{"id"}, "")
	bad, ok := runBuildCriteria(t, reflect.TypeOf(afterBeforeRequest{}), "/x?after="+a+"&before="+b)
	if ok || bad != "after,before" {
		t.Fatalf("expected after,before mutual exclusion, got bad=%q ok=%v", bad, ok)
	}
}

func TestValidateByIDQuery_RejectsUnknownKeys(t *testing.T) {
	app := fiber.New()
	var bad string
	var ok bool
	app.Get("/x", func(c fiber.Ctx) error {
		bad, ok = validateByIDQuery(c)
		return c.SendStatus(fiber.StatusOK)
	})
	// Two unknown keys exercise the early-return guard inside VisitAll.
	if _, err := app.Test(httptest.NewRequest("GET", "/x?foo=1&bar=2", nil)); err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if ok || bad == "" {
		t.Fatalf("expected by-id query rejection, got bad=%q ok=%v", bad, ok)
	}
}

func TestValidateByIDQuery_AllowsIncludeArchived(t *testing.T) {
	app := fiber.New()
	var bad string
	var ok bool
	app.Get("/x", func(c fiber.Ctx) error {
		bad, ok = validateByIDQuery(c)
		return c.SendStatus(fiber.StatusOK)
	})
	if _, err := app.Test(httptest.NewRequest("GET", "/x?includeArchived=true", nil)); err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if !ok || bad != "" {
		t.Fatalf("includeArchived must be allowed on by-id, got bad=%q ok=%v", bad, ok)
	}
}

// parseKeyAgainstSchema: a key whose last segment is a known operator but
// whose prefix is not a declared filter resolves to ("","").
func TestParseKeyAgainstSchema_UnknownPrefixWithKnownOp(t *testing.T) {
	s := extractAllowedKeys(reflect.TypeOf(testFindParamsRequest{}))
	wirePath, op := parseKeyAgainstSchema("bogus.in", s)
	if wirePath != "" || op != "" {
		t.Fatalf("unknown prefix with known op must reject, got (%q,%q)", wirePath, op)
	}
}

func TestParseKeyAgainstSchema_NoDotReturnsEmpty(t *testing.T) {
	s := extractAllowedKeys(reflect.TypeOf(testFindParamsRequest{}))
	wirePath, op := parseKeyAgainstSchema("totallyunknown", s)
	if wirePath != "" || op != "" {
		t.Fatalf("unknown dotless key must reject, got (%q,%q)", wirePath, op)
	}
}

// --- extractAllowedKeys: pointer type + nested embed group ------------------

type pushEmbedLeaf struct {
	ZipCode *string `query:"zipCode" filter:"eq"`
}

type pushNestedEmbedRequest struct {
	Name      *string       `query:"name" filter:"eq"`
	Addresses pushEmbedLeaf `query:"addresses"` // embed group — no filter tag
	Limit     *int64        `query:"limit"`
}

func TestExtractAllowedKeys_PointerTypeAndNestedEmbed(t *testing.T) {
	s := extractAllowedKeys(reflect.PointerTo(reflect.TypeOf(pushNestedEmbedRequest{})))
	if _, ok := s.filters["name"]; !ok {
		t.Errorf("expected top-level name filter, got %v", s.filters)
	}
	spec, ok := s.filters["addresses.zipCode"]
	if !ok {
		t.Fatalf("expected nested embed filter addresses.zipCode, got %v", s.filters)
	}
	if spec.docPath != "Addresses.ZipCode" {
		t.Errorf("nested embed docPath = %q, want Addresses.ZipCode", spec.docPath)
	}
	if !s.reserved["limit"] {
		t.Errorf("limit must be a reserved key, reserved=%v", s.reserved)
	}
}

// --- parseSortWithSchema / parseProjection direct edge cases ----------------

func TestParseSortWithSchema_EdgeCases(t *testing.T) {
	// Empty string → no sort.
	if fields, bad, ok := parseSortWithSchema("", nil); !ok || bad != "" || fields != nil {
		t.Fatalf("empty sort = (%v,%q,%v)", fields, bad, ok)
	}
	// Nil schema → verbatim pass-through, empty tokens skipped.
	fields, bad, ok := parseSortWithSchema("-name,,age", nil)
	if !ok || bad != "" || len(fields) != 2 {
		t.Fatalf("nil-schema sort = (%v,%q,%v)", fields, bad, ok)
	}
	if fields[0].Field != "name" || !fields[0].Desc {
		t.Errorf("expected name desc, got %+v", fields[0])
	}
}

func TestParseSortWithSchema_UnknownTokenWithSchema(t *testing.T) {
	ps := extractProjectionSchema(reflect.TypeOf(sparseUser{}))
	if _, bad, ok := parseSortWithSchema("bogus", ps); ok || bad != "bogus" {
		t.Fatalf("expected unknown sort token rejection, got bad=%q ok=%v", bad, ok)
	}
}

func TestParseProjection_EmptyAndNilSchema(t *testing.T) {
	if proj, _, bad, ok := parseProjection("", nil); !ok || bad != "" || proj != nil {
		t.Fatalf("empty projection = (%v,%q,%v)", proj, bad, ok)
	}
	// Nil schema → verbatim entries, empty tokens skipped.
	proj, wireSet, bad, ok := parseProjection("a,,b", nil)
	if !ok || bad != "" || len(proj) != 2 || !wireSet["a"] {
		t.Fatalf("nil-schema projection = (%v,%v,%q,%v)", proj, wireSet, bad, ok)
	}
}

// --- projection / response-guard walker defensive branches ------------------

func TestWalkProjectionLevel_PointerAndNonStructDefensive(t *testing.T) {
	s := &projectionSchema{paths: map[string]string{}}
	type leaf struct {
		Name *string `json:"name,omitempty"`
	}
	// Pointer top type exercises the deref loop; the underlying struct still
	// contributes its leaf path.
	walkProjectionLevel(reflect.PointerTo(reflect.TypeOf(leaf{})), "", "", s)
	if _, ok := s.paths["name"]; !ok {
		t.Fatalf("expected name path after pointer deref, got %v", s.paths)
	}
	// Non-struct top type returns without panicking and adds nothing.
	before := len(s.paths)
	walkProjectionLevel(reflect.TypeOf(0), "", "", s)
	if len(s.paths) != before {
		t.Fatalf("non-struct walk must add nothing, paths changed: %v", s.paths)
	}
}

type pushGuardEmbedInner struct {
	Inner *string `json:"inner,omitempty"`
}

type guardWithEmbedAndSkips struct {
	pushGuardEmbedInner
	hidden *string //nolint:unused // unexported field must be skipped by the guard
	Skip   *string `json:"-"`
	Empty  *string `json:",omitempty"` // empty json name falls back to field name
	Name   *string `json:"name,omitempty"`
}

func TestValidateFieldsResponse_EmbedSkipAndEmptyNameCompliant(t *testing.T) {
	errs := validateFieldsResponse(reflect.TypeOf(guardWithEmbedAndSkips{}))
	if len(errs) != 0 {
		t.Fatalf("expected fully compliant type, got violations: %v", errs)
	}
}

func TestWalkResponseGuard_PointerAndNonStructDefensive(t *testing.T) {
	var errs []string
	// Pointer top type → deref loop; non-struct → early return. Neither emits.
	walkResponseGuard(reflect.PointerTo(reflect.TypeOf(pushGuardEmbedInner{})), "", &errs)
	walkResponseGuard(reflect.TypeOf(0), "", &errs)
	if len(errs) != 0 {
		t.Fatalf("defensive walks must not report violations, got %v", errs)
	}
}
