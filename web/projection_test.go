package web

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/gofiber/fiber/v3"
)

// ─── projectionSchema (extractProjectionSchema + walkProjectionLevel) ─────

type sparseAddress struct {
	ID      *string `json:"id,omitempty"`
	City    *string `json:"city,omitempty"`
	ZipCode *string `json:"zipCode,omitempty"`
	State   *string `json:"state,omitempty" view:"st"`
}

type sparseUser struct {
	ID        *string         `json:"id,omitempty"`
	Name      *string         `json:"name,omitempty"`
	Email     *string         `json:"email,omitempty"`
	Phone     *string         `json:"phone,omitempty"`
	Addresses []sparseAddress `json:"addresses,omitempty"`
}

func TestExtractProjectionSchema_TopLevelPathsUseSnakeCase(t *testing.T) {
	s := extractProjectionSchema(reflect.TypeOf(sparseUser{}))
	if got := s.paths["id"]; got != "ID" {
		t.Errorf("id → %q, want id", got)
	}
	if got := s.paths["name"]; got != "Name" {
		t.Errorf("name → %q, want name", got)
	}
}

func TestExtractProjectionSchema_NestedSliceOfStructPathsTranslated(t *testing.T) {
	s := extractProjectionSchema(reflect.TypeOf(sparseUser{}))
	if got := s.paths["addresses"]; got != "Addresses" {
		t.Errorf("addresses → %q, want addresses", got)
	}
	if got := s.paths["addresses.id"]; got != "Addresses.ID" {
		t.Errorf("addresses.id → %q, want addresses.id", got)
	}
	if got := s.paths["addresses.city"]; got != "Addresses.City" {
		t.Errorf("addresses.city → %q, want addresses.city", got)
	}
	if got := s.paths["addresses.zipCode"]; got != "Addresses.ZipCode" {
		t.Errorf("addresses.zipCode → %q, want addresses.zip_code (Go field path)", got)
	}
}

func TestExtractProjectionSchema_ViewTagOverridesAutoSnake(t *testing.T) {
	s := extractProjectionSchema(reflect.TypeOf(sparseUser{}))
	if got := s.paths["addresses.state"]; got != "Addresses.State" {
		t.Errorf("addresses.state → %q, want addresses.st (Go field path (view: removed))", got)
	}
}

func TestExtractProjectionSchema_CachedByReflectType(t *testing.T) {
	s1 := extractProjectionSchema(reflect.TypeOf(sparseUser{}))
	s2 := extractProjectionSchema(reflect.TypeOf(sparseUser{}))
	if s1 != s2 {
		t.Errorf("expected the same *projectionSchema pointer on the second call (cache hit)")
	}
}

// ─── validateFieldsResponse (boot guard) ───────────────────────────────────

func TestValidateFieldsResponse_AcceptsPointerWithOmitempty(t *testing.T) {
	errs := validateFieldsResponse(reflect.TypeOf(sparseUser{}))
	if len(errs) != 0 {
		t.Errorf("expected no violations, got %v", errs)
	}
}

type guardMissingOmitempty struct {
	Name *string `json:"name"`
}

func TestValidateFieldsResponse_RejectsMissingOmitempty(t *testing.T) {
	errs := validateFieldsResponse(reflect.TypeOf(guardMissingOmitempty{}))
	if len(errs) == 0 {
		t.Fatal("expected violations for missing ,omitempty")
	}
	joined := strings.Join(errs, "\n")
	if !strings.Contains(joined, "name") || !strings.Contains(joined, "omitempty") {
		t.Errorf("expected diagnostic to mention name + omitempty, got: %s", joined)
	}
}

type guardNonPointerScalar struct {
	Name string `json:"name,omitempty"`
}

func TestValidateFieldsResponse_RejectsNonPointerScalar(t *testing.T) {
	errs := validateFieldsResponse(reflect.TypeOf(guardNonPointerScalar{}))
	if len(errs) == 0 {
		t.Fatal("expected violations for non-pointer scalar field")
	}
	joined := strings.Join(errs, "\n")
	if !strings.Contains(joined, "name") || !strings.Contains(joined, "must be") {
		t.Errorf("expected diagnostic to demand pointer for name, got: %s", joined)
	}
}

type guardNestedBad struct {
	ID *string             `json:"id,omitempty"`
	A  []guardNestedItemA  `json:"a,omitempty"`
}

type guardNestedItemA struct {
	Label string `json:"label,omitempty"` // violation — non-pointer scalar at depth 2
}

func TestValidateFieldsResponse_RecursesIntoSliceOfStruct(t *testing.T) {
	errs := validateFieldsResponse(reflect.TypeOf(guardNestedBad{}))
	if len(errs) == 0 {
		t.Fatal("expected nested violation on a[].label")
	}
	joined := strings.Join(errs, "\n")
	if !strings.Contains(joined, "a.label") {
		t.Errorf("expected diagnostic to mention path a.label, got: %s", joined)
	}
}

func TestValidateFieldsResponse_JSONHyphenSkipsField(t *testing.T) {
	type withSkip struct {
		Name   *string `json:"name,omitempty"`
		Hidden string  `json:"-"`
	}
	errs := validateFieldsResponse(reflect.TypeOf(withSkip{}))
	if len(errs) != 0 {
		t.Errorf("expected no violations (json:- skipped), got %v", errs)
	}
}

// ─── HandleQueryWithParams boot guard integration ──────────────────────────

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
	app.Get("/x", HandleQueryWithParams(pipe, testFindParamsRequest{}, func(_ map[string]any) guardNonPointerScalar {
		return guardNonPointerScalar{}
	}, h))
}

// ─── runtime ?fields= behavior ─────────────────────────────────────────────

func TestFieldsParam_UnknownTokenReturns400WithBracketedField(t *testing.T) {
	app := fiber.New()
	pipe := newTestPipeline()
	h := &capturingParamsHandler{}
	app.Get("/users", HandleQueryWithParams(pipe, testFindParamsRequest{}, func(_ map[string]any) sparseUser {
		return sparseUser{}
	}, h))

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
	app.Get("/users", HandleQueryWithParams(pipe, testFindParamsRequest{}, func(_ map[string]any) sparseUser {
		return sparseUser{}
	}, h))

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
	app.Get("/users", HandleQueryWithParams(pipe, testFindParamsRequest{}, func(_ map[string]any) sparseUser {
		return sparseUser{}
	}, h))

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
	app.Get("/users", HandleQueryWithParams(pipe, testFindParamsRequest{}, func(_ map[string]any) sparseUser {
		return sparseUser{}
	}, h))

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
	app.Get("/users", HandleQueryWithParams(pipe, testFindParamsRequest{}, func(_ map[string]any) sparseUser {
		return sparseUser{}
	}, h))

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
	app.Get("/users", HandleQueryWithParams(pipe, testFindParamsRequest{}, func(_ map[string]any) sparseUser {
		return sparseUser{}
	}, h))

	_, _ = app.Test(httptest.NewRequest("GET", "/users?fields=addresses", nil))
	got := h.got.Criteria.Projection
	if v, ok := got["Addresses"]; !ok || v != 1 {
		t.Errorf("expected addresses:1 (whole subtree), got %v", got)
	}
	if v, ok := got["_id"]; !ok || v != 0 {
		t.Errorf("expected _id:0, got %v", got)
	}
}

func TestFieldsParam_PassThroughModeOnParseCriteria(t *testing.T) {
	// ParseCriteria passes nil projSchema → no allowlist, no translation,
	// each token becomes an inclusion entry verbatim.
	app := fiber.New()
	var got queries.ReadCriteria
	app.Get("/x", func(c fiber.Ctx) error {
		got, _, _ = ParseCriteria(c, testFindParamsRequest{})
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

