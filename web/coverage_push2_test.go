package web

import (
	"io"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
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

// runReadCriteria drives the read path — decode then assemble — over a single
// GET request, returning
// the (violation, ok) the wrapper would surface.
func runReadCriteria(t *testing.T, schemaType reflect.Type, url string) (*queryschema.Violation, bool) {
	t.Helper()
	s := queryschema.ExtractRequestSchema(schemaType)
	app := fiber.New()
	var violation *queryschema.Violation
	var ok bool
	app.Get("/x", func(c fiber.Ctx) error {
		_, _, violation, ok = readCriteria(c, s, nil)
		return c.SendStatus(fiber.StatusOK)
	})
	if _, err := app.Test(httptest.NewRequest("GET", url, nil)); err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	return violation, ok
}

type afterBeforeRequest struct {
	After   *string `query:"after"`
	Before  *string `query:"before"`
	Name    *string `query:"name" sort:"asc,desc"`
	OrderBy *string `query:"orderBy"`
}

func TestBuildCriteria_AfterCursorTupleLengthMismatch(t *testing.T) {
	// K has 2 elements → len(K)-1 == 1, but no ?sort (len 0) → reject on after.
	cur := mustCursor(t, []any{"a", "id"}, "")
	v, ok := runReadCriteria(t, reflect.TypeOf(testFindParamsRequest{}), "/x?after="+cur)
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
	v, ok := runReadCriteria(t, reflect.TypeOf(testFindParamsRequest{}), "/x?after="+cur)
	if !ok || v != nil {
		t.Fatalf("expected the hash check deferred to the reader, got v=%+v ok=%v", v, ok)
	}
}

func TestBuildCriteria_BeforeCursorTupleLengthMismatch(t *testing.T) {
	cur := mustCursor(t, []any{"a", "id"}, "")
	v, ok := runReadCriteria(t, reflect.TypeOf(afterBeforeRequest{}), "/x?before="+cur)
	if ok || v == nil || v.Field != "before" {
		t.Fatalf("expected before tuple-length rejection, got v=%+v ok=%v", v, ok)
	}
}

func TestBuildCriteria_AfterAndBeforeTogetherRejected(t *testing.T) {
	a := mustCursor(t, []any{"id"}, "")
	b := mustCursor(t, []any{"id"}, "")
	v, ok := runReadCriteria(t, reflect.TypeOf(afterBeforeRequest{}), "/x?after="+a+"&before="+b)
	if ok || v == nil || v.Field != "before" {
		t.Fatalf("expected after,before mutual exclusion, got v=%+v ok=%v", v, ok)
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

func TestQueryByID_UndeclaredIncludeArchivedRejects(t *testing.T) {
	// The DTO opt-in gate on the by-id surface: without `query:"includeArchived"`
	// on the Request DTO the key is a loud NotDeclared 400, never a silent
	// ignore. The verdict comes from the SHARED gate — the same one a listing
	// runs, and the same one the gRPC and GraphQL by-id routes now run — so
	// this asserts the ROUTE, not the local allowlist helper: the helper only
	// decides which keys this route recognizes at all.
	app := fiber.New()
	pipe := newTestPipeline()
	app.Get("/users/:id", QueryByID(pipe, bareByIDRequest{}, rawItem, &bareByIDHandler{}))

	resp, err := app.Test(httptest.NewRequest("GET", "/users/9a1f6e2c-8b47-4d3a-9c5e-1f0b7d2a6e34?includeArchived=true", nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("undeclared includeArchived must be a 400, got %d (%s)", resp.StatusCode, body)
	}
	if got := violationField(t, body); got != "includeArchived" {
		t.Fatalf("the refusal must name the control, got %q", got)
	}
}

// bareByIDRequest declares NO reserved control, so every control key reaching
// its route is a NotDeclared refusal.
type bareByIDRequest struct{}

func (r bareByIDRequest) ToQuery(criteria queries.ReadCriteria) *testFindIDQuery {
	return &testFindIDQuery{Criteria: criteria}
}

type bareByIDHandler struct{}

func (h *bareByIDHandler) Handle(_ *configuration.AppContext, q *testFindIDQuery) (testUserResult, error) {
	return testUserResult{ID: strPtr(q.PathID().String())}, nil
}
