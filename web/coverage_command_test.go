package web

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	fwresults "github.com/ClaudioSchirmer/omnicore/application/results"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/web/responses"
	"github.com/gofiber/fiber/v3"
)

// strictInsertHandler embeds pipeline.FullBody so CommandWithBody runs
// the strict missing-field check on *testInsertCmd (POST is normally lenient;
// the marker forces all Request fields mandatory).
type strictInsertHandler struct {
	pipeline.FullBody
	called bool
}

func (h *strictInsertHandler) Handle(ctx *configuration.AppContext, cmd *testInsertCmd) (fwresults.None, error) {
	h.called = true
	return fwresults.None{}, nil
}

func mountStrictInsert(app *fiber.App, h *strictInsertHandler) {
	pipe := newTestPipeline()
	app.Post("/things", CommandWithBody(pipe, testInsertRequest{}, responses.NoBody, h, fiber.StatusCreated))
}

func TestHandleCommandWithBody_Strict_EmptyBody_400(t *testing.T) {
	app := fiber.New()
	h := &strictInsertHandler{}
	mountStrictInsert(app, h)

	resp, _ := app.Test(httptest.NewRequest("POST", "/things", nil))
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("expected 400 for empty body under strict, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "RequiredFieldNotification") {
		t.Errorf("expected RequiredFieldNotification, got: %s", body)
	}
	if !strings.Contains(string(body), `"context":"Schema"`) {
		t.Errorf("expected context Schema, got: %s", body)
	}
	if h.called {
		t.Error("handler must not run when strict body check fails")
	}
}

func TestHandleCommandWithBody_Strict_MissingField_400(t *testing.T) {
	app := fiber.New()
	h := &strictInsertHandler{}
	mountStrictInsert(app, h)

	body, _ := json.Marshal(map[string]string{"name": "alice"}) // email + phone missing
	req := httptest.NewRequest("POST", "/things", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, _ := app.Test(req)
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("expected 400 for missing field under strict, got %d", resp.StatusCode)
	}
	out, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(out), `"field":"email"`) {
		t.Errorf("expected missing email field, got: %s", out)
	}
}

func TestHandleCommandWithBody_Strict_MalformedJSON_400(t *testing.T) {
	app := fiber.New()
	h := &strictInsertHandler{}
	mountStrictInsert(app, h)

	req := httptest.NewRequest("POST", "/things", bytes.NewReader([]byte("{not json")))
	req.Header.Set("Content-Type", "application/json")
	resp, _ := app.Test(req)
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("expected 400 for malformed JSON under strict, got %d", resp.StatusCode)
	}
	out, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(out), "SchemaViolationNotification") {
		t.Errorf("expected SchemaViolationNotification, got: %s", out)
	}
}

// pathIDRequiredInsertHandler embeds pipeline.PathIDRequired so a Group A
// wrapper (CommandWithBody, no :id auto-bind) paired with it AND a
// Request with no path: tag triggers warnGroupAMissingPathTag at construction.
type pathIDRequiredInsertHandler struct {
	pipeline.PathIDRequired
}

func (h *pathIDRequiredInsertHandler) Handle(ctx *configuration.AppContext, cmd *testInsertCmd) (fwresults.None, error) {
	return fwresults.None{}, nil
}

func TestHandleCommandWithBody_WarnsWhenPathIDHandlerHasNoPathTag(t *testing.T) {
	// Construction is enough — warnGroupAMissingPathTag fires the slog.Warn.
	// The test asserts construction does not panic and returns a usable handler.
	pipe := newTestPipeline()
	got := CommandWithBody(pipe, testInsertRequest{}, responses.NoBody,
		&pathIDRequiredInsertHandler{}, fiber.StatusCreated)
	if got == nil {
		t.Fatal("expected a handler even when the path-ID-source warning fires")
	}
}

// ─── formatPathIDConflict panic path ─────────────────────────────────────────

type pathIDConflictCmd struct {
	pipeline.CommandByIDBase
}

type pathIDConflictRequest struct {
	ID string `path:"id"`
}

func (r pathIDConflictRequest) ToCommand() *pathIDConflictCmd { return &pathIDConflictCmd{} }

func TestHandleCommandWithBodyID_PanicsOnPathIDTag(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic when the Request declares path:\"id\" on a :id-binding wrapper")
		}
		if !strings.Contains(strings.ToLower(toStr(r)), "path") {
			t.Fatalf("panic message should mention the path conflict, got %v", r)
		}
	}()
	pipe := newTestPipeline()
	_ = CommandWithBodyID(pipe, pathIDConflictRequest{}, responses.NoBody,
		&capturingPathIDConflictHandler{}, fiber.StatusOK)
}

type capturingPathIDConflictHandler struct{}

func (h *capturingPathIDConflictHandler) Handle(ctx *configuration.AppContext, cmd *pathIDConflictCmd) (fwresults.None, error) {
	return fwresults.None{}, nil
}

func toStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if e, ok := v.(error); ok {
		return e.Error()
	}
	return ""
}

// ─── respondCarrier via ErrorHandler ─────────────────────────────────────────

// A NotificationCarrier error escaping a route (not via RespondFromResult)
// must be funneled through respondCarrier and emitted as the canonical
// envelope with the status derived from the carrier's Semantic.
func TestErrorHandler_CarrierEscapes_RespondCarrier(t *testing.T) {
	app := newAppWithErrorHandler()
	app.Get("/x", func(c fiber.Ctx) error {
		return domain.SingleNotificationError("User", "id", domain.RecordNotFoundNotification{})
	})

	resp, err := app.Test(httptest.NewRequest("GET", "/x", nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != fiber.StatusNotFound {
		t.Fatalf("RecordNotFound carrier should map to 404, got %d", resp.StatusCode)
	}
	body := decodeResponse(t, resp.Body)
	resp.Body.Close()
	if body.Success {
		t.Fatal("carrier escape should not be success")
	}
	if len(body.Errors) != 1 || body.Errors[0].Messages[0].NotificationKey != "RecordNotFoundNotification" {
		t.Fatalf("expected RecordNotFoundNotification envelope, got %+v", body)
	}
}

