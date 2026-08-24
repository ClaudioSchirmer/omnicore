package web

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/gofiber/fiber/v3"
)

// QueryParser is the single parsing surface for manual query routes. The
// tests below cover the four observable behaviors the canonical wrapper also
// enforces — boot panic on sparse-render violation, slog.Warn for sort
// opt-in, wire→doc translation at runtime, allowlist rejection of unknown
// tokens — plus the pass-through degradation paths (map Response, no opt-in
// Request).

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
	// The advisory is warn-once per (Request type, sortable path set), and
	// earlier tests in this package construct the same pair — clear the dedup
	// so this test observes the FIRST emission rather than a suppressed one.
	resetSortableWarned(t)

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prev)

	_ = NewQueryParser[testFindParamsRequest, sparseUser]()

	logs := buf.String()
	if !strings.Contains(logs, "query.sortable") {
		t.Fatalf("expected query.sortable warn, log was: %s", logs)
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
	// Request that opts into ?fields= but NOT ?orderBy= → no warn fires.
	type fieldsOnlyRequest struct {
		Name   *string `query:"name"  filter:"eq"`
		Fields *string `query:"fields"`
	}
	resetSortableWarned(t)

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prev)

	_ = NewQueryParser[fieldsOnlyRequest, sparseUser]()

	if strings.Contains(buf.String(), "query.sortable") {
		t.Errorf("did not expect sort opt-in warn when only Fields is declared, log was: %s", buf.String())
	}
}

// TestNewQueryParser_SortOptInWarnIsOncePerShape asserts the advisory does not
// repeat for a Request/Response pair already warned about. The same DTO is
// scanned once per endpoint serving it, which used to print the identical
// multi-kilobyte line several times per boot.
func TestNewQueryParser_SortOptInWarnIsOncePerShape(t *testing.T) {
	resetSortableWarned(t)
	_ = NewQueryParser[testFindParamsRequest, sparseUser]()

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prev)

	_ = NewQueryParser[testFindParamsRequest, sparseUser]()

	if strings.Contains(buf.String(), "query.sortable") {
		t.Errorf("expected the second scan of the same shape to be silent, log was: %s", buf.String())
	}
	// A DIFFERENT sortable surface still gets its own line.
	_ = NewQueryParser[testFindSortOnlyRequest, sparseUser]()
	if !strings.Contains(buf.String(), "query.sortable") {
		t.Errorf("expected a distinct request shape to warn, log was: %s", buf.String())
	}
}

// resetSortableWarned clears the warn-once dedup and restores it when the
// test ends, so tests observing the advisory stay independent of package test
// ordering.
func resetSortableWarned(t *testing.T) {
	t.Helper()
	prev := sortableWarned
	sortableWarned = &sync.Map{}
	t.Cleanup(func() { sortableWarned = prev })
}

// ─── Runtime translation via Parse ─────────────────────────────────────────

func TestQueryParser_Parse_FieldsTranslatesWireToDocAndAddsIDExclusion(t *testing.T) {
	parser := NewQueryParser[testFindParamsRequest, sparseUser]()
	app := fiber.New()
	var crit queries.ReadCriteria
	app.Get("/x", func(c fiber.Ctx) error {
		got, _, _ := parser.Parse(c)
		crit = got
		return c.SendStatus(fiber.StatusOK)
	})

	_, _ = app.Test(httptest.NewRequest("GET", "/x?fields=name,addresses.zipCode", nil))

	if !crit.Projection.Selects("Name") {
		t.Errorf("expected name:1, got %v", crit.Projection)
	}
	if !crit.Projection.Selects("Addresses.ZipCode") {
		t.Errorf("expected addresses.zip_code:1 (auto PascalToSnake), got %v", crit.Projection)
	}
	if crit.Projection.Selects("ID") {
		t.Errorf("id was not requested, so it must not be selected: %v", crit.Projection)
	}
}

func TestQueryParser_Parse_SortTranslatesWireToDocAndHonorsMinusPrefix(t *testing.T) {
	parser := NewQueryParser[testFindParamsRequest, sparseUser]()
	app := fiber.New()
	var crit queries.ReadCriteria
	app.Get("/x", func(c fiber.Ctx) error {
		got, _, _ := parser.Parse(c)
		crit = got
		return c.SendStatus(fiber.StatusOK)
	})

	_, _ = app.Test(httptest.NewRequest("GET", "/x?orderBy=-addresses.zipCode,name", nil))

	if len(crit.OrderBy) != 2 {
		t.Fatalf("expected 2 OrderByFields, got %d (%+v)", len(crit.OrderBy), crit.OrderBy)
	}
	if crit.OrderBy[0].Field != "Addresses.ZipCode" || !crit.OrderBy[0].Desc {
		t.Errorf("expected addresses.zip_code desc, got %+v", crit.OrderBy[0])
	}
	if crit.OrderBy[1].Field != "Name" || crit.OrderBy[1].Desc {
		t.Errorf("expected name asc, got %+v", crit.OrderBy[1])
	}
}

func TestQueryParser_Parse_UnknownFieldsTokenSurfacesBracketedField(t *testing.T) {
	parser := NewQueryParser[testFindParamsRequest, sparseUser]()
	app := fiber.New()
	pipe := newTestPipeline()
	app.Get("/x", func(c fiber.Ctx) error {
		_, v, ok := parser.Parse(c)
		if !ok {
			return RespondSchemaViolation(c, pipe, v.Field)
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
	app.Get("/x", func(c fiber.Ctx) error {
		_, v, ok := parser.Parse(c)
		if !ok {
			return RespondSchemaViolation(c, pipe, v.Field)
		}
		return c.SendStatus(fiber.StatusOK)
	})

	resp, _ := app.Test(httptest.NewRequest("GET", "/x?orderBy=-bogus", nil))
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
	if got := msg["field"]; got != "orderBy[-bogus]" {
		t.Errorf("expected field=orderBy[-bogus], got %v", got)
	}
}

// ─── Pass-through degradation paths ────────────────────────────────────────

func TestNewQueryParser_MapResponseFallsBackToPassThrough(t *testing.T) {
	// Resp = map[string]any → no projection schema is built → the parser
	// degrades to pass-through: tokens land verbatim, no allowlist, no
	// wire→doc translation and no `_id` auto-exclusion.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("did not expect panic for map[string]any Response, got: %v", r)
		}
	}()
	parser := NewQueryParser[testFindParamsRequest, map[string]any]()

	app := fiber.New()
	var crit queries.ReadCriteria
	app.Get("/x", func(c fiber.Ctx) error {
		got, _, _ := parser.Parse(c)
		crit = got
		return c.SendStatus(fiber.StatusOK)
	})
	_, _ = app.Test(httptest.NewRequest("GET", "/x?fields=foo,bar&orderBy=name", nil))

	if !crit.Projection.Selects("foo") {
		t.Errorf("expected foo:1 in pass-through projection, got %v", crit.Projection)
	}
	if crit.Projection.Selects("ID") {
		t.Errorf("pass-through mode must not select an id nobody asked for: %v", crit.Projection)
	}
	// `?fields=` falls back to pass-through on an untyped Response; ordering
	// does not, because its vocabulary is the Request's either way.
	if len(crit.OrderBy) != 1 || crit.OrderBy[0].Field != "Name" {
		t.Errorf("expected OrderByField=Name, got %+v", crit.OrderBy)
	}
}
