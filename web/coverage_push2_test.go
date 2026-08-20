package web

import (
	"reflect"
	"testing"

	"net/http/httptest"

	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/web/queryschema"
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
// the (violation, ok) the wrapper would surface.
func runBuildCriteria(t *testing.T, schemaType reflect.Type, url string) (*queryschema.Violation, bool) {
	t.Helper()
	s := queryschema.ExtractRequestSchema(schemaType)
	app := fiber.New()
	var violation *queryschema.Violation
	var ok bool
	app.Get("/x", func(c fiber.Ctx) error {
		_, _, violation, ok = buildCriteria(c, s, nil)
		return c.SendStatus(fiber.StatusOK)
	})
	if _, err := app.Test(httptest.NewRequest("GET", url, nil)); err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	return violation, ok
}

type afterBeforeRequest struct {
	After  *string `query:"after"`
	Before *string `query:"before"`
	Name   *string `query:"name" sort:"asc,desc"`
}

func TestBuildCriteria_AfterCursorTupleLengthMismatch(t *testing.T) {
	// K has 2 elements → len(K)-1 == 1, but no ?sort (len 0) → reject on after.
	cur := mustCursor(t, []any{"a", "id"}, "")
	v, ok := runBuildCriteria(t, reflect.TypeOf(testFindParamsRequest{}), "/x?after="+cur)
	if ok || v == nil || v.Field != "after" {
		t.Fatalf("expected after tuple-length rejection, got v=%+v ok=%v", v, ok)
	}
}

func TestBuildCriteria_AfterCursorHashDeferredToReader(t *testing.T) {
	// K aligns (len 1, no sort); H is non-empty while the WIRE context hashes
	// to "". The wrapper deliberately does NOT compare hashes — at this layer
	// the criteria predates the Query's ToCriteria overlays, while cursors are
	// stamped post-overlay; the reader performs the authoritative check.
	cur := mustCursor(t, []any{"id"}, "deadbeefcafe")
	v, ok := runBuildCriteria(t, reflect.TypeOf(testFindParamsRequest{}), "/x?after="+cur)
	if !ok || v != nil {
		t.Fatalf("expected the hash check deferred to the reader, got v=%+v ok=%v", v, ok)
	}
}

func TestBuildCriteria_BeforeCursorTupleLengthMismatch(t *testing.T) {
	cur := mustCursor(t, []any{"a", "id"}, "")
	v, ok := runBuildCriteria(t, reflect.TypeOf(afterBeforeRequest{}), "/x?before="+cur)
	if ok || v == nil || v.Field != "before" {
		t.Fatalf("expected before tuple-length rejection, got v=%+v ok=%v", v, ok)
	}
}

func TestBuildCriteria_AfterAndBeforeTogetherRejected(t *testing.T) {
	a := mustCursor(t, []any{"id"}, "")
	b := mustCursor(t, []any{"id"}, "")
	v, ok := runBuildCriteria(t, reflect.TypeOf(afterBeforeRequest{}), "/x?after="+a+"&before="+b)
	if ok || v == nil || v.Field != "before" {
		t.Fatalf("expected after,before mutual exclusion, got v=%+v ok=%v", v, ok)
	}
}

func TestValidateByIDQuery_RejectsUnknownKeys(t *testing.T) {
	app := fiber.New()
	var bad string
	var ok bool
	app.Get("/x", func(c fiber.Ctx) error {
		bad, ok = validateByIDQuery(c, true)
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
		bad, ok = validateByIDQuery(c, true)
		return c.SendStatus(fiber.StatusOK)
	})
	if _, err := app.Test(httptest.NewRequest("GET", "/x?includeArchived=true", nil)); err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if !ok || bad != "" {
		t.Fatalf("includeArchived must be allowed on by-id, got bad=%q ok=%v", bad, ok)
	}
}

func TestValidateByIDQuery_UndeclaredIncludeArchivedRejects(t *testing.T) {
	// The DTO opt-in gate on the by-id surface: without `query:"includeArchived"`
	// on the Request DTO the key is a loud NotDeclared 400, never a silent ignore.
	app := fiber.New()
	var bad string
	var ok bool
	app.Get("/x", func(c fiber.Ctx) error {
		bad, ok = validateByIDQuery(c, false)
		return c.SendStatus(fiber.StatusOK)
	})
	if _, err := app.Test(httptest.NewRequest("GET", "/x?includeArchived=true", nil)); err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if ok || bad != "includeArchived" {
		t.Fatalf("undeclared includeArchived must reject with its key, got ok=%v bad=%q", ok, bad)
	}
}
