package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/queries"
	fwresults "github.com/ClaudioSchirmer/omnicore/application/results"
	"github.com/ClaudioSchirmer/omnicore/application/translation"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/web/responses"
	"github.com/gofiber/fiber/v3"
)

// ─── shared failure helper ──────────────────────────────────────────────────

// failureError returns a NotificationCarrier so pipeline.Dispatch resolves the
// result to a Failure, driving the RespondFromResult branches in the wrappers.
func failureError() error {
	nc := domain.NewNotificationContext("Test")
	nc.AddNotificationMessage(domain.NotificationMessage{
		FieldName:    "field",
		Notification: domain.RequiredFieldNotification{},
	})
	return domain.NewDomainError([]*domain.NotificationContext{nc})
}

// ─── HandleCommandByID — Failure branch (respondWithProjection) ───────────

type failingNoBodyHandler struct{}

func (failingNoBodyHandler) Handle(_ *configuration.AppContext, _ *testNoBodyCmd) (fwresults.None, error) {
	return fwresults.None{}, failureError()
}

func TestHandleCommandByID_FailureBranch(t *testing.T) {
	app := fiber.New()
	app.Patch("/things/:id/archive", HandleCommandByID(newTestPipeline(), responses.NoBody, failingNoBodyHandler{}, fiber.StatusOK))

	resp, _ := app.Test(httptest.NewRequest("PATCH", "/things/x/archive", nil))
	if resp.StatusCode != fiber.StatusUnprocessableEntity {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 422 from Failure, got %d (body=%s)", resp.StatusCode, b)
	}
}

// ─── HandleCommandWithBody — path-binding failure ───────────────────────────

type pathNumInsertReq struct {
	Num int `path:"num"`
}

func (r pathNumInsertReq) ToCommand() *testInsertCmd { return &testInsertCmd{} }

func TestHandleCommandWithBody_PathBindingFailure_400(t *testing.T) {
	resetPathSchemaCache()
	app := fiber.New()
	h := &capturingInsertHandler{}
	app.Post("/things/:num", HandleCommandWithBody(newTestPipeline(), pathNumInsertReq{}, responses.NoBody, h, fiber.StatusCreated))

	// "abc" cannot convert to int → path-binding rejection → 400.
	resp, _ := app.Test(httptest.NewRequest("POST", "/things/abc", nil))
	if resp.StatusCode != fiber.StatusBadRequest {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 400, got %d (body=%s)", resp.StatusCode, b)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"field":"num"`) {
		t.Errorf("expected field=num in the violation, got: %s", body)
	}
}

// ─── HandleCommandWithBodyID — path-binding failure + body-bind failure ──────

type lenientUpdateHandler struct{}

func (lenientUpdateHandler) Handle(_ *configuration.AppContext, _ *testUpdateCmd) (fwresults.None, error) {
	return fwresults.None{}, nil
}

type pathNumUpdateReq struct {
	Num int `path:"num"`
}

func (r pathNumUpdateReq) ToCommand() *testUpdateCmd { return &testUpdateCmd{} }

func TestHandleCommandWithBodyID_PathBindingFailure_400(t *testing.T) {
	resetPathSchemaCache()
	app := fiber.New()
	app.Put("/things/:id/extra/:num", HandleCommandWithBodyID(newTestPipeline(), pathNumUpdateReq{}, responses.NoBody, lenientUpdateHandler{}, fiber.StatusOK))

	resp, _ := app.Test(httptest.NewRequest("PUT", "/things/x/extra/abc", nil))
	if resp.StatusCode != fiber.StatusBadRequest {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 400, got %d (body=%s)", resp.StatusCode, b)
	}
}

type typedUpdateReq struct {
	Age int `json:"age"`
}

func (r typedUpdateReq) ToCommand() *testUpdateCmd { return &testUpdateCmd{} }

func TestHandleCommandWithBodyID_BodyBindFailure_400(t *testing.T) {
	resetPathSchemaCache()
	app := fiber.New()
	app.Put("/things/:id", HandleCommandWithBodyID(newTestPipeline(), typedUpdateReq{}, responses.NoBody, lenientUpdateHandler{}, fiber.StatusOK))

	// age declared int; pass a string → c.Bind().Body fails → 400.
	body, _ := json.Marshal(map[string]string{"age": "not-an-int"})
	req := httptest.NewRequest("PUT", "/things/x", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, _ := app.Test(req)
	if resp.StatusCode != fiber.StatusBadRequest {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 400, got %d (body=%s)", resp.StatusCode, b)
	}
	bodyResp, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(bodyResp), "SchemaViolationNotification") {
		t.Errorf("expected SchemaViolationNotification, got: %s", bodyResp)
	}
}

// ─── warnGroupAMissingPathTag — early return when a path: tag is present ─────

type pathTaggedInsertReq struct {
	Tenant string `path:"tenantId"`
}

func (r pathTaggedInsertReq) ToCommand() *testInsertCmd { return &testInsertCmd{} }

// pathIDRequiredInsertHandler is declared in coverage_command_test.go; reused
// here paired with a path:-tagged Request to drive warnGroupAMissingPathTag's
// early-return branch (schema has fields → no warning).
func TestWarnGroupAMissingPathTag_SilentWhenPathTagPresent(t *testing.T) {
	resetPathSchemaCache()
	// Construction alone runs warnGroupAMissingPathTag; with a PathIDRequired
	// handler AND a path:-tagged Request, the helper returns early (no warning).
	_ = HandleCommandWithBody(newTestPipeline(), pathTaggedInsertReq{}, responses.NoBody, &pathIDRequiredInsertHandler{}, fiber.StatusCreated)
}

// ─── HandleQueryWithParams — path-binding failure + Failure branch ──────────

type pathParamsReq struct {
	Tenant int     `path:"tenantId"`
	Name   *string `query:"name" filter:"eq"`
}

func (r pathParamsReq) ToQuery(crit queries.ReadCriteria) *testFindParamsQuery {
	return &testFindParamsQuery{Criteria: crit}
}

func TestHandleQueryWithParams_PathBindingFailure_400(t *testing.T) {
	resetPathSchemaCache()
	app := fiber.New()
	h := &capturingParamsHandler{}
	app.Get("/t/:tenantId/users", HandleQueryWithParams(newTestPipeline(), pathParamsReq{}, responses.RawDoc, h))

	resp, _ := app.Test(httptest.NewRequest("GET", "/t/abc/users", nil))
	if resp.StatusCode != fiber.StatusBadRequest {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 400 from path-binding, got %d (body=%s)", resp.StatusCode, b)
	}
	if h.got != nil {
		t.Error("handler must not run when path-binding fails")
	}
}

type failingParamsHandler struct{}

func (failingParamsHandler) Handle(_ *configuration.AppContext, _ *testFindParamsQuery) (queries.Page, error) {
	return queries.Page{}, failureError()
}

func TestHandleQueryWithParams_FailureBranch(t *testing.T) {
	app := fiber.New()
	app.Get("/users", HandleQueryWithParams(newTestPipeline(), testFindParamsRequest{}, responses.RawDoc, failingParamsHandler{}))

	resp, _ := app.Test(httptest.NewRequest("GET", "/users", nil))
	if resp.StatusCode != fiber.StatusUnprocessableEntity {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 422 from Failure, got %d (body=%s)", resp.StatusCode, b)
	}
}

// ─── HandleQueryWithParams — VisitAll short-circuits after first bad key ────

func TestHandleQueryWithParams_TwoUnknownKeysShortCircuit_400(t *testing.T) {
	app := fiber.New()
	h := &capturingParamsHandler{}
	app.Get("/users", HandleQueryWithParams(newTestPipeline(), testFindParamsRequest{}, responses.RawDoc, h))

	// Two unknown keys: the first flips ok=false; the second hits the
	// VisitAll short-circuit guard.
	resp, _ := app.Test(httptest.NewRequest("GET", "/users?alpha=1&beta=2", nil))
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	if h.got != nil {
		t.Error("handler must not run when the allowlist rejects keys")
	}
}

// ─── buildCriteria — malformed ?after= cursor ───────────────────────────────

func TestHandleQueryWithParams_MalformedCursor_400(t *testing.T) {
	app := fiber.New()
	h := &capturingParamsHandler{}
	app.Get("/users", HandleQueryWithParams(newTestPipeline(), testFindParamsRequest{}, responses.RawDoc, h))

	resp, _ := app.Test(httptest.NewRequest("GET", "/users?after=@@not-a-cursor@@", nil))
	if resp.StatusCode != fiber.StatusBadRequest {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 400 for malformed cursor, got %d (body=%s)", resp.StatusCode, b)
	}
	if h.got != nil {
		t.Error("handler must not run when the cursor is malformed")
	}
}

// ─── validateCursorAgainstCriteria — decode / tuple-length / hash branches ──

func TestValidateCursorAgainstCriteria_AllBranches(t *testing.T) {
	crit := queries.ReadCriteria{Filter: map[string]any{}}
	h := queries.HashContext(crit.Filter, crit.Sort, crit.Search, crit.IncludeArchived)

	// Valid: K=[_id] → len(K)-1 == 0 == len(Sort); hash matches.
	okCursor, err := queries.EncodeCursor([]any{"id-1"}, h)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if bad, ok := validateCursorAgainstCriteria(okCursor, crit, "after"); !ok || bad != "" {
		t.Errorf("expected valid cursor to pass, got bad=%q ok=%v", bad, ok)
	}

	// Tuple-length mismatch: K=[a,b] → len(K)-1 == 1 != len(Sort)==0.
	tupleCursor, _ := queries.EncodeCursor([]any{"a", "b"}, h)
	if bad, ok := validateCursorAgainstCriteria(tupleCursor, crit, "after"); ok || bad != "after" {
		t.Errorf("expected tuple-length rejection, got bad=%q ok=%v", bad, ok)
	}

	// Context-hash mismatch is NOT the wrapper's job: at this layer the
	// criteria is the pre-ToCriteria wire snapshot, while cursors are stamped
	// from the post-ToCriteria context — comparing them would reject every
	// legitimate cursor of an overlay-bearing paged query. The reader performs
	// the authoritative hash check post-ToCriteria; here the cursor passes.
	hashCursor, _ := queries.EncodeCursor([]any{"x"}, "deadbeef")
	if bad, ok := validateCursorAgainstCriteria(hashCursor, crit, "after"); !ok || bad != "" {
		t.Errorf("expected the hash check to be deferred to the reader, got bad=%q ok=%v", bad, ok)
	}

	// Decode error: not a valid cursor token.
	if bad, ok := validateCursorAgainstCriteria("@@not-a-cursor@@", crit, "after"); ok || bad != "after" {
		t.Errorf("expected decode-error rejection, got bad=%q ok=%v", bad, ok)
	}
}

// ─── HandleQueryByID — boot panic on path:"id" + Failure branch ───────────

type idTaggedIDReq struct {
	ID string `path:"id"`
}

func (r idTaggedIDReq) ToQuery() *testFindIDQuery { return &testFindIDQuery{} }

func TestHandleQueryByID_PanicsOnPathIDTag(t *testing.T) {
	resetPathSchemaCache()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected boot panic when Request declares path:\"id\"")
		}
	}()
	_ = HandleQueryByID(newTestPipeline(), idTaggedIDReq{}, responses.RawDoc, &capturingIDHandler{})
}

type failingIDHandler struct{}

func (failingIDHandler) Handle(_ *configuration.AppContext, _ *testFindIDQuery) (map[string]any, error) {
	return nil, failureError()
}

func TestHandleQueryByID_FailureBranch(t *testing.T) {
	resetPathSchemaCache()
	app := fiber.New()
	app.Get("/users/:id", HandleQueryByID(newTestPipeline(), testFindIDRequest{}, responses.RawDoc, failingIDHandler{}))

	resp, _ := app.Test(httptest.NewRequest("GET", "/users/abc", nil))
	if resp.StatusCode != fiber.StatusUnprocessableEntity {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 422 from Failure, got %d (body=%s)", resp.StatusCode, b)
	}
}

// ─── NewQueryParser — pointer Req / pointer Resp deref ──────────────────────

func TestNewQueryParser_PointerReqAndPointerResp(t *testing.T) {
	// *Req exercises the reqType pointer-deref loop.
	p1 := NewQueryParser[*testFindParamsRequest, sparseUser]()
	if p1 == nil {
		t.Fatal("NewQueryParser[*Req, Resp] returned nil")
	}
	// *Resp exercises the respType pointer-deref loop (fields/sort opt-in is
	// present on testFindParamsRequest, so the Response branch runs).
	p2 := NewQueryParser[testFindParamsRequest, *sparseUser]()
	if p2 == nil {
		t.Fatal("NewQueryParser[Req, *Resp] returned nil")
	}
}

// ─── HandleQueryAsCSV — path-binding failure ────────────────────────────────

type pathExportReq struct {
	Tenant int     `path:"tenantId"`
	Email  *string `query:"email" filter:"eq"`
}

func (r pathExportReq) ToQuery(c queries.ReadCriteria) *expCSVQuery {
	return &expCSVQuery{Criteria: c}
}

func TestHandleQueryAsCSV_PathBindingFailure_400(t *testing.T) {
	resetPathSchemaCache()
	app := fiber.New()
	h := newExportHandler()
	view := fakeExportView{plan: expCSVPlan(), name: "users"}
	deps := ExportDeps{Translator: translation.Default(), MaxExportRows: 100}
	app.Get("/t/:tenantId/users.csv", HandleQueryAsCSV(newTestPipeline(), pathExportReq{}, view, deps, h))

	resp, _ := app.Test(httptest.NewRequest("GET", "/t/abc/users.csv", nil))
	if resp.StatusCode != fiber.StatusBadRequest {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 400 from path-binding, got %d (body=%s)", resp.StatusCode, b)
	}
}

// ─── ErrorHandler — nil error short-circuit ─────────────────────────────────

func TestErrorHandler_NilErrorReturnsNil(t *testing.T) {
	app := fiber.New()
	pipe := newTestPipeline()
	app.Get("/x", func(c fiber.Ctx) error {
		eh := ErrorHandler(pipe)
		if err := eh(c, nil); err != nil {
			t.Errorf("ErrorHandler with nil error must return nil, got %v", err)
		}
		return c.SendString("ok")
	})
	resp, _ := app.Test(httptest.NewRequest("GET", "/x", nil))
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

// ─── extractBearerToken — whitespace-only token after Bearer ────────────────

func TestExtractBearerToken_WhitespaceOnlyAfterScheme(t *testing.T) {
	if tok, ok := extractBearerToken("Bearer    "); ok {
		t.Errorf("whitespace-only token must be rejected, got %q ok=%v", tok, ok)
	}
}

// ─── external_validator — newExternalValidator parseJSONPath error ──────────

func TestNewExternalValidator_InvalidJSONPath(t *testing.T) {
	_, err := newExternalValidator(ExternalValidatorOptions{
		URL:            "https://idp/introspect",
		TokenPlacement: "bearer_header",
		Success:        ExternalValidatorSuccess{JSONPath: "no-dollar", ExpectedValue: true},
	})
	if err == nil || !strings.Contains(err.Error(), "jsonPath") {
		t.Fatalf("expected jsonPath parse error, got %v", err)
	}
}

// ─── external_validator — buildRequest errors (invalid target URL) ──────────

func TestValidate_BearerHeader_BuildRequestError(t *testing.T) {
	// A control char in the URL makes http.NewRequestWithContext fail inside
	// buildRequest (bearer_header path) → callIdP surfaces the error.
	v, err := newExternalValidator(validateOpts("http://\nbad", "bearer_header", ""))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := v.Validate(context.Background(), "tok"); err == nil {
		t.Fatal("expected buildRequest error for a malformed URL")
	}
}

func TestValidate_QueryParam_BuildRequestURLParseError(t *testing.T) {
	// query_param placement parses v.url up front; a control char makes
	// url.Parse fail inside buildRequest.
	v, err := newExternalValidator(validateOpts("http://\nbad", "query_param", "token"))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := v.Validate(context.Background(), "tok"); err == nil {
		t.Fatal("expected url.Parse error for a malformed query_param URL")
	}
}

// ─── path_binding — unexported field carrying a path: tag panics ────────────

type unexportedPathReq struct {
	email string `path:"email"` //nolint:unused // reflection-only: must panic at inspect
}

func TestUnexportedPathField_PanicsAtInspect(t *testing.T) {
	resetPathSchemaCache()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on an unexported field with a path: tag")
		}
		if msg, _ := r.(string); !strings.Contains(msg, "unexported") {
			t.Fatalf("panic missing 'unexported' hint: %v", r)
		}
	}()
	inspectPathTags(reflect.TypeOf(unexportedPathReq{}))
}
