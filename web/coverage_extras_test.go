package web

import (
	"io"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
)

// ─── export `?orderBy=`: one vocabulary with the JSON listing ────────────────
//
// The export no longer parses ordering against a view-derived path map; it
// runs the SAME queryschema.ParseOrderByWithSchema the JSON listing uses,
// against the SAME Response projection schema. The token grammar itself
// (`-` prefix, empty-token skip, unknown token reported verbatim) is covered
// where that parser lives; what matters here is that the export ROUTE speaks
// that vocabulary — a Response-declared wire path sorts, and an undeclared
// one is the canonical 400 with the `orderBy[<token>]` field spelling.

func TestHandleQueryAsCSV_OrderByUsesTheResponseVocabulary(t *testing.T) {
	app := fiber.New()
	h := newExportHandler()
	mountCSV(app, h)

	// `name` is a Response wire path; `-` marks descending. It must reach the
	// criteria translated to the Go field path the reader consumes.
	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/users.csv?orderBy=-name", nil))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	crit := h.got.Criteria
	if len(crit.OrderBy) != 1 || crit.OrderBy[0].Field != "Name" || !crit.OrderBy[0].Desc {
		t.Fatalf("orderBy did not translate to the Go field path: %+v", crit.OrderBy)
	}
}

func TestHandleQueryAsCSV_OrderByUndeclaredToken400(t *testing.T) {
	app := fiber.New()
	mountCSV(app, newExportHandler())

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/users.csv?orderBy=bogus", nil))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "orderBy[bogus]") {
		t.Errorf("expected the canonical orderBy[bogus] spelling, got %s", body)
	}
}

// ─── buildKeyfunc: the PEM-parse-error branch ────────────────────────────────

func TestBuildKeyfunc_InvalidPEMReturnsError(t *testing.T) {
	_, err := buildKeyfunc(AuthOptions{PublicKeyPEM: "not a real pem"})
	if err == nil {
		t.Fatal("expected error from an unparsable PEM public key")
	}
}

// ─── formatPathIDConflict: the rendered diagnostic ───────────────────────────

func TestFormatPathIDConflict_MentionsWrapperAndRequest(t *testing.T) {
	msg := formatPathIDConflict("CommandWithBodyID", reflect.TypeOf(struct{ X int }{}))
	if !strings.Contains(msg, "CommandWithBodyID") {
		t.Errorf("diagnostic must name the wrapper: %s", msg)
	}
	if !strings.Contains(msg, `path:"id"`) {
		t.Errorf("diagnostic must mention the path:%q tag: %s", "id", msg)
	}
}
