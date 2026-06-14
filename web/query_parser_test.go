package web

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/gofiber/fiber/v2"
)

// QueryParser closes the asymmetry between HandleQueryWithParams and
// ParseCriteria on manual query routes. The tests below cover the four
// observable behaviors the canonical wrapper already enforces — boot panic
// on sparse-render violation, slog.Warn for sort opt-in, wire→doc
// translation at runtime, allowlist rejection of unknown tokens — plus the
// pass-through degradation paths (RawDoc Response, no opt-in Request).

// ─── Construction-time boot scan ───────────────────────────────────────────

func TestNewQueryParser_PanicsOnSparseContractViolation(t *testing.T) {
	// Same guard the canonical wrapper enforces: Req declares
	// query:"fields", Resp shape has a non-pointer scalar → boot panic.
	// guardNonPointerScalar and testFindParamsRequest live in
	// projection_test.go and handle_query_test.go respectively.
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected boot panic when Resp violates sparse-render contract")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, "guardNonPointerScalar") || !strings.Contains(msg, "name") {
			t.Errorf("expected diagnostic naming type + offending field, got: %s", msg)
		}
	}()
	_ = NewQueryParser[testFindParamsRequest, guardNonPointerScalar]()
}

func TestNewQueryParser_NoPanicWhenFieldsNotOptedIn(t *testing.T) {
	// guardNonPointerScalar would violate the contract IF Fields were
	// declared — testFindSortOnlyRequest declares only Sort, so the
	// fields-side guard never runs and construction succeeds.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("did not expect panic when Fields is not opted in, got: %v", r)
		}
	}()
	_ = NewQueryParser[testFindSortOnlyRequest, guardNonPointerScalar]()
}

func TestNewQueryParser_EmitsSortOptInWarn(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prev)

	_ = NewQueryParser[testFindParamsRequest, sparseUser]()

	logs := buf.String()
	if !strings.Contains(logs, "query.sort.opt-in") {
		t.Fatalf("expected query.sort.opt-in warn, log was: %s", logs)
	}
	// Sortable paths come from extractProjectionSchema(sparseUser); the
	// projection_test already verifies the path map's contents. Spot check
	// that nested paths surface so the operator can compare against index
	// declarations.
	if !strings.Contains(logs, "addresses.zipCode") {
		t.Errorf("expected sortable_wire_paths to include addresses.zipCode, log was: %s", logs)
	}
	if !strings.Contains(logs, "requests=") && !strings.Contains(logs, "request") {
		t.Errorf("expected request type attr on the warn, log was: %s", logs)
	}
}

func TestNewQueryParser_NoWarnWhenSortNotOptedIn(t *testing.T) {
	// Request that opts into ?fields= but NOT ?sort= → no warn fires.
	type fieldsOnlyRequest struct {
		Name   *string `query:"name"  filter:"eq"`
		Fields *string `query:"fields"`
	}
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prev)

	_ = NewQueryParser[fieldsOnlyRequest, sparseUser]()

	if strings.Contains(buf.String(), "query.sort.opt-in") {
		t.Errorf("did not expect sort opt-in warn when only Fields is declared, log was: %s", buf.String())
	}
}

// ─── Runtime translation via Parse ─────────────────────────────────────────

func TestQueryParser_Parse_FieldsTranslatesWireToDocAndAddsIDExclusion(t *testing.T) {
	parser := NewQueryParser[testFindParamsRequest, sparseUser]()
	app := fiber.New()
	var crit queries.ReadCriteria
	app.Get("/x", func(c *fiber.Ctx) error {
		got, _, _ := parser.Parse(c)
		crit = got
		return c.SendStatus(fiber.StatusOK)
	})

	_, _ = app.Test(httptest.NewRequest("GET", "/x?fields=name,addresses.zipCode", nil))

	if v, ok := crit.Projection["name"]; !ok || v != 1 {
		t.Errorf("expected name:1, got %v", crit.Projection)
	}
	if v, ok := crit.Projection["addresses.zip_code"]; !ok || v != 1 {
		t.Errorf("expected addresses.zip_code:1 (auto PascalToSnake), got %v", crit.Projection)
	}
	if v, ok := crit.Projection["_id"]; !ok || v != 0 {
		t.Errorf("expected _id:0 (id not requested), got %v", crit.Projection)
	}
}

func TestQueryParser_Parse_SortTranslatesWireToDocAndHonorsMinusPrefix(t *testing.T) {
	parser := NewQueryParser[testFindParamsRequest, sparseUser]()
	app := fiber.New()
	var crit queries.ReadCriteria
	app.Get("/x", func(c *fiber.Ctx) error {
		got, _, _ := parser.Parse(c)
		crit = got
		return c.SendStatus(fiber.StatusOK)
	})

	_, _ = app.Test(httptest.NewRequest("GET", "/x?sort=-addresses.zipCode,name", nil))

	if len(crit.Sort) != 2 {
		t.Fatalf("expected 2 SortFields, got %d (%+v)", len(crit.Sort), crit.Sort)
	}
	if crit.Sort[0].Field != "addresses.zip_code" || !crit.Sort[0].Desc {
		t.Errorf("expected addresses.zip_code desc, got %+v", crit.Sort[0])
	}
	if crit.Sort[1].Field != "name" || crit.Sort[1].Desc {
		t.Errorf("expected name asc, got %+v", crit.Sort[1])
	}
}

func TestQueryParser_Parse_UnknownFieldsTokenSurfacesBracketedField(t *testing.T) {
	parser := NewQueryParser[testFindParamsRequest, sparseUser]()
	app := fiber.New()
	pipe := newTestPipeline()
	app.Get("/x", func(c *fiber.Ctx) error {
		_, badField, ok := parser.Parse(c)
		if !ok {
			return RespondSchemaViolation(c, pipe, badField)
		}
		return c.SendStatus(fiber.StatusOK)
	})

	resp, _ := app.Test(httptest.NewRequest("GET", "/x?fields=name,bogus", nil))
	if resp.StatusCode != fiber.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 400 from unknown fields token, got %d (body=%s)", resp.StatusCode, body)
	}
	body, _ := io.ReadAll(resp.Body)
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, body)
	}
	errs := parsed["errors"].([]any)
	msg := errs[0].(map[string]any)["messages"].([]any)[0].(map[string]any)
	if got := msg["field"]; got != "fields[bogus]" {
		t.Errorf("expected field=fields[bogus], got %v", got)
	}
}

func TestQueryParser_Parse_UnknownSortTokenSurfacesBracketedField(t *testing.T) {
	parser := NewQueryParser[testFindParamsRequest, sparseUser]()
	app := fiber.New()
	pipe := newTestPipeline()
	app.Get("/x", func(c *fiber.Ctx) error {
		_, badField, ok := parser.Parse(c)
		if !ok {
			return RespondSchemaViolation(c, pipe, badField)
		}
		return c.SendStatus(fiber.StatusOK)
	})

	resp, _ := app.Test(httptest.NewRequest("GET", "/x?sort=-bogus", nil))
	if resp.StatusCode != fiber.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 400 from unknown sort token, got %d (body=%s)", resp.StatusCode, body)
	}
	body, _ := io.ReadAll(resp.Body)
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, body)
	}
	errs := parsed["errors"].([]any)
	msg := errs[0].(map[string]any)["messages"].([]any)[0].(map[string]any)
	// The `-` prefix is preserved verbatim — matches the canonical wrapper.
	if got := msg["field"]; got != "sort[-bogus]" {
		t.Errorf("expected field=sort[-bogus], got %v", got)
	}
}

// ─── Pass-through degradation paths ────────────────────────────────────────

func TestNewQueryParser_RawDocResponseFallsBackToPassThrough(t *testing.T) {
	// Resp = map[string]any → projSchema stays nil → parser behavior is
	// identical to ParseCriteria (tokens land verbatim, no allowlist).
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("did not expect panic for map[string]any Response, got: %v", r)
		}
	}()
	parser := NewQueryParser[testFindParamsRequest, map[string]any]()

	app := fiber.New()
	var crit queries.ReadCriteria
	app.Get("/x", func(c *fiber.Ctx) error {
		got, _, _ := parser.Parse(c)
		crit = got
		return c.SendStatus(fiber.StatusOK)
	})
	_, _ = app.Test(httptest.NewRequest("GET", "/x?fields=foo,bar&sort=anything", nil))

	if v, ok := crit.Projection["foo"]; !ok || v != 1 {
		t.Errorf("expected foo:1 in pass-through projection, got %v", crit.Projection)
	}
	if _, hasIDExclusion := crit.Projection["_id"]; hasIDExclusion {
		t.Errorf("pass-through mode should not add _id:0, got %v", crit.Projection)
	}
	if len(crit.Sort) != 1 || crit.Sort[0].Field != "anything" {
		t.Errorf("expected SortField=anything verbatim, got %+v", crit.Sort)
	}
}
