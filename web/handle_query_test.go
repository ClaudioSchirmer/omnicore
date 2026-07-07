package web

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/web/responses"
	"github.com/gofiber/fiber/v3"
)

// testFindParamsRequest declares an allowlist for the params endpoint:
// name accepts only equality; email accepts equality + in; the remaining
// fields are reserved pagination/control keys recognized by the framework.
type testFindParamsRequest struct {
	Name  *string `query:"name"  filter:"eq"`
	Email *string `query:"email" filter:"eq,in"`

	Limit           *int64  `query:"limit"`
	After           *string `query:"after"`
	Sort            *string `query:"sort"`
	Fields          *string `query:"fields"`
	Search          *string `query:"search"`
	IncludeArchived *bool   `query:"includeArchived"`
}

// testFindParamsQuery is the Query produced by ToQuery. ToCriteria(ctx) is the
// only layer that consumes *AppContext on the read side, so SeenLang is
// captured there (mirroring the production pattern where identity-derived
// overlays — tenant id etc. — layer onto the criteria inside ToCriteria).
type testFindParamsQuery struct {
	pipeline.QueryBase
	Criteria queries.ReadCriteria
	SeenLang configuration.Language
}

func (q *testFindParamsQuery) ToCriteria(ctx *configuration.AppContext) (queries.ReadCriteria, error) {
	q.SeenLang = ctx.Language()
	return q.Criteria, nil
}

func (r testFindParamsRequest) ToQuery(crit queries.ReadCriteria) *testFindParamsQuery {
	return &testFindParamsQuery{Criteria: crit}
}

// capturingParamsHandler records the Query received and invokes ToCriteria so
// the SeenLang assertion still proves ctx propagation end to end.
type capturingParamsHandler struct {
	got *testFindParamsQuery
}

func (h *capturingParamsHandler) Handle(ctx *configuration.AppContext, q *testFindParamsQuery) (queries.Page, error) {
	h.got = q
	_, _ = q.ToCriteria(ctx)
	return queries.Page{
		Items:   []map[string]any{{"id": "abc"}},
		HasNext: true,
		Total:   1,
	}, nil
}

func TestHandleQueryWithParams_UnknownFieldReturns400(t *testing.T) {
	app := fiber.New()
	pipe := newTestPipeline()
	h := &capturingParamsHandler{}

	app.Get("/users", QueryWithParams(pipe, testFindParamsRequest{}, responses.RawDoc, h))

	resp, err := app.Test(httptest.NewRequest("GET", "/users?role=admin", nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != fiber.StatusBadRequest {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 400, got %d (body=%s)", resp.StatusCode, b)
	}
	if h.got != nil {
		t.Error("expected handler NOT to be called when allowlist rejects the request")
	}
}

func TestHandleQueryWithParams_OperatorOutsideDeclaredListReturns400(t *testing.T) {
	app := fiber.New()
	pipe := newTestPipeline()
	h := &capturingParamsHandler{}

	app.Get("/users", QueryWithParams(pipe, testFindParamsRequest{}, responses.RawDoc, h))

	// name has `filter:"eq"` only — using .in must 400.
	resp, _ := app.Test(httptest.NewRequest("GET", "/users?name.in=Jane,Mary", nil))
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("expected 400 for unknown operator, got %d", resp.StatusCode)
	}
	if h.got != nil {
		t.Error("expected handler NOT to be called when operator gate rejects the request")
	}
}

func TestHandleQueryWithParams_AllowedOperatorAssemblesCriteria(t *testing.T) {
	app := fiber.New()
	pipe := newTestPipeline()
	h := &capturingParamsHandler{}

	app.Get("/users", QueryWithParams(pipe, testFindParamsRequest{}, responses.RawDoc, h))

	resp, _ := app.Test(httptest.NewRequest("GET", "/users?email.in=a@x.com,b@y.com&limit=20&sort=-name", nil))
	if resp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d (body=%s)", resp.StatusCode, b)
	}
	if h.got == nil {
		t.Fatal("expected handler to be called")
	}
	emailFilter, ok := h.got.Criteria.Filter["Email"].(map[string]any)
	if !ok {
		t.Fatalf("expected email filter to be a map, got %T", h.got.Criteria.Filter["Email"])
	}
	in, ok := emailFilter["$in"].([]any)
	if !ok || len(in) != 2 || in[0] != "a@x.com" || in[1] != "b@y.com" {
		t.Errorf("expected $in=[a@x.com, b@y.com], got %v", emailFilter["$in"])
	}
	if h.got.Criteria.Limit != 20 {
		t.Errorf("expected Limit=20, got %d", h.got.Criteria.Limit)
	}
	if len(h.got.Criteria.Sort) != 1 || h.got.Criteria.Sort[0].Field != "name" || !h.got.Criteria.Sort[0].Desc {
		t.Errorf("expected Sort=[-name desc], got %v", h.got.Criteria.Sort)
	}
}

func TestHandleQueryWithParams_EmptyQueryStringStillCallsHandler(t *testing.T) {
	app := fiber.New()
	pipe := newTestPipeline()
	h := &capturingParamsHandler{}

	app.Get("/users", QueryWithParams(pipe, testFindParamsRequest{}, responses.RawDoc, h))

	resp, _ := app.Test(httptest.NewRequest("GET", "/users", nil))
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if h.got == nil {
		t.Fatal("expected handler called even with empty query string")
	}
	if len(h.got.Criteria.Filter) != 0 {
		t.Errorf("expected empty Filter, got %v", h.got.Criteria.Filter)
	}
}

func TestHandleQueryWithParams_AppContextFlowsIntoToCriteria(t *testing.T) {
	app := fiber.New()
	app.Use(AppContextMiddleware())
	pipe := newTestPipeline()
	h := &capturingParamsHandler{}

	app.Get("/users", QueryWithParams(pipe, testFindParamsRequest{}, responses.RawDoc, h))

	req := httptest.NewRequest("GET", "/users", nil)
	req.Header.Set("Accept-Language", "en")
	resp, _ := app.Test(req)
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if h.got.SeenLang != configuration.LangENG {
		t.Errorf("expected ToCriteria to see LangENG from AppContext, got %v", h.got.SeenLang)
	}
}

func TestHandleQueryWithParams_ResponseEnvelopeShape(t *testing.T) {
	app := fiber.New()
	pipe := newTestPipeline()
	h := &capturingParamsHandler{}

	app.Get("/users", QueryWithParams(pipe, testFindParamsRequest{}, responses.RawDoc, h))

	resp, _ := app.Test(httptest.NewRequest("GET", "/users", nil))
	body, _ := io.ReadAll(resp.Body)
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("unmarshal body: %v (body=%s)", err, body)
	}
	if parsed["success"] != true {
		t.Errorf("expected success=true, got %v", parsed["success"])
	}
	// data is the items array (NOT a wrapper object)
	if _, ok := parsed["data"].([]any); !ok {
		t.Errorf("expected data to be an array, got %T (%v)", parsed["data"], parsed["data"])
	}
	pag, ok := parsed["pagination"].(map[string]any)
	if !ok {
		t.Fatalf("expected pagination to be an object, got %T", parsed["pagination"])
	}
	if pag["has_next"] != true {
		t.Errorf("expected pagination.has_next=true, got %v", pag["has_next"])
	}
	if pag["total"] != float64(1) {
		t.Errorf("expected pagination.total=1, got %v", pag["total"])
	}
}

// TestHandleQueryWithParams_CustomProjectorReshapesData proves the projector
// parameter is honored — the wire data array carries the typed Response shape
// the consumer declared, not the raw view doc.
//
// testUserSummary follows the sparse-render contract enforced by the boot
// guard whenever a Request DTO declares `query:"fields"` and is consumed
// by QueryWithParams with a struct Response: every field is *T +
// ,omitempty so encoding/json elides absent values cleanly.
type testUserSummary struct {
	ID   *string `json:"id,omitempty"`
	Name *string `json:"name,omitempty"`
}

// summaryFromDoc is the projector used by every test that pairs a Page
// with the testUserSummary shape. Returns a struct whose pointer fields
// carry the doc value when present.
func summaryFromDoc(doc map[string]any) testUserSummary {
	id := docStringField(doc, "id")
	name := docStringField(doc, "name")
	return testUserSummary{ID: &id, Name: &name}
}

// fixedParamsHandler returns a hardcoded Page regardless of the incoming Query.
type fixedParamsHandler struct {
	page queries.Page
}

func (h *fixedParamsHandler) Handle(_ *configuration.AppContext, _ *testFindParamsQuery) (queries.Page, error) {
	return h.page, nil
}

func TestHandleQueryWithParams_CustomProjectorReshapesData(t *testing.T) {
	app := fiber.New()
	pipe := newTestPipeline()
	// Handler returns docs with id + email; the projector keeps only id + name
	// (email absent in the output — exercises that the projector controls the wire shape).
	h := &fixedParamsHandler{page: queries.Page{
		Items: []map[string]any{
			{"id": "u1", "name": "Alice", "email": "a@x.com"},
			{"id": "u2", "name": "Bob"},
		},
		Total: 2,
	}}

	app.Get("/users", QueryWithParams(pipe, testFindParamsRequest{}, summaryFromDoc, h))

	resp, _ := app.Test(httptest.NewRequest("GET", "/users", nil))
	body, _ := io.ReadAll(resp.Body)
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, body)
	}
	items, ok := parsed["data"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("expected data array of len 2, got %T (%v)", parsed["data"], parsed["data"])
	}
	first, _ := items[0].(map[string]any)
	if first["id"] != "u1" || first["name"] != "Alice" {
		t.Errorf("first item mismatch: %v", first)
	}
	if _, leaked := first["email"]; leaked {
		t.Errorf("projector should have stripped 'email' from wire shape, got: %v", first)
	}
}

// --- by-id wrapper ---

type testFindIDRequest struct {
	IncludeArchived *bool `query:"includeArchived"`
}

type testFindIDQuery struct {
	queries.QueryByIDBase
	IncludeArchived bool
	SeenLang        configuration.Language
}

func (q *testFindIDQuery) ToCriteria(ctx *configuration.AppContext) (queries.ReadCriteria, error) {
	q.SeenLang = ctx.Language()
	return queries.ReadCriteria{IncludeArchived: q.IncludeArchived}, nil
}
func (q *testFindIDQuery) ContextName() string { return "" }

func (r testFindIDRequest) ToQuery() *testFindIDQuery {
	arch := false
	if r.IncludeArchived != nil {
		arch = *r.IncludeArchived
	}
	return &testFindIDQuery{IncludeArchived: arch}
}

type capturingIDHandler struct {
	got *testFindIDQuery
}

func (h *capturingIDHandler) Handle(ctx *configuration.AppContext, q *testFindIDQuery) (map[string]any, error) {
	h.got = q
	_, _ = q.ToCriteria(ctx)
	return map[string]any{"id": q.PathID().String()}, nil
}

func TestHandleQueryByID_AcceptsIncludeArchivedParam(t *testing.T) {
	app := fiber.New()
	pipe := newTestPipeline()
	h := &capturingIDHandler{}

	app.Get("/users/:id", QueryByID(pipe, testFindIDRequest{}, responses.RawDoc, h))

	resp, _ := app.Test(httptest.NewRequest("GET", "/users/abc?includeArchived=true", nil))
	if resp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d (body=%s)", resp.StatusCode, b)
	}
	if h.got == nil {
		t.Fatal("expected handler called")
	}
	if !h.got.IncludeArchived {
		t.Error("expected IncludeArchived=true to flow from ?includeArchived=true")
	}
	if h.got.PathID().Value() != "abc" {
		t.Errorf("expected PathID()='abc', got %q", h.got.PathID().Value())
	}
}

func TestHandleQueryByID_RejectsExtraParamWith400(t *testing.T) {
	app := fiber.New()
	pipe := newTestPipeline()
	h := &capturingIDHandler{}

	app.Get("/users/:id", QueryByID(pipe, testFindIDRequest{}, responses.RawDoc, h))

	resp, _ := app.Test(httptest.NewRequest("GET", "/users/abc?role=admin", nil))
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("expected 400 for extra query param, got %d", resp.StatusCode)
	}
	if h.got != nil {
		t.Error("expected handler NOT to be called when extra param is rejected")
	}
}

func TestHandleQueryByID_NoQueryStringDefaults(t *testing.T) {
	app := fiber.New()
	pipe := newTestPipeline()
	h := &capturingIDHandler{}

	app.Get("/users/:id", QueryByID(pipe, testFindIDRequest{}, responses.RawDoc, h))

	resp, _ := app.Test(httptest.NewRequest("GET", "/users/abc", nil))
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if h.got == nil || h.got.IncludeArchived {
		t.Errorf("expected IncludeArchived=false by default")
	}
}

func TestHandleQueryByID_AppContextFlowsIntoToCriteria(t *testing.T) {
	app := fiber.New()
	app.Use(AppContextMiddleware())
	pipe := newTestPipeline()
	h := &capturingIDHandler{}

	app.Get("/users/:id", QueryByID(pipe, testFindIDRequest{}, responses.RawDoc, h))

	req := httptest.NewRequest("GET", "/users/abc", nil)
	req.Header.Set("Accept-Language", "fr")
	resp, _ := app.Test(req)
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if h.got.SeenLang != configuration.LangFR {
		t.Errorf("expected ToCriteria to see LangFR from AppContext, got %v", h.got.SeenLang)
	}
}

// idDocHandler returns a fixed doc with the path id injected.
type idDocHandler struct{}

func (h *idDocHandler) Handle(_ *configuration.AppContext, q *testFindIDQuery) (map[string]any, error) {
	return map[string]any{"id": q.PathID().String(), "name": "Carol", "email": "c@x.com"}, nil
}

// TestHandleQueryByID_CustomProjectorReshapesData proves the projector
// applies on the by-id success path — the wire data carries the typed shape,
// not the raw doc.
func TestHandleQueryByID_CustomProjectorReshapesData(t *testing.T) {
	app := fiber.New()
	pipe := newTestPipeline()
	h := &idDocHandler{}

	app.Get("/users/:id", QueryByID(pipe, testFindIDRequest{}, summaryFromDoc, h))

	resp, _ := app.Test(httptest.NewRequest("GET", "/users/abc", nil))
	body, _ := io.ReadAll(resp.Body)
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, body)
	}
	data, ok := parsed["data"].(map[string]any)
	if !ok {
		t.Fatalf("expected data object, got %T", parsed["data"])
	}
	if data["id"] != "abc" || data["name"] != "Carol" {
		t.Errorf("data mismatch: %v", data)
	}
	if _, leaked := data["email"]; leaked {
		t.Errorf("projector should have stripped 'email' from wire shape, got: %v", data)
	}
}

// --- ParseCriteria / RespondSchemaViolation ---

func TestParseCriteria_HappyPath(t *testing.T) {
	app := fiber.New()
	var got queries.ReadCriteria
	var gotBad string
	var gotOK bool
	app.Get("/x", func(c fiber.Ctx) error {
		got, gotBad, gotOK = ParseCriteria(c, testFindParamsRequest{})
		return c.SendStatus(fiber.StatusOK)
	})

	resp, _ := app.Test(httptest.NewRequest("GET", "/x?name=Jane&limit=5", nil))
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if !gotOK || gotBad != "" {
		t.Errorf("expected ok=true, badField empty; got ok=%v, badField=%q", gotOK, gotBad)
	}
	if got.Filter["Name"] != "Jane" {
		t.Errorf("expected filter name=Jane, got %v", got.Filter["Name"])
	}
	if got.Limit != 5 {
		t.Errorf("expected limit=5, got %d", got.Limit)
	}
}

func TestParseCriteria_UnknownKey(t *testing.T) {
	app := fiber.New()
	var gotBad string
	var gotOK bool
	app.Get("/x", func(c fiber.Ctx) error {
		_, gotBad, gotOK = ParseCriteria(c, testFindParamsRequest{})
		return c.SendStatus(fiber.StatusOK)
	})

	_, _ = app.Test(httptest.NewRequest("GET", "/x?role=admin", nil))
	if gotOK {
		t.Error("expected ok=false for unknown key")
	}
	if gotBad != "role" {
		t.Errorf("expected badField=role, got %q", gotBad)
	}
}

func TestParseCriteria_BadOperator(t *testing.T) {
	app := fiber.New()
	var gotBad string
	var gotOK bool
	app.Get("/x", func(c fiber.Ctx) error {
		_, gotBad, gotOK = ParseCriteria(c, testFindParamsRequest{})
		return c.SendStatus(fiber.StatusOK)
	})

	// name has filter:"eq" only — .in is disallowed
	_, _ = app.Test(httptest.NewRequest("GET", "/x?name.in=a,b", nil))
	if gotOK {
		t.Error("expected ok=false for disallowed operator")
	}
	if gotBad != "name.in" {
		t.Errorf("expected badField=name.in, got %q", gotBad)
	}
}

func TestRespondSchemaViolation_EmitsCanonical400(t *testing.T) {
	app := fiber.New()
	pipe := newTestPipeline()
	app.Get("/x", func(c fiber.Ctx) error {
		return RespondSchemaViolation(c, pipe, "tenant")
	})

	resp, _ := app.Test(httptest.NewRequest("GET", "/x", nil))
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, body)
	}
	errs, ok := parsed["errors"].([]any)
	if !ok || len(errs) == 0 {
		t.Fatalf("expected errors array, got %v", parsed["errors"])
	}
	first, _ := errs[0].(map[string]any)
	if first["context"] != "Schema" {
		t.Errorf("expected context=Schema, got %v", first["context"])
	}
	msgs, _ := first["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatalf("expected at least one message")
	}
	msg, _ := msgs[0].(map[string]any)
	if msg["notificationKey"] != "SchemaViolationNotification" {
		t.Errorf("expected notificationKey=SchemaViolationNotification, got %v", msg["notificationKey"])
	}
	if msg["field"] != "tenant" {
		t.Errorf("expected field=tenant, got %v", msg["field"])
	}
}

// --- ProjectPage / RespondPaged ---

func TestProjectPage_EmptyItems(t *testing.T) {
	items, pag := ProjectPage(queries.Page{}, responses.RawDoc)
	if len(items) != 0 {
		t.Errorf("expected empty items, got %d", len(items))
	}
	if pag == nil {
		t.Fatal("expected pagination to be non-nil even on empty page")
	}
}

func TestProjectPage_AppliesProjectorPerDoc(t *testing.T) {
	page := queries.Page{
		Items: []map[string]any{
			{"id": "1", "name": "A"},
			{"id": "2", "name": "B"},
		},
		HasNext:    true,
		NextCursor: "cur-xyz",
		Total:      2,
	}
	items, pag := ProjectPage(page, summaryFromDoc)
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	if items[0].ID == nil || *items[0].ID != "1" || items[0].Name == nil || *items[0].Name != "A" {
		t.Errorf("first item mismatch: %+v", items[0])
	}
	if !pag.HasNext || pag.NextCursor != "cur-xyz" || pag.Total != 2 {
		t.Errorf("pagination mismatch: %+v", pag)
	}
}

func TestRespondPaged_EmitsEnvelope(t *testing.T) {
	app := fiber.New()
	app.Get("/x", func(c fiber.Ctx) error {
		page := queries.Page{
			Items:   []map[string]any{{"id": "1", "name": "A"}},
			HasNext: false,
			Total:   1,
		}
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
	if parsed["success"] != true {
		t.Errorf("expected success=true, got %v", parsed["success"])
	}
	items, _ := parsed["data"].([]any)
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	first, _ := items[0].(map[string]any)
	if first["id"] != "1" || first["name"] != "A" {
		t.Errorf("item shape mismatch: %v", first)
	}
	if _, hasPag := parsed["pagination"].(map[string]any); !hasPag {
		t.Errorf("expected pagination block in envelope")
	}
}

// --- RawDoc identity projector ---

func TestRawDoc_IsIdentity(t *testing.T) {
	in := map[string]any{"id": "x", "n": 1}
	out := responses.RawDoc(in)
	if &out == &in {
		// We don't assert pointer equality (Go maps are reference types — same
		// header passed back), but ensure no key was stripped or added.
	}
	if len(out) != 2 || out["id"] != "x" || out["n"] != 1 {
		t.Errorf("expected identity output, got %v", out)
	}
}

// docStringField is a tiny helper used by the projector test fixtures —
// the consumer is expected to ship its own equivalent in web/responses/*.
func docStringField(doc map[string]any, key string) string {
	if v, ok := doc[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// ─── runtime ?sort= behavior ───────────────────────────────────────────────
//
// The reserved `sort` key is symmetric to `fields`: the Request DTO opts in
// by declaring `Sort *string query:"sort"`; when the wrapper is paired with
// a typed Response struct, each comma-separated token is validated against
// the Response's declared wire paths and translated to the doc-side path.
// Manual handlers via ParseCriteria (or wrappers paired with a RawDoc-style
// projector) get pass-through behavior — the legacy contract before the
// allowlist landed.

func TestSortParam_UnknownTokenReturns400WithBracketedField(t *testing.T) {
	app := fiber.New()
	pipe := newTestPipeline()
	h := &capturingParamsHandler{}
	app.Get("/users", QueryWithParams(pipe, testFindParamsRequest{}, func(_ map[string]any) sparseUser {
		return sparseUser{}
	}, h))

	resp, _ := app.Test(httptest.NewRequest("GET", "/users?sort=bogus", nil))
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, body)
	}
	errs := parsed["errors"].([]any)
	first := errs[0].(map[string]any)
	msg := first["messages"].([]any)[0].(map[string]any)
	if got := msg["field"]; got != "sort[bogus]" {
		t.Errorf("expected field=sort[bogus], got %v", got)
	}
}

func TestSortParam_UnknownTokenWithMinusPrefixPreservesPrefixInError(t *testing.T) {
	// The minus sign is part of the wire token the consumer sent — preserve
	// it verbatim in the 400 envelope so the rejection traces back to the
	// exact query-string fragment.
	app := fiber.New()
	pipe := newTestPipeline()
	h := &capturingParamsHandler{}
	app.Get("/users", QueryWithParams(pipe, testFindParamsRequest{}, func(_ map[string]any) sparseUser {
		return sparseUser{}
	}, h))

	resp, _ := app.Test(httptest.NewRequest("GET", "/users?sort=-bogus", nil))
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, body)
	}
	errs := parsed["errors"].([]any)
	first := errs[0].(map[string]any)
	msg := first["messages"].([]any)[0].(map[string]any)
	if got := msg["field"]; got != "sort[-bogus]" {
		t.Errorf("expected field=sort[-bogus] (prefix preserved), got %v", got)
	}
}

func TestSortParam_KnownTokenTranslatesToDocPath(t *testing.T) {
	app := fiber.New()
	pipe := newTestPipeline()
	h := &capturingParamsHandler{}
	app.Get("/users", QueryWithParams(pipe, testFindParamsRequest{}, func(_ map[string]any) sparseUser {
		return sparseUser{}
	}, h))

	_, _ = app.Test(httptest.NewRequest("GET", "/users?sort=addresses.zipCode", nil))
	if h.got == nil {
		t.Fatal("expected handler called")
	}
	if len(h.got.Criteria.Sort) != 1 {
		t.Fatalf("expected 1 SortField, got %d", len(h.got.Criteria.Sort))
	}
	if h.got.Criteria.Sort[0].Field != "Addresses.ZipCode" {
		t.Errorf("expected Field=addresses.zip_code (PascalToSnake), got %q", h.got.Criteria.Sort[0].Field)
	}
	if h.got.Criteria.Sort[0].Desc {
		t.Errorf("expected Desc=false on bare token, got true")
	}
}

func TestSortParam_NestedViewTagOverride(t *testing.T) {
	app := fiber.New()
	pipe := newTestPipeline()
	h := &capturingParamsHandler{}
	app.Get("/users", QueryWithParams(pipe, testFindParamsRequest{}, func(_ map[string]any) sparseUser {
		return sparseUser{}
	}, h))

	_, _ = app.Test(httptest.NewRequest("GET", "/users?sort=addresses.state", nil))
	if h.got == nil {
		t.Fatal("expected handler called")
	}
	if len(h.got.Criteria.Sort) != 1 {
		t.Fatalf("expected 1 SortField, got %d", len(h.got.Criteria.Sort))
	}
	if h.got.Criteria.Sort[0].Field != "Addresses.State" {
		t.Errorf("expected Field=addresses.st (view: override), got %q", h.got.Criteria.Sort[0].Field)
	}
}

func TestSortParam_MinusPrefixSetsDesc(t *testing.T) {
	app := fiber.New()
	pipe := newTestPipeline()
	h := &capturingParamsHandler{}
	app.Get("/users", QueryWithParams(pipe, testFindParamsRequest{}, func(_ map[string]any) sparseUser {
		return sparseUser{}
	}, h))

	_, _ = app.Test(httptest.NewRequest("GET", "/users?sort=-name", nil))
	if h.got == nil {
		t.Fatal("expected handler called")
	}
	if len(h.got.Criteria.Sort) != 1 {
		t.Fatalf("expected 1 SortField, got %d", len(h.got.Criteria.Sort))
	}
	if h.got.Criteria.Sort[0].Field != "Name" || !h.got.Criteria.Sort[0].Desc {
		t.Errorf("expected Field=Name,Desc=true, got %+v", h.got.Criteria.Sort[0])
	}
}

func TestSortParam_MultipleTokensIndependentDirections(t *testing.T) {
	app := fiber.New()
	pipe := newTestPipeline()
	h := &capturingParamsHandler{}
	app.Get("/users", QueryWithParams(pipe, testFindParamsRequest{}, func(_ map[string]any) sparseUser {
		return sparseUser{}
	}, h))

	_, _ = app.Test(httptest.NewRequest("GET", "/users?sort=name,-email", nil))
	if h.got == nil {
		t.Fatal("expected handler called")
	}
	if len(h.got.Criteria.Sort) != 2 {
		t.Fatalf("expected 2 SortFields, got %d", len(h.got.Criteria.Sort))
	}
	if h.got.Criteria.Sort[0].Field != "Name" || h.got.Criteria.Sort[0].Desc {
		t.Errorf("expected first SortField=name asc, got %+v", h.got.Criteria.Sort[0])
	}
	if h.got.Criteria.Sort[1].Field != "Email" || !h.got.Criteria.Sort[1].Desc {
		t.Errorf("expected second SortField=email desc, got %+v", h.got.Criteria.Sort[1])
	}
}

func TestSortParam_PassThroughModeOnParseCriteria(t *testing.T) {
	// ParseCriteria passes nil projSchema → no allowlist, no translation,
	// each token becomes a SortField verbatim (the manual handler knows the
	// doc shape and assembled its own wire→doc mapping upstream).
	app := fiber.New()
	var got queries.ReadCriteria
	app.Get("/x", func(c fiber.Ctx) error {
		got, _, _ = ParseCriteria(c, testFindParamsRequest{})
		return c.SendStatus(fiber.StatusOK)
	})
	_, _ = app.Test(httptest.NewRequest("GET", "/x?sort=-anything,foo.bar", nil))
	if len(got.Sort) != 2 {
		t.Fatalf("expected 2 SortFields in pass-through mode, got %d", len(got.Sort))
	}
	if got.Sort[0].Field != "anything" || !got.Sort[0].Desc {
		t.Errorf("expected first SortField=anything desc (verbatim), got %+v", got.Sort[0])
	}
	if got.Sort[1].Field != "foo.bar" || got.Sort[1].Desc {
		t.Errorf("expected second SortField=foo.bar asc (verbatim), got %+v", got.Sort[1])
	}
}

// testFindSortOnlyRequest opts into sort but NOT into fields — proves the
// projSchema is built (and the allowlist fires) when sort is the only
// reserved key requesting Response-side validation.
type testFindSortOnlyRequest struct {
	Name *string `query:"name" filter:"eq"`
	Sort *string `query:"sort"`
}

func (r testFindSortOnlyRequest) ToQuery(crit queries.ReadCriteria) *testFindParamsQuery {
	return &testFindParamsQuery{Criteria: crit}
}

func TestSortParam_OptInWithoutFieldsBuildsProjSchema(t *testing.T) {
	app := fiber.New()
	pipe := newTestPipeline()
	h := &capturingParamsHandler{}
	app.Get("/users", QueryWithParams(pipe, testFindSortOnlyRequest{}, func(_ map[string]any) sparseUser {
		return sparseUser{}
	}, h))

	// Allowlist must fire even though the DTO did not declare `Fields`.
	resp, _ := app.Test(httptest.NewRequest("GET", "/users?sort=bogus", nil))
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("expected 400 from sort allowlist (sort opt-in alone), got %d", resp.StatusCode)
	}

	// Valid token must translate via PascalToSnake.
	h2 := &capturingParamsHandler{}
	app2 := fiber.New()
	app2.Get("/users", QueryWithParams(pipe, testFindSortOnlyRequest{}, func(_ map[string]any) sparseUser {
		return sparseUser{}
	}, h2))
	_, _ = app2.Test(httptest.NewRequest("GET", "/users?sort=-addresses.zipCode", nil))
	if h2.got == nil {
		t.Fatal("expected handler called")
	}
	if len(h2.got.Criteria.Sort) != 1 ||
		h2.got.Criteria.Sort[0].Field != "Addresses.ZipCode" ||
		!h2.got.Criteria.Sort[0].Desc {
		t.Errorf("expected SortField=addresses.zip_code desc, got %+v", h2.got.Criteria.Sort)
	}
}

func TestSortParam_RawDocResponseFallsBackToPassThrough(t *testing.T) {
	// R = map[string]any (responses.RawDoc) → projSchema stays nil → tokens
	// land verbatim with no allowlist enforcement (same fallback fields uses).
	app := fiber.New()
	pipe := newTestPipeline()
	h := &capturingParamsHandler{}
	app.Get("/users", QueryWithParams(pipe, testFindParamsRequest{}, responses.RawDoc, h))

	_, _ = app.Test(httptest.NewRequest("GET", "/users?sort=anything,-not_in_any_schema", nil))
	if h.got == nil {
		t.Fatal("expected handler called")
	}
	if len(h.got.Criteria.Sort) != 2 {
		t.Fatalf("expected 2 SortFields verbatim, got %d", len(h.got.Criteria.Sort))
	}
	if h.got.Criteria.Sort[0].Field != "anything" || h.got.Criteria.Sort[0].Desc {
		t.Errorf("expected first SortField=anything asc, got %+v", h.got.Criteria.Sort[0])
	}
	if h.got.Criteria.Sort[1].Field != "not_in_any_schema" || !h.got.Criteria.Sort[1].Desc {
		t.Errorf("expected second SortField=not_in_any_schema desc, got %+v", h.got.Criteria.Sort[1])
	}
}
