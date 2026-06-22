package web

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/web/responses"
	"github.com/gofiber/fiber/v3"
)

// testOnlyTotalRequest opts into the full reserved-key set + a couple of
// filter leaves so the conflict matrix and the still-valid keys are both
// exercised. Reuses *testFindParamsQuery (from handle_query_test.go) so the
// fixtures stay small.
type testOnlyTotalRequest struct {
	Name  *string `query:"name"  filter:"eq,startswith"`
	Email *string `query:"email" filter:"eq,in"`

	Limit           *int64  `query:"limit"`
	After           *string `query:"after"`
	Before          *string `query:"before"`
	Sort            *string `query:"sort"`
	Fields          *string `query:"fields"`
	Search          *string `query:"search"`
	IncludeArchived *bool   `query:"includeArchived"`
	OnlyTotal       *bool   `query:"onlyTotal"`
}

func (r testOnlyTotalRequest) ToQuery(crit queries.ReadCriteria) *testFindParamsQuery {
	return &testFindParamsQuery{Criteria: crit}
}

// countOnlyHandler returns a Page already shaped for the count-only mode so
// the wrapper exercises the envelope branch end to end.
type countOnlyHandler struct {
	got *testFindParamsQuery
}

func (h *countOnlyHandler) Handle(ctx *configuration.AppContext, q *testFindParamsQuery) (queries.Page, error) {
	h.got = q
	_, _ = q.ToCriteria(ctx)
	if q.Criteria.OnlyTotal {
		return queries.Page{OnlyTotal: true, Total: 42}, nil
	}
	return queries.Page{
		Items: []map[string]any{{"id": "x"}},
		Total: 1,
	}, nil
}

// ─── envelope shape ───────────────────────────────────────────────────────

func TestOnlyTotal_EnvelopeOmitsDataAndListingFields(t *testing.T) {
	app := fiber.New()
	pipe := newTestPipeline()
	h := &countOnlyHandler{}

	app.Get("/users", HandleQueryWithParams(pipe, testOnlyTotalRequest{}, responses.RawDoc, h))

	resp, err := app.Test(httptest.NewRequest("GET", "/users?onlyTotal=true", nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d (body=%s)", resp.StatusCode, b)
	}
	body, _ := io.ReadAll(resp.Body)
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, body)
	}
	if parsed["success"] != true {
		t.Errorf("expected success=true, got %v", parsed["success"])
	}
	// data must NOT appear in the envelope — the consumer-stated rule
	// rejects forced zero-value fields.
	if _, present := parsed["data"]; present {
		t.Errorf("expected 'data' to be absent in count-only mode, got %v", parsed["data"])
	}
	pag, ok := parsed["pagination"].(map[string]any)
	if !ok {
		t.Fatalf("expected pagination object, got %T", parsed["pagination"])
	}
	if pag["total"] != float64(42) {
		t.Errorf("expected pagination.total=42, got %v", pag["total"])
	}
	// Listing-only fields must NOT appear — they would carry zero-value noise.
	for _, k := range []string{"has_next", "has_prev", "next_cursor", "prev_cursor"} {
		if _, present := pag[k]; present {
			t.Errorf("expected pagination.%s to be absent in count-only mode, got %v", k, pag[k])
		}
	}
}

func TestOnlyTotal_PropagatesIntoCriteria(t *testing.T) {
	app := fiber.New()
	pipe := newTestPipeline()
	h := &countOnlyHandler{}

	app.Get("/users", HandleQueryWithParams(pipe, testOnlyTotalRequest{}, responses.RawDoc, h))

	_, _ = app.Test(httptest.NewRequest("GET", "/users?onlyTotal=true", nil))
	if h.got == nil {
		t.Fatal("expected handler called")
	}
	if !h.got.Criteria.OnlyTotal {
		t.Error("expected Criteria.OnlyTotal=true to flow from wire")
	}
}

func TestOnlyTotal_FalseExplicitKeepsListingShape(t *testing.T) {
	// `?onlyTotal=false` is the same as omitting it — listing envelope stays.
	app := fiber.New()
	pipe := newTestPipeline()
	h := &countOnlyHandler{}

	app.Get("/users", HandleQueryWithParams(pipe, testOnlyTotalRequest{}, responses.RawDoc, h))

	resp, _ := app.Test(httptest.NewRequest("GET", "/users?onlyTotal=false", nil))
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var parsed map[string]any
	_ = json.Unmarshal(body, &parsed)
	if _, present := parsed["data"]; !present {
		t.Error("expected listing 'data' to be present when onlyTotal=false")
	}
	pag := parsed["pagination"].(map[string]any)
	if _, present := pag["has_next"]; !present {
		t.Error("expected listing pagination.has_next to be present when onlyTotal=false")
	}
}

// ─── conflict matrix ──────────────────────────────────────────────────────

func TestOnlyTotal_ConflictMatrixRejectsListingControls(t *testing.T) {
	cases := []struct {
		conflictKey string
		extra       string
	}{
		{"fields", "fields=name"},
		{"sort", "sort=-name"},
		{"limit", "limit=10"},
		{"after", "after=cur-xyz"},
		{"before", "before=cur-xyz"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.conflictKey, func(t *testing.T) {
			app := fiber.New()
			pipe := newTestPipeline()
			h := &countOnlyHandler{}
			app.Get("/users", HandleQueryWithParams(pipe, testOnlyTotalRequest{}, responses.RawDoc, h))

			url := "/users?onlyTotal=true&" + tc.extra
			resp, _ := app.Test(httptest.NewRequest("GET", url, nil))
			if resp.StatusCode != fiber.StatusBadRequest {
				b, _ := io.ReadAll(resp.Body)
				t.Fatalf("expected 400 for %s conflict, got %d (body=%s)", tc.conflictKey, resp.StatusCode, b)
			}
			if h.got != nil {
				t.Errorf("expected handler NOT called for %s conflict", tc.conflictKey)
			}
			body, _ := io.ReadAll(resp.Body)
			var parsed map[string]any
			_ = json.Unmarshal(body, &parsed)
			errs := parsed["errors"].([]any)
			msg := errs[0].(map[string]any)["messages"].([]any)[0].(map[string]any)
			want := "onlyTotal[" + tc.conflictKey + "]"
			if msg["field"] != want {
				t.Errorf("expected field=%q, got %v", want, msg["field"])
			}
		})
	}
}

// ─── still-valid keys ────────────────────────────────────────────────────

func TestOnlyTotal_PreservesFilterLeaves(t *testing.T) {
	app := fiber.New()
	pipe := newTestPipeline()
	h := &countOnlyHandler{}
	app.Get("/users", HandleQueryWithParams(pipe, testOnlyTotalRequest{}, responses.RawDoc, h))

	resp, _ := app.Test(httptest.NewRequest("GET", "/users?onlyTotal=true&name.startswith=Bo", nil))
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("expected 200 with filter leaf, got %d", resp.StatusCode)
	}
	if h.got == nil {
		t.Fatal("expected handler called")
	}
	if !h.got.Criteria.OnlyTotal {
		t.Error("expected OnlyTotal preserved alongside filter leaf")
	}
	// Filter leaf must reach the criteria.
	if _, has := h.got.Criteria.Filter["Name"]; !has {
		t.Errorf("expected filter['name'] populated, got %v", h.got.Criteria.Filter)
	}
}

func TestOnlyTotal_PreservesSearch(t *testing.T) {
	app := fiber.New()
	pipe := newTestPipeline()
	h := &countOnlyHandler{}
	app.Get("/users", HandleQueryWithParams(pipe, testOnlyTotalRequest{}, responses.RawDoc, h))

	resp, _ := app.Test(httptest.NewRequest("GET", "/users?onlyTotal=true&search=foo", nil))
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("expected 200 with search, got %d", resp.StatusCode)
	}
	if h.got == nil || h.got.Criteria.Search != "foo" {
		t.Errorf("expected Criteria.Search=foo, got %+v", h.got)
	}
}

func TestOnlyTotal_PreservesIncludeArchived(t *testing.T) {
	app := fiber.New()
	pipe := newTestPipeline()
	h := &countOnlyHandler{}
	app.Get("/users", HandleQueryWithParams(pipe, testOnlyTotalRequest{}, responses.RawDoc, h))

	resp, _ := app.Test(httptest.NewRequest("GET", "/users?onlyTotal=true&includeArchived=true", nil))
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("expected 200 with includeArchived, got %d", resp.StatusCode)
	}
	if h.got == nil || !h.got.Criteria.IncludeArchived {
		t.Errorf("expected Criteria.IncludeArchived=true, got %+v", h.got)
	}
}

// ─── opt-in gating ────────────────────────────────────────────────────────

// Without the OnlyTotal field declared, the wire key is unknown and the
// wrapper rejects it as a generic schema violation — same posture every
// reserved key follows (includeArchived/fields/sort all require opt-in).
func TestOnlyTotal_RejectsWhenDTODoesNotDeclareIt(t *testing.T) {
	app := fiber.New()
	pipe := newTestPipeline()
	h := &capturingParamsHandler{}
	// testFindParamsRequest (from handle_query_test.go) does NOT declare OnlyTotal.
	app.Get("/users", HandleQueryWithParams(pipe, testFindParamsRequest{}, responses.RawDoc, h))

	resp, _ := app.Test(httptest.NewRequest("GET", "/users?onlyTotal=true", nil))
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("expected 400 for undeclared onlyTotal, got %d", resp.StatusCode)
	}
	if h.got != nil {
		t.Error("expected handler NOT called when onlyTotal undeclared")
	}
}

// ─── ParseCriteria parity ────────────────────────────────────────────────

func TestOnlyTotal_ParseCriteriaPropagatesFlag(t *testing.T) {
	// Manual handlers via ParseCriteria get the same treatment — onlyTotal flows
	// into the criteria, conflicts are rejected with the same field shape.
	app := fiber.New()
	var got queries.ReadCriteria
	var gotBad string
	var gotOK bool
	app.Get("/x", func(c fiber.Ctx) error {
		got, gotBad, gotOK = ParseCriteria(c, testOnlyTotalRequest{})
		return c.SendStatus(fiber.StatusOK)
	})

	_, _ = app.Test(httptest.NewRequest("GET", "/x?onlyTotal=true&name=Jane", nil))
	if !gotOK || gotBad != "" {
		t.Fatalf("expected ok=true, got ok=%v bad=%q", gotOK, gotBad)
	}
	if !got.OnlyTotal {
		t.Error("expected OnlyTotal=true from ParseCriteria")
	}
}

func TestOnlyTotal_ParseCriteriaRejectsConflict(t *testing.T) {
	app := fiber.New()
	var gotBad string
	var gotOK bool
	app.Get("/x", func(c fiber.Ctx) error {
		_, gotBad, gotOK = ParseCriteria(c, testOnlyTotalRequest{})
		return c.SendStatus(fiber.StatusOK)
	})

	_, _ = app.Test(httptest.NewRequest("GET", "/x?onlyTotal=true&sort=-name", nil))
	if gotOK {
		t.Error("expected ok=false on onlyTotal + sort")
	}
	if gotBad != "onlyTotal[sort]" {
		t.Errorf("expected bad=onlyTotal[sort], got %q", gotBad)
	}
}

// ─── RespondPaged direct (Page → envelope branch) ─────────────────────────

func TestRespondPaged_OnlyTotalEmitsDedicatedShape(t *testing.T) {
	app := fiber.New()
	app.Get("/x", func(c fiber.Ctx) error {
		page := queries.Page{OnlyTotal: true, Total: 7}
		return RespondPaged(c, fiber.StatusOK, page, summaryFromDoc)
	})

	resp, _ := app.Test(httptest.NewRequest("GET", "/x", nil))
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, body)
	}
	if _, present := parsed["data"]; present {
		t.Errorf("expected 'data' absent in count-only envelope")
	}
	pag, ok := parsed["pagination"].(map[string]any)
	if !ok {
		t.Fatalf("expected pagination object, got %T", parsed["pagination"])
	}
	if len(pag) != 1 {
		t.Errorf("expected pagination to carry only 'total', got %d keys: %v", len(pag), pag)
	}
	if pag["total"] != float64(7) {
		t.Errorf("expected pagination.total=7, got %v", pag["total"])
	}
}
