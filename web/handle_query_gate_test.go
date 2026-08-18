package web

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/gofiber/fiber/v3"
)

// bareGateRequest declares only filter leaves — no reserved control keys.
// The DTO is the single source of truth for what the endpoint exposes: every
// reserved control arriving on the wire must reject as the canonical
// NotDeclared violation (400, field = the control key).
type bareGateRequest struct {
	Name *string `query:"name" filter:"eq,startswith"`
}

func (r bareGateRequest) ToQuery(crit queries.ReadCriteria) *testFindParamsQuery {
	return &testFindParamsQuery{Criteria: crit}
}

func gateApp(h *onlyTotalHandler) *fiber.App {
	app := fiber.New()
	pipe := newTestPipeline()
	app.Get("/users", QueryWithParams(pipe, bareGateRequest{}, rawItem, h))
	return app
}

func TestReservedGate_UndeclaredControlKeysReject(t *testing.T) {
	cases := []struct {
		key   string
		query string
	}{
		{"first", "first=5"},
		{"last", "last=5"},
		{"after", "after=abc"},
		{"before", "before=abc"},
		{"orderBy", "orderBy=-name"},
		{"fields", "fields=name"},
		{"search", "search=x"},
		{"includeArchived", "includeArchived=true"},
		{"includeArchived", "includeArchived=false"},
		{"onlyTotal", "onlyTotal=true"},
		// Presence gates regardless of value — `?onlyTotal=false` on the
		// wire still needs the DTO opt-in, exactly like includeArchived.
		{"onlyTotal", "onlyTotal=false"},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			h := &onlyTotalHandler{}
			app := gateApp(h)
			resp, _ := app.Test(httptest.NewRequest("GET", "/users?"+tc.query, nil))
			if resp.StatusCode != fiber.StatusBadRequest {
				b, _ := io.ReadAll(resp.Body)
				t.Fatalf("undeclared %s must 400, got %d (body=%s)", tc.key, resp.StatusCode, b)
			}
			if h.got != nil {
				t.Fatalf("handler must not run for undeclared %s", tc.key)
			}
			body, _ := io.ReadAll(resp.Body)
			var parsed map[string]any
			_ = json.Unmarshal(body, &parsed)
			errs := parsed["errors"].([]any)
			msg := errs[0].(map[string]any)["messages"].([]any)[0].(map[string]any)
			if msg["field"] != tc.key {
				t.Fatalf("expected field=%q, got %v", tc.key, msg["field"])
			}
		})
	}

	// Filter leaves stay untouched by the gate.
	h := &onlyTotalHandler{}
	app := gateApp(h)
	resp, _ := app.Test(httptest.NewRequest("GET", "/users?name.startswith=Bo", nil))
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("filter-only request must pass the gate, got %d", resp.StatusCode)
	}
}

func TestReservedGate_DirectionPairREST(t *testing.T) {
	// last alone = backward window from the end.
	h := &onlyTotalHandler{}
	app := fiber.New()
	pipe := newTestPipeline()
	app.Get("/users", QueryWithParams(pipe, testOnlyTotalRequest{}, rawItem, h))

	resp, _ := app.Test(httptest.NewRequest("GET", "/users?last=3", nil))
	if resp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("last alone must pass, got %d (body=%s)", resp.StatusCode, b)
	}
	if h.got == nil || h.got.Criteria.Limit != 3 || !h.got.Criteria.Backward {
		t.Fatalf("last=3 must map to Limit=3 Backward=true, got %+v", h.got)
	}

	// forward+backward mixes reject on the backward-side key.
	for _, tc := range []struct {
		query string
		want  string
	}{
		{"first=2&last=3", "last"},
		{"first=2&before=abc", "before"},
		{"last=2&after=abc", "last"},
		{"after=abc&before=abc", "before"},
	} {
		h := &onlyTotalHandler{}
		app := fiber.New()
		app.Get("/users", QueryWithParams(newTestPipeline(), testOnlyTotalRequest{}, rawItem, h))
		resp, _ := app.Test(httptest.NewRequest("GET", "/users?"+tc.query, nil))
		if resp.StatusCode != fiber.StatusBadRequest {
			t.Fatalf("%s must 400, got %d", tc.query, resp.StatusCode)
		}
		body, _ := io.ReadAll(resp.Body)
		var parsed map[string]any
		_ = json.Unmarshal(body, &parsed)
		errs := parsed["errors"].([]any)
		msg := errs[0].(map[string]any)["messages"].([]any)[0].(map[string]any)
		if msg["field"] != tc.want {
			t.Fatalf("%s: expected field=%q, got %v", tc.query, tc.want, msg["field"])
		}
	}

	// Non-positive sizes reject on the size key.
	for _, q := range []string{"first=0", "last=-1"} {
		app := fiber.New()
		app.Get("/users", QueryWithParams(newTestPipeline(), testOnlyTotalRequest{}, rawItem, &onlyTotalHandler{}))
		resp, _ := app.Test(httptest.NewRequest("GET", "/users?"+q, nil))
		if resp.StatusCode != fiber.StatusBadRequest {
			t.Fatalf("%s must 400, got %d", q, resp.StatusCode)
		}
	}
}

// TestReservedGate_OnlyTotalPresenceVsActivation proves the two halves of
// the presence/activation split on a DECLARED endpoint: `?onlyTotal=false`
// alongside page-shaping controls is a plain paged read (no conflict-matrix
// 400, no count short-circuit), while `?onlyTotal=true` with the same
// controls still trips the matrix.
func TestReservedGate_OnlyTotalPresenceVsActivation(t *testing.T) {
	h := &onlyTotalHandler{}
	app := fiber.New()
	app.Get("/users", QueryWithParams(newTestPipeline(), testOnlyTotalRequest{}, rawItem, h))

	resp, _ := app.Test(httptest.NewRequest("GET", "/users?onlyTotal=false&first=5&orderBy=name", nil))
	if resp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("inactive onlyTotal with paging must pass, got %d (body=%s)", resp.StatusCode, b)
	}
	if h.got == nil || h.got.Criteria.OnlyTotal || h.got.Criteria.Limit != 5 {
		t.Fatalf("inactive onlyTotal must run the plain paged read, got %+v", h.got)
	}

	resp, _ = app.Test(httptest.NewRequest("GET", "/users?onlyTotal=true&first=5", nil))
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("active onlyTotal with paging must keep the conflict 400, got %d", resp.StatusCode)
	}
}
