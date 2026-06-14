package web

import (
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/translation"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/gofiber/fiber/v2"
)

// --- middleware.go ---------------------------------------------------------

func TestCORS_DefaultsAndOverride(t *testing.T) {
	if h := CORS(); h == nil {
		t.Error("CORS() should return non-nil handler")
	}
	if h := CORS("https://app.example"); h == nil {
		t.Error("CORS(origin) should return non-nil handler")
	}
}

func TestLogger_HandlerExists(t *testing.T) {
	if Logger() == nil {
		t.Error("Logger() should return non-nil handler")
	}
}

func TestRateLimit_HandlerExists(t *testing.T) {
	if RateLimit(10) == nil {
		t.Error("RateLimit(10) should return non-nil handler")
	}
}

func TestRecover_Handler_RecoversPanic(t *testing.T) {
	app := fiber.New(fiber.Config{ErrorHandler: func(c *fiber.Ctx, err error) error {
		return c.Status(fiber.StatusInternalServerError).SendString("recovered: " + err.Error())
	}})
	app.Use(Recover())
	app.Get("/boom", func(c *fiber.Ctx) error { panic("kaboom") })

	req := httptest.NewRequest(fiber.MethodGet, "/boom", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != fiber.StatusInternalServerError {
		t.Errorf("expected 500 after panic recovery, got %d", resp.StatusCode)
	}
}

// --- parse.go -------------------------------------------------------------

func TestParseBody_ReturnsFields(t *testing.T) {
	app := fiber.New()
	app.Post("/x", func(c *fiber.Ctx) error {
		f, err := ParseBody(c)
		if err != nil {
			return c.Status(fiber.StatusBadRequest).SendString("parse: " + err.Error())
		}
		buf, _ := json.Marshal(f)
		return c.Send(buf)
	})

	req := httptest.NewRequest(fiber.MethodPost, "/x", strings.NewReader(`{"name":"alice","age":30}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	var got domain.Fields
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal response: %v\nbody=%s", err, body)
	}
	if got["name"] != "alice" {
		t.Errorf("ParseBody name = %v, want alice", got["name"])
	}
}

func TestParseBody_BadJSONReturnsError(t *testing.T) {
	app := fiber.New()
	saw := false
	app.Post("/x", func(c *fiber.Ctx) error {
		_, err := ParseBody(c)
		if err != nil {
			saw = true
		}
		return c.SendStatus(fiber.StatusOK)
	})
	req := httptest.NewRequest(fiber.MethodPost, "/x", strings.NewReader(`not-json`))
	req.Header.Set("Content-Type", "application/json")
	if _, err := app.Test(req); err != nil {
		t.Fatalf("Test: %v", err)
	}
	if !saw {
		t.Error("expected ParseBody to surface json error")
	}
}

func TestParseID(t *testing.T) {
	app := fiber.New()
	app.Get("/users/:id", func(c *fiber.Ctx) error {
		return c.SendString(ParseID(c))
	})
	req := httptest.NewRequest(fiber.MethodGet, "/users/abc-123", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "abc-123" {
		t.Errorf("ParseID returned %q, want abc-123", body)
	}
}

// --- response.go: status helpers ------------------------------------------

func TestRespondWithBadRequest(t *testing.T) {
	app := fiber.New()
	app.Get("/x", func(c *fiber.Ctx) error { return RespondWithBadRequest(c) })
	resp, _ := app.Test(httptest.NewRequest(fiber.MethodGet, "/x", nil))
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestRespondWithUnauthorized(t *testing.T) {
	app := fiber.New()
	app.Get("/x", func(c *fiber.Ctx) error { return RespondWithUnauthorized(c) })
	resp, _ := app.Test(httptest.NewRequest(fiber.MethodGet, "/x", nil))
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestRespondWithForbidden(t *testing.T) {
	app := fiber.New()
	app.Get("/x", func(c *fiber.Ctx) error { return RespondWithForbidden(c) })
	resp, _ := app.Test(httptest.NewRequest(fiber.MethodGet, "/x", nil))
	if resp.StatusCode != fiber.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
}

func TestRespondWithNotFound(t *testing.T) {
	app := fiber.New()
	app.Get("/x", func(c *fiber.Ctx) error { return RespondWithNotFound(c) })
	resp, _ := app.Test(httptest.NewRequest(fiber.MethodGet, "/x", nil))
	if resp.StatusCode != fiber.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// --- from_error.go --------------------------------------------------------

func TestResponseFromError_Nil(t *testing.T) {
	resp := ResponseFromError(nil, nil, configuration.LangENG)
	if !resp.Success || resp.Status != fiber.StatusOK {
		t.Errorf("nil error = %+v, want success 200", resp)
	}
}

func TestResponseFromError_CarrierWithTranslator(t *testing.T) {
	tr := translation.Default()
	cause := domain.SingleNotificationError("User", "id", domain.RecordNotFoundNotification{})
	resp := ResponseFromError(cause, tr, configuration.LangENG)
	if resp.Success {
		t.Error("carrier error should NOT be success")
	}
	if resp.Status != fiber.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", resp.Status)
	}
	if len(resp.Errors) != 1 {
		t.Errorf("expected 1 error context, got %d", len(resp.Errors))
	}
}

func TestResponseFromError_CarrierWithoutTranslator(t *testing.T) {
	cause := domain.SingleNotificationError("User", "email", domain.RequiredFieldNotification{})
	resp := ResponseFromError(cause, nil, configuration.LangENG)
	if resp.Success {
		t.Error("carrier should NOT be success")
	}
	if len(resp.Errors) != 1 {
		t.Errorf("expected 1 error context, got %d", len(resp.Errors))
	}
}

func TestResponseFromError_RawErrorIs500(t *testing.T) {
	resp := ResponseFromError(errors.New("downstream"), nil, configuration.LangENG)
	if resp.Status != fiber.StatusInternalServerError {
		t.Errorf("status = %d, want 500", resp.Status)
	}
}

func TestRespondFromError_NilEndToEnd(t *testing.T) {
	app := fiber.New()
	app.Get("/x", func(c *fiber.Ctx) error {
		return RespondFromError(c, nil, nil, configuration.LangENG)
	})
	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/x", nil))
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestRespondFromError_CarrierEndToEnd(t *testing.T) {
	tr := translation.Default()
	app := fiber.New()
	app.Get("/x", func(c *fiber.Ctx) error {
		return RespondFromError(c,
			domain.NotFoundError("User", "id", "abc"),
			tr, configuration.LangENG)
	})
	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/x", nil))
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != fiber.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", resp.StatusCode)
	}
}

// --- from_notifications.go: ResponseFromContexts (no translator path) ---

func TestResponseFromContexts_UsesNotificationKeyAsMessage(t *testing.T) {
	ctx := domain.NewNotificationContext("User")
	ctx.AddNotificationMessage(domain.NotificationMessage{
		FieldName:    "id",
		FieldValue:   "abc",
		Notification: domain.RecordNotFoundNotification{},
	})
	resp := ResponseFromContexts([]*domain.NotificationContext{ctx}, fiber.StatusNotFound, "")
	if resp.Status != fiber.StatusNotFound {
		t.Errorf("status = %d", resp.Status)
	}
	if len(resp.Errors) != 1 {
		t.Fatalf("expected 1 error ctx, got %d", len(resp.Errors))
	}
	if resp.Errors[0].Messages[0].Message != "RecordNotFoundNotification" {
		t.Errorf("Message should default to NotificationKey when no translator, got %q",
			resp.Errors[0].Messages[0].Message)
	}
}

func TestResponseFromContexts_FillsDescriptionFromStatusText(t *testing.T) {
	resp := ResponseFromContexts(nil, fiber.StatusUnprocessableEntity, "")
	if resp.Description == "" {
		t.Error("expected Description to be filled from http.StatusText")
	}
}

func TestResponseFromContexts_SkipsNilContexts(t *testing.T) {
	resp := ResponseFromContexts([]*domain.NotificationContext{nil}, fiber.StatusBadRequest, "")
	if len(resp.Errors) != 0 {
		t.Errorf("expected nil contexts to be skipped, got %d errors", len(resp.Errors))
	}
}
