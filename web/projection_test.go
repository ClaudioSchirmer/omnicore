package web

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/web/responses"
	"github.com/gofiber/fiber/v3"
)

// Response fixtures for the runtime ?fields= behavior. The reflection of these
// shapes (projection paths, sparse-render guard) is unit-tested in
// web/queryschema; here they drive the end-to-end wrapper behavior. Each one
// is backed field-for-field by the canonical Result fixtures
// (testUserResult / testAddressResult) the Result↔Response alignment guard
// now enforces at mount time.

type sparseAddress struct {
	ID      *string `json:"id,omitempty"`
	City    *string `json:"city,omitempty"`
	ZipCode *string `json:"zipCode,omitempty"`
	State   *string `json:"state,omitempty"`
}

type sparseUser struct {
	ID        *string         `json:"id,omitempty"`
	Name      *string         `json:"name,omitempty"`
	Email     *string         `json:"email,omitempty"`
	Phone     *string         `json:"phone,omitempty"`
	Addresses []sparseAddress `json:"addresses,omitempty"`
}

// FromResult is the canonical consumer projector: the generic name-based
// Result→Response mapper.
func (sparseUser) FromResult(r testUserResult) sparseUser {
	return responses.Map[sparseUser](r)
}

type guardNonPointerScalar struct {
	Name string `json:"name,omitempty"`
}

// FromResult exists only so the boot-guard fixture can be mounted the same
// way a real Response is — the guard panics before it is ever invoked.
func (guardNonPointerScalar) FromResult(r testUserResult) guardNonPointerScalar {
	return responses.Map[guardNonPointerScalar](r)
}

// ─── QueryWithParams boot guard integration ──────────────────────────

func TestHandleQueryWithParams_BootPanicsOnBadResponse(t *testing.T) {
	pipe := newTestPipeline()
	h := &capturingParamsHandler{}
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected boot panic when Request declares query:fields and Response has non-pointer fields")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, "guardNonPointerScalar") || !strings.Contains(msg, "name") {
			t.Errorf("expected diagnostic to mention type + offending field, got: %s", msg)
		}
	}()
	app := fiber.New()
	app.Get("/x", QueryWithParams(pipe, testFindParamsRequest{}, guardNonPointerScalar{}.FromResult, h))
}

// ─── runtime ?fields= behavior ─────────────────────────────────────────────

func TestFieldsParam_UnknownTokenReturns400WithBracketedField(t *testing.T) {
	app := fiber.New()
	pipe := newTestPipeline()
	h := &capturingParamsHandler{}
	app.Get("/users", QueryWithParams(pipe, testFindParamsRequest{}, sparseUser{}.FromResult, h))

	resp, _ := app.Test(httptest.NewRequest("GET", "/users?fields=name,bogus", nil))
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
	if got := msg["field"]; got != "fields[bogus]" {
		t.Errorf("expected field=fields[bogus], got %v", got)
	}
}

func TestFieldsParam_ProjectionIncludesAutoIDExclusionWhenIDNotRequested(t *testing.T) {
	app := fiber.New()
	pipe := newTestPipeline()
	h := &capturingParamsHandler{}
	app.Get("/users", QueryWithParams(pipe, testFindParamsRequest{}, sparseUser{}.FromResult, h))

	_, _ = app.Test(httptest.NewRequest("GET", "/users?fields=name,email", nil))
	got := h.got.Criteria.Projection
	if v, ok := got["Name"]; !ok || v != 1 {
		t.Errorf("expected name:1 in projection, got %v", got)
	}
	if v, ok := got["Email"]; !ok || v != 1 {
		t.Errorf("expected email:1 in projection, got %v", got)
	}
	if v, ok := got["_id"]; !ok || v != 0 {
		t.Errorf("expected _id:0 in projection (id not requested), got %v", got)
	}
}

func TestFieldsParam_ProjectionOmitsIDExclusionWhenIDRequested(t *testing.T) {
	app := fiber.New()
	pipe := newTestPipeline()
	h := &capturingParamsHandler{}
	app.Get("/users", QueryWithParams(pipe, testFindParamsRequest{}, sparseUser{}.FromResult, h))

	_, _ = app.Test(httptest.NewRequest("GET", "/users?fields=id,name", nil))
	got := h.got.Criteria.Projection
	if v, ok := got["ID"]; !ok || v != 1 {
		t.Errorf("expected id:1 in projection, got %v", got)
	}
	if v, ok := got["Name"]; !ok || v != 1 {
		t.Errorf("expected name:1 in projection, got %v", got)
	}
	if _, hasIDExclusion := got["_id"]; hasIDExclusion {
		t.Errorf("expected no _id:0 entry when id was requested, got %v", got)
	}
}

func TestFieldsParam_NestedPathTranslatesViaAutoSnake(t *testing.T) {
	app := fiber.New()
	pipe := newTestPipeline()
	h := &capturingParamsHandler{}
	app.Get("/users", QueryWithParams(pipe, testFindParamsRequest{}, sparseUser{}.FromResult, h))

	_, _ = app.Test(httptest.NewRequest("GET", "/users?fields=addresses.zipCode", nil))
	got := h.got.Criteria.Projection
	if v, ok := got["Addresses.ZipCode"]; !ok || v != 1 {
		t.Errorf("expected addresses.zip_code:1 (PascalToSnake), got %v", got)
	}
	if v, ok := got["_id"]; !ok || v != 0 {
		t.Errorf("expected _id:0 (id not requested), got %v", got)
	}
}

func TestFieldsParam_NestedPathHonorsViewOverride(t *testing.T) {
	app := fiber.New()
	pipe := newTestPipeline()
	h := &capturingParamsHandler{}
	app.Get("/users", QueryWithParams(pipe, testFindParamsRequest{}, sparseUser{}.FromResult, h))

	_, _ = app.Test(httptest.NewRequest("GET", "/users?fields=addresses.state", nil))
	got := h.got.Criteria.Projection
	if v, ok := got["Addresses.State"]; !ok || v != 1 {
		t.Errorf("expected addresses.st:1 (Go field path (view: removed)), got %v", got)
	}
}

func TestFieldsParam_WholeAggregateProjectsSubtree(t *testing.T) {
	app := fiber.New()
	pipe := newTestPipeline()
	h := &capturingParamsHandler{}
	app.Get("/users", QueryWithParams(pipe, testFindParamsRequest{}, sparseUser{}.FromResult, h))

	_, _ = app.Test(httptest.NewRequest("GET", "/users?fields=addresses", nil))
	got := h.got.Criteria.Projection
	if v, ok := got["Addresses"]; !ok || v != 1 {
		t.Errorf("expected addresses:1 (whole subtree), got %v", got)
	}
	if v, ok := got["_id"]; !ok || v != 0 {
		t.Errorf("expected _id:0, got %v", got)
	}
}

func TestFieldsParam_PassThroughModeOnQueryParser(t *testing.T) {
	// A map Response keeps the manual parser in pass-through mode (no
	// projection schema is built) → no allowlist, no translation, each token
	// becomes an inclusion entry verbatim. This is the surface that replaced
	// the removed web.ParseCriteria helper.
	parser := NewQueryParser[testFindParamsRequest, map[string]any]()
	app := fiber.New()
	var got queries.ReadCriteria
	app.Get("/x", func(c fiber.Ctx) error {
		got, _, _ = parser.Parse(c)
		return c.SendStatus(fiber.StatusOK)
	})
	_, _ = app.Test(httptest.NewRequest("GET", "/x?fields=foo,bar", nil))
	if v, ok := got.Projection["foo"]; !ok || v != 1 {
		t.Errorf("expected foo:1 in pass-through projection, got %v", got.Projection)
	}
	if v, ok := got.Projection["bar"]; !ok || v != 1 {
		t.Errorf("expected bar:1 in pass-through projection, got %v", got.Projection)
	}
	if _, hasIDExclusion := got.Projection["_id"]; hasIDExclusion {
		t.Errorf("pass-through mode should not add _id:0, got %v", got.Projection)
	}
}
