package web

import (
	"net/http/httptest"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/web/responses"
	"github.com/gofiber/fiber/v3"
)

// testCoerceRequest declares leaves with different Go base types so the
// coercion rules in coerceValue can be observed end-to-end through the
// wrapper. The wire is always strings; the criteria carries the typed
// values that match each field's stored type in Mongo.
type testCoerceRequest struct {
	Code   *string  `query:"code"   filter:"eq,in"`     // string — keep verbatim
	Age    *int64   `query:"age"    filter:"eq,in,gte"` // int64  — parse decimal
	Score  *float64 `query:"score"  filter:"eq,gte"`    // float  — parse float
	Active *bool    `query:"active" filter:"eq"`        // bool   — parse "true"/"false"

	Limit *int64 `query:"first"`
}

type testCoerceQuery struct {
	pipeline.QueryBase
	Criteria queries.ReadCriteria
}

func (q *testCoerceQuery) ToCriteria(_ *configuration.AppContext) (queries.ReadCriteria, error) {
	return q.Criteria, nil
}

func (r testCoerceRequest) ToQuery(crit queries.ReadCriteria) *testCoerceQuery {
	return &testCoerceQuery{Criteria: crit}
}

type capturingCoerceHandler struct {
	got *testCoerceQuery
}

func (h *capturingCoerceHandler) Handle(_ *configuration.AppContext, q *testCoerceQuery) (queries.Page, error) {
	h.got = q
	return queries.Page{}, nil
}

func dispatchCoerce(t *testing.T, query string) (queries.ReadCriteria, int) {
	t.Helper()
	app := fiber.New()
	pipe := newTestPipeline()
	h := &capturingCoerceHandler{}
	app.Get("/x", QueryWithParams(pipe, testCoerceRequest{}, responses.RawDoc, h))

	resp, err := app.Test(httptest.NewRequest("GET", "/x"+query, nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if h.got == nil {
		return queries.ReadCriteria{}, resp.StatusCode
	}
	return h.got.Criteria, resp.StatusCode
}

func TestCoerce_StringLeafKeepsDigitsAsString(t *testing.T) {
	// Regression: pre-fix, parseValue would coerce "95014" → int64(95014)
	// and Mongo's string-typed `code` field would never match. The string
	// leaf must keep the wire value verbatim.
	crit, status := dispatchCoerce(t, "?code=95014")
	if status != fiber.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	got, ok := crit.Filter["Code"].(string)
	if !ok {
		t.Fatalf("expected Filter[code] to be string, got %T (%v)", crit.Filter["Code"], crit.Filter["Code"])
	}
	if got != "95014" {
		t.Errorf("expected '95014' verbatim, got %q", got)
	}
}

func TestCoerce_StringLeafKeepsTrueAsString(t *testing.T) {
	// Another regression case: parseValue used to turn "true" into bool true.
	// On a string leaf the wire value must stay "true".
	crit, status := dispatchCoerce(t, "?code=true")
	if status != fiber.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	if crit.Filter["Code"] != "true" {
		t.Errorf("expected 'true' string, got %v (%T)", crit.Filter["Code"], crit.Filter["Code"])
	}
}

func TestCoerce_Int64LeafParsesDecimal(t *testing.T) {
	crit, status := dispatchCoerce(t, "?age=25")
	if status != fiber.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	if got, ok := crit.Filter["Age"].(int64); !ok || got != 25 {
		t.Errorf("expected int64(25), got %v (%T)", crit.Filter["Age"], crit.Filter["Age"])
	}
}

func TestCoerce_Int64LeafFallsBackToStringOnParseFailure(t *testing.T) {
	// Non-numeric value on an int leaf falls through as string (the
	// downstream query will return zero hits — the wrapper does not 400).
	// Documents the fail-soft behavior; the existing wrapper has always
	// degraded silently here.
	crit, status := dispatchCoerce(t, "?age=abc")
	if status != fiber.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	if crit.Filter["Age"] != "abc" {
		t.Errorf("expected string fallback 'abc', got %v (%T)", crit.Filter["Age"], crit.Filter["Age"])
	}
}

func TestCoerce_Float64LeafParsesDecimal(t *testing.T) {
	crit, status := dispatchCoerce(t, "?score=4.5")
	if status != fiber.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	if got, ok := crit.Filter["Score"].(float64); !ok || got != 4.5 {
		t.Errorf("expected float64(4.5), got %v (%T)", crit.Filter["Score"], crit.Filter["Score"])
	}
}

func TestCoerce_BoolLeafParsesTrueFalse(t *testing.T) {
	crit, status := dispatchCoerce(t, "?active=true")
	if status != fiber.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	if got, ok := crit.Filter["Active"].(bool); !ok || got != true {
		t.Errorf("expected bool(true), got %v (%T)", crit.Filter["Active"], crit.Filter["Active"])
	}
}

func TestCoerce_InListPreservesPerElementType(t *testing.T) {
	crit, status := dispatchCoerce(t, "?age.in=25,30,42")
	if status != fiber.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	cl, _ := crit.Filter["Age"].(queries.Clause)
	arr := cl.Values
	if cl.Op != queries.FilterIn || len(arr) != 3 {
		t.Fatalf("expected in-Clause of 3, got %#v", crit.Filter["Age"])
	}
	for i, want := range []int64{25, 30, 42} {
		if got, ok := arr[i].(int64); !ok || got != want {
			t.Errorf("element %d: expected int64(%d), got %v (%T)", i, want, arr[i], arr[i])
		}
	}
}

func TestCoerce_StringInListKeepsStringPerElement(t *testing.T) {
	// String-leaf `code.in=95014,SW1A2AA` — every element stays string,
	// including the digit-only "95014".
	crit, status := dispatchCoerce(t, "?code.in=95014,SW1A2AA")
	if status != fiber.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	cl, _ := crit.Filter["Code"].(queries.Clause)
	arr := cl.Values
	if cl.Op != queries.FilterIn || len(arr) != 2 {
		t.Fatalf("expected in-Clause of 2, got %#v", crit.Filter["Code"])
	}
	if arr[0] != "95014" {
		t.Errorf("first element should stay string '95014', got %v (%T)", arr[0], arr[0])
	}
	if arr[1] != "SW1A2AA" {
		t.Errorf("second element should stay string, got %v", arr[1])
	}
}
