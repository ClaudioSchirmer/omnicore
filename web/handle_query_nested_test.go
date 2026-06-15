package web

import (
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/web/responses"
	"github.com/gofiber/fiber/v3"
)

// Address-side embed group used by the nested tests. View tag intentionally
// omitted on the leaves — the auto-snake default (zipCode → zip_code,
// postalArea → postal_area) is what we want to assert.
type testAddrFilter struct {
	City    *string `query:"city"    filter:"eq,istartswith"`
	State   *string `query:"state"   filter:"eq,in"`
	ZipCode *string `query:"zipCode" filter:"eq,startswith"` // auto-snake → addresses.zip_code
}

type testNestedRequest struct {
	Name      *string        `query:"name"  filter:"eq,startswith"`
	Email     *string        `query:"email" filter:"eq"`
	Addresses testAddrFilter `query:"addresses"`

	Limit *int64 `query:"limit"`
}

type testNestedQuery struct {
	pipeline.QueryBase
	Criteria queries.ReadCriteria
}

func (q *testNestedQuery) ToCriteria(_ *configuration.AppContext) queries.ReadCriteria {
	return q.Criteria
}

func (r testNestedRequest) ToQuery(crit queries.ReadCriteria) *testNestedQuery {
	return &testNestedQuery{Criteria: crit}
}

type capturingNestedHandler struct {
	got *testNestedQuery
}

func (h *capturingNestedHandler) Handle(_ *configuration.AppContext, q *testNestedQuery) (queries.Page, error) {
	h.got = q
	return queries.Page{}, nil
}

// dispatchNested runs the wrapper end to end and returns the assembled
// criteria + the HTTP status, so each case asserts on the doc path the
// walker computed for the nested wire key.
func dispatchNested(t *testing.T, query string) (queries.ReadCriteria, int) {
	t.Helper()
	app := fiber.New()
	pipe := newTestPipeline()
	h := &capturingNestedHandler{}
	app.Get("/users", HandleQueryWithParams(pipe, testNestedRequest{}, responses.RawDoc, h))

	resp, err := app.Test(httptest.NewRequest("GET", "/users"+query, nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if h.got == nil {
		return queries.ReadCriteria{}, resp.StatusCode
	}
	return h.got.Criteria, resp.StatusCode
}

func TestNested_LeafWireKeyMapsToDottedDocPath(t *testing.T) {
	crit, status := dispatchNested(t, "?addresses.city=Berlin")
	if status != fiber.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	if crit.Filter["addresses.city"] != "Berlin" {
		t.Errorf("expected Filter[addresses.city]=Berlin, got %v", crit.Filter)
	}
}

func TestNested_AutoSnakeAppliesToLeafName(t *testing.T) {
	// zipCode wire → zip_code doc segment (default snake convention).
	// Confirms no view: tag is needed for the common camelCase case.
	// Use a non-numeric value so parseValue keeps it a string and the
	// equality assertion is unambiguous.
	crit, status := dispatchNested(t, "?addresses.zipCode=SW1A2AA")
	if status != fiber.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	if crit.Filter["addresses.zip_code"] != "SW1A2AA" {
		t.Errorf("expected Filter[addresses.zip_code]=SW1A2AA, got %v", crit.Filter)
	}
	if _, lingering := crit.Filter["addresses.zipCode"]; lingering {
		t.Errorf("auto-snake should remove the camelCase wire key from the filter, got %v", crit.Filter)
	}
}

func TestNested_OperatorSuffixOnNestedLeaf(t *testing.T) {
	crit, status := dispatchNested(t, "?addresses.zipCode.startswith=100")
	if status != fiber.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	got, ok := crit.Filter["addresses.zip_code"].(map[string]any)
	if !ok {
		t.Fatalf("expected nested filter to be a regex map, got %T (%v)", crit.Filter["addresses.zip_code"], crit.Filter["addresses.zip_code"])
	}
	if got["$regex"] != "^100" {
		t.Errorf("expected $regex='^100', got %v", got["$regex"])
	}
}

func TestNested_OperatorSuffixOnNestedLeaf_IStartsWith(t *testing.T) {
	crit, status := dispatchNested(t, "?addresses.city.istartswith=ber")
	if status != fiber.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	got, ok := crit.Filter["addresses.city"].(map[string]any)
	if !ok {
		t.Fatalf("expected map, got %T", crit.Filter["addresses.city"])
	}
	if got["$regex"] != "^ber" || got["$options"] != "i" {
		t.Errorf("expected {$regex:^ber, $options:i}, got %v", got)
	}
}

func TestNested_TopLevelEqStillWorks(t *testing.T) {
	// Adding nested support must not regress flat top-level cases.
	crit, status := dispatchNested(t, "?name=Bob")
	if status != fiber.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	if crit.Filter["name"] != "Bob" {
		t.Errorf("expected Filter[name]=Bob, got %v", crit.Filter)
	}
}

func TestNested_TopLevelOperatorSuffix(t *testing.T) {
	crit, status := dispatchNested(t, "?name.startswith=Bob")
	if status != fiber.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	got, ok := crit.Filter["name"].(map[string]any)
	if !ok {
		t.Fatalf("expected map, got %T", crit.Filter["name"])
	}
	if got["$regex"] != "^Bob" {
		t.Errorf("expected $regex='^Bob', got %v", got["$regex"])
	}
}

func TestNested_UndeclaredLeafReturns400(t *testing.T) {
	// `addresses.country` is NOT declared on testAddrFilter — must 400.
	_, status := dispatchNested(t, "?addresses.country=DE")
	if status != fiber.StatusBadRequest {
		t.Fatalf("expected 400 for undeclared nested leaf, got %d", status)
	}
}

func TestNested_UndeclaredOpOnNestedLeafReturns400(t *testing.T) {
	// City declared with eq,istartswith — using .startswith is rejected.
	_, status := dispatchNested(t, "?addresses.city.startswith=Berlin")
	if status != fiber.StatusBadRequest {
		t.Fatalf("expected 400 for op outside declared list, got %d", status)
	}
}

func TestNested_UnknownPrefixReturns400(t *testing.T) {
	// `orders.city` — no embed group named `orders` declared.
	_, status := dispatchNested(t, "?orders.city=X")
	if status != fiber.StatusBadRequest {
		t.Fatalf("expected 400 for unknown embed prefix, got %d", status)
	}
}

func TestNested_ReservedPaginationOnlyAtTopLevel(t *testing.T) {
	// `limit` at top level is reserved → honored.
	crit, status := dispatchNested(t, "?limit=42")
	if status != fiber.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	if crit.Limit != 42 {
		t.Errorf("expected Limit=42, got %d", crit.Limit)
	}
	// `addresses.limit` is not a declared leaf and the embed group does NOT
	// honor reserved keys recursively → must 400.
	_, status = dispatchNested(t, "?addresses.limit=42")
	if status != fiber.StatusBadRequest {
		t.Fatalf("expected 400 for pagination key inside embed group, got %d", status)
	}
}

func TestNested_ViewTagOverridesDocSegment(t *testing.T) {
	// View tag wins over the auto-snake default — exotic schemas where the
	// doc field name diverges from the convention can opt in to verbatim.
	type WithOverride struct {
		City *string `query:"city" filter:"eq" view:"municipality"`
	}
	type Req struct {
		Loc WithOverride `query:"locations"`
	}
	type Q struct {
		pipeline.QueryBase
		Criteria queries.ReadCriteria
	}
	type Wrap struct{ Req Req }
	app := fiber.New()
	pipe := newTestPipeline()
	captured := &queries.ReadCriteria{}
	app.Get("/x", func(c fiber.Ctx) error {
		crit, bad, ok := ParseCriteria(c, Req{})
		if !ok {
			return RespondSchemaViolation(c, pipe, bad)
		}
		*captured = crit
		return c.SendStatus(fiber.StatusOK)
	})
	resp, _ := app.Test(httptest.NewRequest("GET", "/x?locations.city=Berlin", nil))
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if captured.Filter["locations.municipality"] != "Berlin" {
		t.Errorf("expected Filter[locations.municipality]=Berlin, got %v", captured.Filter)
	}
}

func TestNested_SchemaCacheCarriesDocPaths(t *testing.T) {
	pipe := newTestPipeline()
	h := &capturingNestedHandler{}
	app := fiber.New()
	app.Get("/users", HandleQueryWithParams(pipe, testNestedRequest{}, responses.RawDoc, h))

	_, _ = app.Test(httptest.NewRequest("GET", "/users", nil))

	v, ok := schemaCache.Load(reflect.TypeOf(testNestedRequest{}))
	if !ok {
		t.Fatal("expected schemaCache to contain entry")
	}
	schema, _ := v.(*requestSchema)
	if got := schema.filters["addresses.zipCode"].docPath; got != "addresses.zip_code" {
		t.Errorf("expected addresses.zipCode → addresses.zip_code, got %q", got)
	}
	if got := schema.filters["addresses.city"].docPath; got != "addresses.city" {
		t.Errorf("expected addresses.city → addresses.city, got %q", got)
	}
	if got := schema.filters["name"].docPath; got != "name" {
		t.Errorf("expected name → name, got %q", got)
	}
}

func TestNested_PointerToNestedStructAlsoWorks(t *testing.T) {
	// Pointer-typed embed group must walk the same as value-typed.
	type Inner struct {
		Tag *string `query:"tag" filter:"eq"`
	}
	type Req struct {
		Meta *Inner `query:"meta"`
	}
	app := fiber.New()
	pipe := newTestPipeline()
	captured := &queries.ReadCriteria{}
	app.Get("/x", func(c fiber.Ctx) error {
		crit, bad, ok := ParseCriteria(c, Req{})
		if !ok {
			return RespondSchemaViolation(c, pipe, bad)
		}
		*captured = crit
		return c.SendStatus(fiber.StatusOK)
	})
	resp, _ := app.Test(httptest.NewRequest("GET", "/x?meta.tag=urgent", nil))
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if captured.Filter["meta.tag"] != "urgent" {
		t.Errorf("expected Filter[meta.tag]=urgent, got %v", captured.Filter)
	}
}
