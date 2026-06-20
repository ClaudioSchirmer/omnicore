package web

import (
	"io"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/application/translation"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/web/responses"
	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
)

// ─── HandleQueryWithID — path-binding conversion failure → 400 ──────────────

type idPathBindReq struct {
	Tenant uuid.UUID `path:"tenantId"`
}

func (r idPathBindReq) ToQuery() *testFindIDQuery { return &testFindIDQuery{} }

func TestHandleQueryWithID_PathBindFailureReturns400(t *testing.T) {
	resetPathSchemaCache()
	app := fiber.New()
	pipe := pipeline.New(translation.Default())
	h := &capturingIDHandler{}

	app.Get("/t/:tenantId/users/:id", HandleQueryWithID(pipe, idPathBindReq{}, responses.RawDoc, h))

	resp, _ := app.Test(httptest.NewRequest("GET", "/t/not-a-uuid/users/abc", nil))
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("expected 400 on path conversion failure, got %d", resp.StatusCode)
	}
	if h.got != nil {
		t.Error("handler must not run when path binding fails")
	}
}

// ─── classifyPathFieldType — every scalar arm + rejections ──────────────────

func TestClassifyPathFieldType_AllScalarKinds(t *testing.T) {
	cases := []struct {
		name string
		val  any
		kind pathFieldKind
		bits int
	}{
		{"int8", int8(0), pathKindInt, 8},
		{"int16", int16(0), pathKindInt, 16},
		{"int32", int32(0), pathKindInt, 32},
		{"int64", int64(0), pathKindInt, 64},
		{"uint", uint(0), pathKindUint, strconvIntSize},
		{"uint8", uint8(0), pathKindUint, 8},
		{"uint16", uint16(0), pathKindUint, 16},
		{"uint32", uint32(0), pathKindUint, 32},
		{"uint64", uint64(0), pathKindUint, 64},
		{"float32", float32(0), pathKindFloat, 32},
		{"float64", float64(0), pathKindFloat, 64},
		{"bool", false, pathKindBool, 0},
		{"string", "", pathKindString, 0},
		{"uuid", uuid.UUID{}, pathKindUUID, 0},
		{"domainID", domain.ID{}, pathKindDomainID, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k, bits, errStr := classifyPathFieldType(reflect.TypeOf(tc.val))
			if errStr != "" {
				t.Fatalf("unexpected error for %s: %q", tc.name, errStr)
			}
			if k != tc.kind {
				t.Errorf("%s kind = %v, want %v", tc.name, k, tc.kind)
			}
			if bits != tc.bits {
				t.Errorf("%s bits = %d, want %d", tc.name, bits, tc.bits)
			}
		})
	}
}

func TestClassifyPathFieldType_Rejections(t *testing.T) {
	type someStruct struct{ X int }
	cases := []struct {
		name string
		typ  reflect.Type
		hint string
	}{
		{"pointer", reflect.TypeOf((*int)(nil)), "pointer"},
		{"slice", reflect.TypeOf([]int{}), "slice"},
		{"array", reflect.TypeOf([3]int{}), "slice"},
		{"struct", reflect.TypeOf(someStruct{}), "struct"},
		{"map", reflect.TypeOf(map[string]int{}), "does not support"},
		{"chan", reflect.TypeOf(make(chan int)), "does not support"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k, _, errStr := classifyPathFieldType(tc.typ)
			if errStr == "" {
				t.Fatalf("%s: expected rejection error, got kind=%v", tc.name, k)
			}
			if k != 0 {
				t.Errorf("%s: expected kind 0 on rejection, got %v", tc.name, k)
			}
		})
	}
}

// strconvIntSize mirrors strconv.IntSize without importing strconv here just
// for the constant — the classifier uses strconv.IntSize for word-size ints.
const strconvIntSize = 32 << (^uint(0) >> 63)

// ─── hasAnyPathTag ──────────────────────────────────────────────────────────

func TestHasAnyPathTag(t *testing.T) {
	resetPathSchemaCache()
	if !hasAnyPathTag(reflect.TypeOf(stringPathReq{})) {
		t.Error("expected hasAnyPathTag=true for a Request with a path: tag")
	}
	if hasAnyPathTag(reflect.TypeOf(emptyReq{})) {
		t.Error("expected hasAnyPathTag=false for a Request with no path: tag")
	}
}

// ─── projection walks — anonymous embed, unexported, ptr-struct, map ────────

type embeddedBase struct {
	ID   *string `json:"id,omitempty"`
	memo string  // unexported — skipped by both walks
}

type withEmbedAndPtrStruct struct {
	embeddedBase           // anonymous struct embed — promoted
	Name         *string   `json:"name,omitempty"`
	Profile      *innerPtr `json:"profile,omitempty"`
	Tags         *string   `json:",omitempty"` // empty wire name → falls back to Go field name "Tags"
}

type innerPtr struct {
	City *string `json:"city,omitempty"`
}

func TestExtractProjectionSchema_EmbedUnexportedPtrStruct(t *testing.T) {
	s := extractProjectionSchema(reflect.TypeOf(withEmbedAndPtrStruct{}))
	// promoted embed field
	if got := s.paths["id"]; got != "ID" {
		t.Errorf("promoted id → %q, want ID", got)
	}
	// pointer-to-struct recursion
	if got := s.paths["profile.city"]; got != "Profile.City" {
		t.Errorf("profile.city → %q, want Profile.City", got)
	}
	// empty json name falls back to the Go field name
	if got := s.paths["Tags"]; got != "Tags" {
		t.Errorf("Tags (empty json name fallback) → %q, want Tags", got)
	}
	// unexported never surfaces
	if _, ok := s.paths["memo"]; ok {
		t.Error("unexported field must not appear in projection schema")
	}
}

type guardWithMap struct {
	ID      *string           `json:"id,omitempty"`
	Meta    map[string]string `json:"meta,omitempty"`    // map tolerated without pointer
	Profile *innerGuard       `json:"profile,omitempty"` // ptr-struct recursion
}

type innerGuard struct {
	City *string `json:"city,omitempty"`
}

func TestValidateFieldsResponse_MapAndPtrStructAccepted(t *testing.T) {
	errs := validateFieldsResponse(reflect.TypeOf(guardWithMap{}))
	if len(errs) != 0 {
		t.Errorf("expected no violations (map tolerated, ptr-struct recurses cleanly), got %v", errs)
	}
}

type guardEmbedViolation struct {
	guardEmbedInner         // anonymous embed carrying a violation
	Name            *string `json:"name,omitempty"`
}

type guardEmbedInner struct {
	Bad string `json:"bad,omitempty"` // non-pointer scalar → violation, path kept at parent level
}

func TestValidateFieldsResponse_AnonymousEmbedViolationSurfaces(t *testing.T) {
	errs := validateFieldsResponse(reflect.TypeOf(guardEmbedViolation{}))
	if len(errs) == 0 {
		t.Fatal("expected the embedded non-pointer scalar to surface a violation")
	}
}

// ─── buildExportCriteria — search / includeArchived / sort / bad operator ───

func TestHandleQueryAsCSV_SearchAndIncludeArchived(t *testing.T) {
	app := fiber.New()
	h := newExportHandler()
	mountCSV(app, h)

	resp, _ := app.Test(httptest.NewRequest("GET", "/users.csv?search=jo&includeArchived=true", nil))
	if resp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, b)
	}
	if h.got.Criteria.Search != "jo" {
		t.Errorf("expected Search=jo, got %q", h.got.Criteria.Search)
	}
	if !h.got.Criteria.IncludeArchived {
		t.Error("expected IncludeArchived=true")
	}
}

func TestHandleQueryAsCSV_ValidSortTranslates(t *testing.T) {
	app := fiber.New()
	h := newExportHandler()
	mountCSV(app, h)

	resp, _ := app.Test(httptest.NewRequest("GET", "/users.csv?sort=-name", nil))
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if len(h.got.Criteria.Sort) != 1 || h.got.Criteria.Sort[0].Field != "Name" || !h.got.Criteria.Sort[0].Desc {
		t.Errorf("expected Sort=[Name desc], got %+v", h.got.Criteria.Sort)
	}
}

func TestHandleQueryAsCSV_UnknownSortTokenReturns400(t *testing.T) {
	app := fiber.New()
	h := newExportHandler()
	mountCSV(app, h)

	resp, _ := app.Test(httptest.NewRequest("GET", "/users.csv?sort=bogus", nil))
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("expected 400 for unknown sort token, got %d", resp.StatusCode)
	}
	if h.got != nil {
		t.Error("handler must not run on a rejected sort token")
	}
}

func TestHandleQueryAsCSV_BadOperatorReturns400(t *testing.T) {
	app := fiber.New()
	h := newExportHandler()
	mountCSV(app, h)

	// email declares filter:"eq" only — `.in` is outside the set.
	resp, _ := app.Test(httptest.NewRequest("GET", "/users.csv?email.in=a,b", nil))
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("expected 400 for operator outside declared set, got %d", resp.StatusCode)
	}
	if h.got != nil {
		t.Error("handler must not run on a rejected operator")
	}
}
