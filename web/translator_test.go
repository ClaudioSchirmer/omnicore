package web

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/translation"
	"github.com/gofiber/fiber/v2"
)

// resetTranslator restores the registered translator after the test. SetTranslator
// mutates package-level state; parallel/subsequent tests would observe drift
// without the cleanup.
func resetTranslator(t *testing.T) {
	t.Helper()
	prev := registeredTranslator()
	t.Cleanup(func() { SetTranslator(prev) })
}

func TestRespondWithInternalServerError_NilTranslatorEnglishFallback(t *testing.T) {
	resetTranslator(t)
	SetTranslator(nil)

	app := fiber.New()
	app.Use(AppContextMiddleware())
	app.Get("/x", func(c *fiber.Ctx) error {
		return RespondWithInternalServerError(c)
	})

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/x", nil))
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != fiber.StatusInternalServerError {
		t.Fatalf("got %d, want 500", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var env Response
	_ = json.Unmarshal(body, &env)
	if env.Errors[0].Messages[0].Message != "Internal server error." {
		t.Errorf("expected English fallback, got %q", env.Errors[0].Messages[0].Message)
	}
}

func TestRespondWithInternalServerError_TranslatesViaRegisteredTranslator(t *testing.T) {
	resetTranslator(t)
	SetTranslator(translation.Default())

	app := fiber.New()
	app.Use(AppContextMiddleware())
	app.Get("/x", func(c *fiber.Ctx) error {
		return RespondWithInternalServerError(c)
	})

	req := httptest.NewRequest(fiber.MethodGet, "/x", nil)
	req.Header.Set("Accept-Language", "pt-BR")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	var env Response
	_ = json.Unmarshal(body, &env)
	if env.Errors[0].Messages[0].Message != "Erro interno do servidor." {
		t.Errorf("PT-BR translation expected, got %q", env.Errors[0].Messages[0].Message)
	}
}

func TestSetTranslator_RoundTrips(t *testing.T) {
	resetTranslator(t)
	tr := translation.Default()
	SetTranslator(tr)
	if got := registeredTranslator(); got != tr {
		t.Errorf("registered translator mismatch: got %p, want %p", got, tr)
	}
	SetTranslator(nil)
	if got := registeredTranslator(); got != nil {
		t.Errorf("SetTranslator(nil) should clear, got %p", got)
	}
}
