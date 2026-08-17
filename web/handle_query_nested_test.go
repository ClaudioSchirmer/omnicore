package web

import (
	"net/http/httptest"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/application/queries"
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

	Limit *int64 `query:"first"`
}

type testNestedQuery struct {
	pipeline.QueryBase
	Criteria queries.ReadCriteria
}

func (q *testNestedQuery) ToCriteria(_ *configuration.AppContext) (queries.ReadCriteria, error) {
	return q.Criteria, nil
}

// FromQueryResult is the mandatory doc→Result hook of queries.QueryWithParams; the
// nested cases assert criteria assembly, so the framework-filled Result rides
// through untouched.
func (q *testNestedQuery) FromQueryResult(_ *configuration.AppContext, r testUserResult) (testUserResult, error) {
	return r, nil
}

func (r testNestedRequest) ToQuery(crit queries.ReadCriteria) *testNestedQuery {
	return &testNestedQuery{Criteria: crit}
}

type capturingNestedHandler struct {
	got *testNestedQuery
}

func (h *capturingNestedHandler) Handle(_ *configuration.AppContext, q *testNestedQuery) (queries.PageOf[testUserResult], error) {
	h.got = q
	return queries.PageOf[testUserResult]{}, nil
}

// dispatchNested runs the wrapper end to end and returns the assembled
// criteria + the HTTP status, so each case asserts on the doc path the
// walker computed for the nested wire key.
func dispatchNested(t *testing.T, query string) (queries.ReadCriteria, int) {
	t.Helper()
	app := fiber.New()
	pipe := newTestPipeline()
	h := &capturingNestedHandler{}
	app.Get("/users", QueryWithParams(pipe, testNestedRequest{}, rawItem, h))

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
	if crit.Filter["Addresses.City"] != "Berlin" {
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
	if crit.Filter["Addresses.ZipCode"] != "SW1A2AA" {
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
	got, ok := crit.Filter["Addresses.ZipCode"].(queries.TextMatch)
	if !ok {
		t.Fatalf("expected nested filter to be a TextMatch, got %T (%v)", crit.Filter["Addresses.ZipCode"], crit.Filter["Addresses.ZipCode"])
	}
	if got.Value != "100" || got.Kind != queries.TextPrefix || got.CaseInsensitive {
		t.Errorf("expected {Value:100, Kind:Prefix}, got %#v", got)
	}
}

func TestNested_OperatorSuffixOnNestedLeaf_IStartsWith(t *testing.T) {
	crit, status := dispatchNested(t, "?addresses.city.istartswith=ber")
	if status != fiber.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	got, ok := crit.Filter["Addresses.City"].(queries.TextMatch)
	if !ok {
		t.Fatalf("expected TextMatch, got %T", crit.Filter["Addresses.City"])
	}
	if got.Value != "ber" || got.Kind != queries.TextPrefix || !got.CaseInsensitive {
		t.Errorf("expected {Value:ber, Kind:Prefix, CaseInsensitive:true}, got %#v", got)
	}
}

func TestNested_TopLevelEqStillWorks(t *testing.T) {
	// Adding nested support must not regress flat top-level cases.
	crit, status := dispatchNested(t, "?name=Bob")
	if status != fiber.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	if crit.Filter["Name"] != "Bob" {
		t.Errorf("expected Filter[name]=Bob, got %v", crit.Filter)
	}
}

func TestNested_TopLevelOperatorSuffix(t *testing.T) {
	crit, status := dispatchNested(t, "?name.startswith=Bob")
	if status != fiber.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	got, ok := crit.Filter["Name"].(queries.TextMatch)
	if !ok {
		t.Fatalf("expected TextMatch, got %T", crit.Filter["Name"])
	}
	if got.Value != "Bob" || got.Kind != queries.TextPrefix || got.CaseInsensitive {
		t.Errorf("expected {Value:Bob, Kind:Prefix}, got %#v", got)
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
	crit, status := dispatchNested(t, "?first=42")
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
	// Manual route: the parser is the manual-side parsing surface. A
	// map Response keeps it in pass-through mode (no projection schema), which
	// is irrelevant here — the nested FILTER walk is Request-driven and is the
	// contract under test.
	parser := NewQueryParser[Req, map[string]any]()
	app := fiber.New()
	pipe := newTestPipeline()
	captured := &queries.ReadCriteria{}
	app.Get("/x", func(c fiber.Ctx) error {
		crit, v, ok := parser.Parse(c)
		if !ok {
			return RespondSchemaViolation(c, pipe, v.Field)
		}
		*captured = crit
		return c.SendStatus(fiber.StatusOK)
	})
	resp, _ := app.Test(httptest.NewRequest("GET", "/x?locations.city=Berlin", nil))
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if captured.Filter["Loc.City"] != "Berlin" {
		t.Errorf("expected Filter[locations.municipality]=Berlin, got %v", captured.Filter)
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
	parser := NewQueryParser[Req, map[string]any]()
	app := fiber.New()
	pipe := newTestPipeline()
	captured := &queries.ReadCriteria{}
	app.Get("/x", func(c fiber.Ctx) error {
		crit, v, ok := parser.Parse(c)
		if !ok {
			return RespondSchemaViolation(c, pipe, v.Field)
		}
		*captured = crit
		return c.SendStatus(fiber.StatusOK)
	})
	resp, _ := app.Test(httptest.NewRequest("GET", "/x?meta.tag=urgent", nil))
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if captured.Filter["Meta.Tag"] != "urgent" {
		t.Errorf("expected Filter[meta.tag]=urgent, got %v", captured.Filter)
	}
}
