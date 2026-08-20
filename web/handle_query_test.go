package web

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/web/queryschema"
	"github.com/ClaudioSchirmer/omnicore/web/responses"
	"github.com/gofiber/fiber/v3"
)

// ─── canonical read-side fixtures (Result + Query + Request) ────────────────
//
// The read side mirrors the write side's Result anatomy: the fixtures below
// declare the application-layer Result (tagless, pointer/slice fields so the
// `?fields=` sparse guard passes), the Query implementing
// queries.QueryWithParams[TResult], and the Request DTO owning the wire
// allowlist. Response DTOs live near the tests that assert their wire shape.

// strPtr is the tiny address-of helper the fixtures share.
func strPtr(s string) *string { return &s }

// testAddressResult is the nested Result segment backing sparseAddress.
type testAddressResult struct {
	ID      *string
	City    *string
	ZipCode *string
	State   *string
}

// testUserResult is the canonical application-layer Result: tagless, every
// field *T or slice (the Result-side sparse contract), field names backing
// every Response fixture in the package (sparseUser, testUserSummary, …).
type testUserResult struct {
	ID        *string
	Name      *string
	Email     *string
	Phone     *string
	Addresses []testAddressResult
}

// rawItem projects the canonical Result into a loose wire map — the stand-in
// for the removed responses.RawDoc in tests that only assert criteria
// assembly / envelope framing. A map Response keeps the wrapper in
// pass-through mode: no projection schema, no Result↔Response alignment
// guard, `?orderBy=`/`?fields=` tokens land verbatim.
func rawItem(r testUserResult) map[string]any {
	out := map[string]any{}
	if r.ID != nil {
		out["id"] = *r.ID
	}
	if r.Name != nil {
		out["name"] = *r.Name
	}
	if r.Email != nil {
		out["email"] = *r.Email
	}
	return out
}

// testFindParamsRequest declares an allowlist for the params endpoint:
// name accepts only equality; email accepts equality + in; the remaining
// fields are reserved pagination/control keys recognized by the framework.
type testFindParamsAddresses struct {
	ZipCode *string `query:"zipCode" sort:"asc,desc"`
	State   *string `query:"state"   sort:"asc,desc"`
}

type testFindParamsRequest struct {
	Name  *string `query:"name"  filter:"eq"    sort:"asc,desc"`
	Email *string `query:"email" filter:"eq,in" sort:"asc,desc"`

	// Orderable-only leaves inside an embed group: they add nothing to the
	// filter vocabulary and prove a nested path reaches the reader.
	Addresses testFindParamsAddresses `query:"addresses"`

	Limit           *int64  `query:"first"`
	After           *string `query:"after"`
	Fields          *string `query:"fields"`
	Search          *string `query:"search"`
	IncludeArchived *bool   `query:"includeArchived"`
}

// testFindParamsQuery is the Query produced by ToQuery. ToCriteria(ctx) is the
// only layer that consumes *AppContext on the read side, so SeenLang is
// captured there (mirroring the production pattern where identity-derived
// overlays — tenant id etc. — layer onto the criteria inside ToCriteria).
// FromQueryResult is the mandatory doc→Result hook; the trivial implementation
// passes the framework-filled Result through unchanged.
type testFindParamsQuery struct {
	pipeline.QueryBase
	Criteria queries.ReadCriteria
	SeenLang configuration.Language
}

func (q *testFindParamsQuery) ToCriteria(ctx *configuration.AppContext) (queries.ReadCriteria, error) {
	q.SeenLang = ctx.Language()
	return q.Criteria, nil
}

func (q *testFindParamsQuery) FromQueryResult(_ *configuration.AppContext, r testUserResult) (testUserResult, error) {
	return r, nil
}

func (r testFindParamsRequest) ToQuery(crit queries.ReadCriteria) *testFindParamsQuery {
	return &testFindParamsQuery{Criteria: crit}
}

// capturingParamsHandler records the Query received and invokes ToCriteria so
// the SeenLang assertion still proves ctx propagation end to end. Returns the
// typed PageOf the wrappers now consume.
type capturingParamsHandler struct {
	got *testFindParamsQuery
}

func (h *capturingParamsHandler) Handle(ctx *configuration.AppContext, q *testFindParamsQuery) (queries.PageOf[testUserResult], error) {
	h.got = q
	_, _ = q.ToCriteria(ctx)
	return queries.PageOf[testUserResult]{
		Items:       []testUserResult{{ID: strPtr("abc")}},
		HasNextPage: true,
		TotalCount:  1,
	}, nil
}

func TestHandleQueryWithParams_UnknownFieldReturns400(t *testing.T) {
	app := fiber.New()
	pipe := newTestPipeline()
	h := &capturingParamsHandler{}

	app.Get("/users", QueryWithParams(pipe, testFindParamsRequest{}, rawItem, h))

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

	app.Get("/users", QueryWithParams(pipe, testFindParamsRequest{}, rawItem, h))

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

	app.Get("/users", QueryWithParams(pipe, testFindParamsRequest{}, rawItem, h))

	resp, _ := app.Test(httptest.NewRequest("GET", "/users?email.in=a@x.com,b@y.com&first=20&orderBy=-name", nil))
	if resp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d (body=%s)", resp.StatusCode, b)
	}
	if h.got == nil {
		t.Fatal("expected handler to be called")
	}
	emailFilter, ok := h.got.Criteria.Filter["Email"].(queries.Clause)
	if !ok || emailFilter.Op != queries.FilterIn {
		t.Fatalf("expected email filter to be an in-Clause, got %#v", h.got.Criteria.Filter["Email"])
	}
	in := emailFilter.Values
	if len(in) != 2 || in[0] != "a@x.com" || in[1] != "b@y.com" {
		t.Errorf("expected in=[a@x.com, b@y.com], got %v", in)
	}
	if h.got.Criteria.Limit != 20 {
		t.Errorf("expected Limit=20, got %d", h.got.Criteria.Limit)
	}
	// `?orderBy=-name` resolves to the Go field path, the same two hops a
	// filter leaf takes; the reader maps it to a column via the TableSchema.
	if len(h.got.Criteria.OrderBy) != 1 || h.got.Criteria.OrderBy[0].Field != "Name" || !h.got.Criteria.OrderBy[0].Desc {
		t.Errorf("expected Sort=[Name desc], got %v", h.got.Criteria.OrderBy)
	}
}

func TestHandleQueryWithParams_EmptyQueryStringStillCallsHandler(t *testing.T) {
	app := fiber.New()
	pipe := newTestPipeline()
	h := &capturingParamsHandler{}

	app.Get("/users", QueryWithParams(pipe, testFindParamsRequest{}, rawItem, h))

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

	app.Get("/users", QueryWithParams(pipe, testFindParamsRequest{}, rawItem, h))

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

	app.Get("/users", QueryWithParams(pipe, testFindParamsRequest{}, rawItem, h))

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
	if pag["hasNextPage"] != true {
		t.Errorf("expected pagination.hasNextPage=true, got %v", pag["hasNextPage"])
	}
	if pag["totalCount"] != float64(1) {
		t.Errorf("expected pagination.totalCount=1, got %v", pag["totalCount"])
	}
}

// TestHandleQueryWithParams_ResponseProjectionReshapesData proves the
// responseProjection parameter is honored — the wire data array carries the
// typed Response shape the consumer declared, not the Result's full field set.
//
// testUserSummary follows the sparse-render contract enforced by the boot
// guard whenever a Request DTO declares `query:"fields"` and is consumed
// by QueryWithParams with a struct Response: every field is *T +
// ,omitempty so encoding/json elides absent values cleanly. Its FromResult
// delegates to the generic name-based mapper — the canonical consumer pattern.
type testUserSummary struct {
	responses.Auto
	ID   *string `json:"id,omitempty"`
	Name *string `json:"name,omitempty"`
}

func (testUserSummary) FromResult(r testUserResult) testUserSummary {
	return responses.AutoFromResult[testUserSummary](r)
}

// fixedParamsHandler returns a hardcoded PageOf regardless of the incoming Query.
type fixedParamsHandler struct {
	page queries.PageOf[testUserResult]
}

func (h *fixedParamsHandler) Handle(_ *configuration.AppContext, _ *testFindParamsQuery) (queries.PageOf[testUserResult], error) {
	return h.page, nil
}

func TestHandleQueryWithParams_ResponseProjectionReshapesData(t *testing.T) {
	app := fiber.New()
	pipe := newTestPipeline()
	// Handler returns Results with id + name + email; the Response declares
	// only id + name (email absent in the output — exercises that the
	// projection controls the wire shape).
	h := &fixedParamsHandler{page: queries.PageOf[testUserResult]{
		Items: []testUserResult{
			{ID: strPtr("u1"), Name: strPtr("Alice"), Email: strPtr("a@x.com")},
			{ID: strPtr("u2"), Name: strPtr("Bob")},
		},
		TotalCount: 2,
	}}

	app.Get("/users", QueryWithParams(pipe, testFindParamsRequest{}, testUserSummary{}.FromResult, h))

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
		t.Errorf("response projection should have stripped 'email' from wire shape, got: %v", first)
	}
}

// --- by-id wrapper ---

type testFindIDRequest struct {
	IncludeArchived *bool `query:"includeArchived"`
}

type testFindIDQuery struct {
	queries.QueryByIDBase
	Criteria queries.ReadCriteria
	SeenLang configuration.Language
}

func (q *testFindIDQuery) ToCriteria(ctx *configuration.AppContext) (queries.ReadCriteria, error) {
	q.SeenLang = ctx.Language()
	return q.Criteria, nil
}
func (q *testFindIDQuery) ContextName() string { return "" }
func (q *testFindIDQuery) FromQueryResult(_ *configuration.AppContext, r testUserResult) (testUserResult, error) {
	return r, nil
}

func (r testFindIDRequest) ToQuery(criteria queries.ReadCriteria) *testFindIDQuery {
	return &testFindIDQuery{Criteria: criteria}
}

type capturingIDHandler struct {
	got *testFindIDQuery
}

func (h *capturingIDHandler) Handle(ctx *configuration.AppContext, q *testFindIDQuery) (testUserResult, error) {
	h.got = q
	_, _ = q.ToCriteria(ctx)
	return testUserResult{ID: strPtr(q.PathID().String())}, nil
}

func TestHandleQueryByID_AcceptsIncludeArchivedParam(t *testing.T) {
	app := fiber.New()
	pipe := newTestPipeline()
	h := &capturingIDHandler{}

	app.Get("/users/:id", QueryByID(pipe, testFindIDRequest{}, rawItem, h))

	resp, _ := app.Test(httptest.NewRequest("GET", "/users/abc?includeArchived=true", nil))
	if resp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d (body=%s)", resp.StatusCode, b)
	}
	if h.got == nil {
		t.Fatal("expected handler called")
	}
	if !h.got.Criteria.IncludeArchived {
		t.Error("expected Criteria.IncludeArchived=true to flow from ?includeArchived=true")
	}
	if h.got.PathID().Value() != "abc" {
		t.Errorf("expected PathID()='abc', got %q", h.got.PathID().Value())
	}
}

func TestHandleQueryByID_RejectsExtraParamWith400(t *testing.T) {
	app := fiber.New()
	pipe := newTestPipeline()
	h := &capturingIDHandler{}

	app.Get("/users/:id", QueryByID(pipe, testFindIDRequest{}, rawItem, h))

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

	app.Get("/users/:id", QueryByID(pipe, testFindIDRequest{}, rawItem, h))

	resp, _ := app.Test(httptest.NewRequest("GET", "/users/abc", nil))
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if h.got == nil || h.got.Criteria.IncludeArchived {
		t.Errorf("expected Criteria.IncludeArchived=false by default")
	}
}

func TestHandleQueryByID_AppContextFlowsIntoToCriteria(t *testing.T) {
	app := fiber.New()
	app.Use(AppContextMiddleware())
	pipe := newTestPipeline()
	h := &capturingIDHandler{}

	app.Get("/users/:id", QueryByID(pipe, testFindIDRequest{}, rawItem, h))

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

// idDocHandler returns a fixed Result with the path id injected.
type idDocHandler struct{}

func (h *idDocHandler) Handle(_ *configuration.AppContext, q *testFindIDQuery) (testUserResult, error) {
	return testUserResult{ID: strPtr(q.PathID().String()), Name: strPtr("Carol"), Email: strPtr("c@x.com")}, nil
}

// TestHandleQueryByID_ResponseProjectionReshapesData proves the projection
// applies on the by-id success path — the wire data carries the typed shape,
// not the Result's full field set.
func TestHandleQueryByID_ResponseProjectionReshapesData(t *testing.T) {
	app := fiber.New()
	pipe := newTestPipeline()
	h := &idDocHandler{}

	app.Get("/users/:id", QueryByID(pipe, testFindIDRequest{}, testUserSummary{}.FromResult, h))

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
		t.Errorf("response projection should have stripped 'email' from wire shape, got: %v", data)
	}
}

// --- NewQueryParser (manual-route parsing) / RespondSchemaViolation ---
//
// web.ParseCriteria was REMOVED — manual handlers construct a typed
// QueryParser at mount time. The cases below keep the removed helper's
// behavioral assertions alive on the replacement surface (a map Response
// keeps the parser in pass-through mode, matching the old default).

func TestQueryParser_Parse_HappyPath(t *testing.T) {
	parser := NewQueryParser[testFindParamsRequest, map[string]any]()
	app := fiber.New()
	var got queries.ReadCriteria
	var gotViolation *queryschema.Violation
	var gotOK bool
	app.Get("/x", func(c fiber.Ctx) error {
		got, gotViolation, gotOK = parser.Parse(c)
		return c.SendStatus(fiber.StatusOK)
	})

	resp, _ := app.Test(httptest.NewRequest("GET", "/x?name=Jane&first=5", nil))
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if !gotOK || gotViolation != nil {
		t.Errorf("expected ok=true, no violation; got ok=%v, violation=%+v", gotOK, gotViolation)
	}
	if got.Filter["Name"] != "Jane" {
		t.Errorf("expected filter name=Jane, got %v", got.Filter["Name"])
	}
	if got.Limit != 5 {
		t.Errorf("expected limit=5, got %d", got.Limit)
	}
}

func TestQueryParser_Parse_UnknownKey(t *testing.T) {
	parser := NewQueryParser[testFindParamsRequest, map[string]any]()
	app := fiber.New()
	var gotViolation *queryschema.Violation
	var gotOK bool
	app.Get("/x", func(c fiber.Ctx) error {
		_, gotViolation, gotOK = parser.Parse(c)
		return c.SendStatus(fiber.StatusOK)
	})

	_, _ = app.Test(httptest.NewRequest("GET", "/x?role=admin", nil))
	if gotOK {
		t.Error("expected ok=false for unknown key")
	}
	if gotViolation == nil || gotViolation.Field != "role" {
		t.Errorf("expected badField=role, got %+v", gotViolation)
	}
}

func TestQueryParser_Parse_BadOperator(t *testing.T) {
	parser := NewQueryParser[testFindParamsRequest, map[string]any]()
	app := fiber.New()
	var gotViolation *queryschema.Violation
	var gotOK bool
	app.Get("/x", func(c fiber.Ctx) error {
		_, gotViolation, gotOK = parser.Parse(c)
		return c.SendStatus(fiber.StatusOK)
	})

	// name has filter:"eq" only — .in is disallowed
	_, _ = app.Test(httptest.NewRequest("GET", "/x?name.in=a,b", nil))
	if gotOK {
		t.Error("expected ok=false for disallowed operator")
	}
	if gotViolation == nil || gotViolation.Field != "name.in" {
		t.Errorf("expected badField=name.in, got %+v", gotViolation)
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
	items, pag := ProjectPage(queries.PageOf[testUserResult]{}, rawItem)
	if len(items) != 0 {
		t.Errorf("expected empty items, got %d", len(items))
	}
	if pag == nil {
		t.Fatal("expected pagination to be non-nil even on empty page")
	}
}

func TestProjectPage_AppliesProjectionPerItem(t *testing.T) {
	page := queries.PageOf[testUserResult]{
		Items: []testUserResult{
			{ID: strPtr("1"), Name: strPtr("A")},
			{ID: strPtr("2"), Name: strPtr("B")},
		},
		HasNextPage: true,
		EndCursor:   "cur-xyz",
		TotalCount:  2,
	}
	items, pag := ProjectPage(page, testUserSummary{}.FromResult)
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	if items[0].ID == nil || *items[0].ID != "1" || items[0].Name == nil || *items[0].Name != "A" {
		t.Errorf("first item mismatch: %+v", items[0])
	}
	if !pag.HasNextPage || pag.EndCursor != "cur-xyz" || pag.TotalCount != 2 {
		t.Errorf("pagination mismatch: %+v", pag)
	}
}

func TestRespondPaged_EmitsEnvelope(t *testing.T) {
	app := fiber.New()
	app.Get("/x", func(c fiber.Ctx) error {
		page := queries.PageOf[testUserResult]{
			Items:       []testUserResult{{ID: strPtr("1"), Name: strPtr("A")}},
			HasNextPage: false,
			TotalCount:  1,
		}
		return RespondPaged(c, fiber.StatusOK, page, testUserSummary{}.FromResult)
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

// ─── runtime ?orderBy= behavior ───────────────────────────────────────────────
//
// The reserved `orderBy` key is symmetric to `fields`: the Request DTO opts in
// by declaring `Sort *string query:"orderBy"`; when the wrapper is paired with
// a typed Response struct, each comma-separated token is validated against
// the Response's declared wire paths and translated to the doc-side path.
// Wrappers paired with a map Response get pass-through behavior.

func TestSortParam_UnknownTokenReturns400WithBracketedField(t *testing.T) {
	app := fiber.New()
	pipe := newTestPipeline()
	h := &capturingParamsHandler{}
	app.Get("/users", QueryWithParams(pipe, testFindParamsRequest{}, sparseUser{}.FromResult, h))

	resp, _ := app.Test(httptest.NewRequest("GET", "/users?orderBy=bogus", nil))
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
	if got := msg["field"]; got != "orderBy[bogus]" {
		t.Errorf("expected field=orderBy[bogus], got %v", got)
	}
}

func TestSortParam_UnknownTokenWithMinusPrefixPreservesPrefixInError(t *testing.T) {
	// The minus sign is part of the wire token the consumer sent — preserve
	// it verbatim in the 400 envelope so the rejection traces back to the
	// exact query-string fragment.
	app := fiber.New()
	pipe := newTestPipeline()
	h := &capturingParamsHandler{}
	app.Get("/users", QueryWithParams(pipe, testFindParamsRequest{}, sparseUser{}.FromResult, h))

	resp, _ := app.Test(httptest.NewRequest("GET", "/users?orderBy=-bogus", nil))
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
	if got := msg["field"]; got != "orderBy[-bogus]" {
		t.Errorf("expected field=orderBy[-bogus] (prefix preserved), got %v", got)
	}
}

func TestSortParam_KnownTokenTranslatesToDocPath(t *testing.T) {
	app := fiber.New()
	pipe := newTestPipeline()
	h := &capturingParamsHandler{}
	app.Get("/users", QueryWithParams(pipe, testFindParamsRequest{}, sparseUser{}.FromResult, h))

	_, _ = app.Test(httptest.NewRequest("GET", "/users?orderBy=addresses.zipCode", nil))
	if h.got == nil {
		t.Fatal("expected handler called")
	}
	if len(h.got.Criteria.OrderBy) != 1 {
		t.Fatalf("expected 1 OrderByField, got %d", len(h.got.Criteria.OrderBy))
	}
	if h.got.Criteria.OrderBy[0].Field != "Addresses.ZipCode" {
		t.Errorf("expected Field=Addresses.ZipCode (Go field path), got %q", h.got.Criteria.OrderBy[0].Field)
	}
	if h.got.Criteria.OrderBy[0].Desc {
		t.Errorf("expected Desc=false on bare token, got true")
	}
}

func TestSortParam_NestedTokenTranslatesToGoPath(t *testing.T) {
	app := fiber.New()
	pipe := newTestPipeline()
	h := &capturingParamsHandler{}
	app.Get("/users", QueryWithParams(pipe, testFindParamsRequest{}, sparseUser{}.FromResult, h))

	_, _ = app.Test(httptest.NewRequest("GET", "/users?orderBy=addresses.state", nil))
	if h.got == nil {
		t.Fatal("expected handler called")
	}
	if len(h.got.Criteria.OrderBy) != 1 {
		t.Fatalf("expected 1 OrderByField, got %d", len(h.got.Criteria.OrderBy))
	}
	if h.got.Criteria.OrderBy[0].Field != "Addresses.State" {
		t.Errorf("expected Field=Addresses.State, got %q", h.got.Criteria.OrderBy[0].Field)
	}
}

func TestSortParam_MinusPrefixSetsDesc(t *testing.T) {
	app := fiber.New()
	pipe := newTestPipeline()
	h := &capturingParamsHandler{}
	app.Get("/users", QueryWithParams(pipe, testFindParamsRequest{}, sparseUser{}.FromResult, h))

	_, _ = app.Test(httptest.NewRequest("GET", "/users?orderBy=-name", nil))
	if h.got == nil {
		t.Fatal("expected handler called")
	}
	if len(h.got.Criteria.OrderBy) != 1 {
		t.Fatalf("expected 1 OrderByField, got %d", len(h.got.Criteria.OrderBy))
	}
	if h.got.Criteria.OrderBy[0].Field != "Name" || !h.got.Criteria.OrderBy[0].Desc {
		t.Errorf("expected Field=Name,Desc=true, got %+v", h.got.Criteria.OrderBy[0])
	}
}

func TestSortParam_MultipleTokensIndependentDirections(t *testing.T) {
	app := fiber.New()
	pipe := newTestPipeline()
	h := &capturingParamsHandler{}
	app.Get("/users", QueryWithParams(pipe, testFindParamsRequest{}, sparseUser{}.FromResult, h))

	_, _ = app.Test(httptest.NewRequest("GET", "/users?orderBy=name,-email", nil))
	if h.got == nil {
		t.Fatal("expected handler called")
	}
	if len(h.got.Criteria.OrderBy) != 2 {
		t.Fatalf("expected 2 OrderByFields, got %d", len(h.got.Criteria.OrderBy))
	}
	if h.got.Criteria.OrderBy[0].Field != "Name" || h.got.Criteria.OrderBy[0].Desc {
		t.Errorf("expected first OrderByField=Name asc, got %+v", h.got.Criteria.OrderBy[0])
	}
	if h.got.Criteria.OrderBy[1].Field != "Email" || !h.got.Criteria.OrderBy[1].Desc {
		t.Errorf("expected second OrderByField=Email desc, got %+v", h.got.Criteria.OrderBy[1])
	}
}

// testFindSortOnlyRequest opts into sort but NOT into fields — proves the
// projSchema is built (and the allowlist fires) when sort is the only
// reserved key requesting Response-side validation.
type testFindSortOnlyRequest struct {
	Name *string `query:"name" filter:"eq" sort:"asc,desc"`
}

func (r testFindSortOnlyRequest) ToQuery(crit queries.ReadCriteria) *testFindParamsQuery {
	return &testFindParamsQuery{Criteria: crit}
}

// Ordering does not depend on the `?fields=` opt-in: the vocabulary comes from
// the Request DTO, so a DTO that declares orderable fields and nothing else
// orders and enforces its allowlist all the same.
func TestOrdering_WorksWithoutTheFieldsOptIn(t *testing.T) {
	pipe := newTestPipeline()

	app := fiber.New()
	app.Get("/users", QueryWithParams(pipe, testFindSortOnlyRequest{}, sparseUser{}.FromResult, &capturingParamsHandler{}))
	resp, _ := app.Test(httptest.NewRequest("GET", "/users?orderBy=bogus", nil))
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("an undeclared token must be refused, got %d", resp.StatusCode)
	}

	h := &capturingParamsHandler{}
	app2 := fiber.New()
	app2.Get("/users", QueryWithParams(pipe, testFindSortOnlyRequest{}, sparseUser{}.FromResult, h))
	_, _ = app2.Test(httptest.NewRequest("GET", "/users?orderBy=-name", nil))
	if h.got == nil {
		t.Fatal("expected handler called")
	}
	if len(h.got.Criteria.OrderBy) != 1 ||
		h.got.Criteria.OrderBy[0].Field != "Name" ||
		!h.got.Criteria.OrderBy[0].Desc {
		t.Errorf("expected OrderByField=Name desc, got %+v", h.got.Criteria.OrderBy)
	}
}

// The Response shape is irrelevant to ordering. With an untyped (map) Response
// — where `?fields=` falls back to pass-through — the ordering allowlist is
// unchanged, because it never consulted the Response in the first place.
func TestOrdering_IgnoresTheResponseShape(t *testing.T) {
	pipe := newTestPipeline()

	h := &capturingParamsHandler{}
	app := fiber.New()
	app.Get("/users", QueryWithParams(pipe, testFindParamsRequest{}, rawItem, h))
	_, _ = app.Test(httptest.NewRequest("GET", "/users?orderBy=name,-email", nil))
	if h.got == nil {
		t.Fatal("expected handler called")
	}
	if len(h.got.Criteria.OrderBy) != 2 ||
		h.got.Criteria.OrderBy[0].Field != "Name" || h.got.Criteria.OrderBy[0].Desc ||
		h.got.Criteria.OrderBy[1].Field != "Email" || !h.got.Criteria.OrderBy[1].Desc {
		t.Errorf("declared tokens must translate, got %+v", h.got.Criteria.OrderBy)
	}

	app2 := fiber.New()
	app2.Get("/users", QueryWithParams(pipe, testFindParamsRequest{}, rawItem, &capturingParamsHandler{}))
	resp, _ := app2.Test(httptest.NewRequest("GET", "/users?orderBy=not_in_any_schema", nil))
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("an untyped Response is not a licence to sort by anything, got %d", resp.StatusCode)
	}
}
