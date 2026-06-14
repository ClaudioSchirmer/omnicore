package web

import (
	"net/http/httptest"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
)

func TestAppContextMiddleware_NoHeaders_GeneratesUUID(t *testing.T) {
	app := fiber.New()
	app.Use(AppContextMiddleware())
	app.Get("/test", func(c *fiber.Ctx) error {
		ctx := AppContext(c)
		c.Set("Test-ID", ctx.ID().String())
		return c.SendStatus(200)
	})

	resp, err := app.Test(httptest.NewRequest("GET", "/test", nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	reqID := resp.Header.Get("X-Request-ID")
	testID := resp.Header.Get("Test-ID")
	if reqID == "" {
		t.Fatal("expected X-Request-ID header set on response")
	}
	if reqID != testID {
		t.Fatalf("X-Request-ID (%q) != Test-ID (%q)", reqID, testID)
	}
	if _, err := uuid.Parse(reqID); err != nil {
		t.Fatalf("X-Request-ID is not a valid UUID: %q (%v)", reqID, err)
	}
}

func TestAppContextMiddleware_ValidRequestIDPreserved(t *testing.T) {
	app := fiber.New()
	app.Use(AppContextMiddleware())
	app.Get("/test", func(c *fiber.Ctx) error {
		ctx := AppContext(c)
		c.Set("Test-ID", ctx.ID().String())
		return c.SendStatus(200)
	})

	want := uuid.New().String()
	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("X-Request-ID", want)

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("X-Request-ID"); got != want {
		t.Fatalf("response X-Request-ID = %q, want %q", got, want)
	}
	if got := resp.Header.Get("Test-ID"); got != want {
		t.Fatalf("ctx.ID() = %q, want %q", got, want)
	}
}

func TestAppContextMiddleware_InvalidRequestIDFallsBackToNew(t *testing.T) {
	app := fiber.New()
	app.Use(AppContextMiddleware())
	app.Get("/test", func(c *fiber.Ctx) error { return c.SendStatus(200) })

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("X-Request-ID", "not-a-uuid")

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	got := resp.Header.Get("X-Request-ID")
	if got == "" || got == "not-a-uuid" {
		t.Fatalf("expected new UUID, got %q", got)
	}
	if _, err := uuid.Parse(got); err != nil {
		t.Fatalf("response X-Request-ID is not a valid UUID: %q (%v)", got, err)
	}
}

func TestAppContextMiddleware_AcceptLanguage(t *testing.T) {
	cases := []struct {
		header string
		want   configuration.Language
	}{
		{"", configuration.LangENG}, // absent → English (canonical default)
		{"pt-BR", configuration.LangPTBR},
		{"PT-br", configuration.LangPTBR},
		{"en-US", configuration.LangENG},
		{"en", configuration.LangENG},
		{"es", configuration.LangES},
		{"fr-FR", configuration.LangFR},
		{"ja-JP", configuration.LangENG}, // not supported → English fallback
	}

	for _, tc := range cases {
		t.Run(tc.header, func(t *testing.T) {
			var got configuration.Language
			app := fiber.New()
			app.Use(AppContextMiddleware())
			app.Get("/test", func(c *fiber.Ctx) error {
				got = AppContext(c).Language()
				return c.SendStatus(200)
			})

			req := httptest.NewRequest("GET", "/test", nil)
			if tc.header != "" {
				req.Header.Set("Accept-Language", tc.header)
			}
			resp, err := app.Test(req)
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}
			resp.Body.Close()

			if got != tc.want {
				t.Fatalf("Accept-Language=%q → Language=%v, want %v", tc.header, got, tc.want)
			}
		})
	}
}

func TestAppContext_NoMiddlewareFallback(t *testing.T) {
	var ctx *configuration.AppContext

	app := fiber.New()
	app.Get("/test", func(c *fiber.Ctx) error {
		ctx = AppContext(c)
		return c.SendStatus(200)
	})

	resp, err := app.Test(httptest.NewRequest("GET", "/test", nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if ctx == nil {
		t.Fatal("expected fallback AppContext, got nil")
	}
	if ctx.Language() != configuration.LangENG {
		t.Fatalf("fallback Language = %v, want LangENG", ctx.Language())
	}
	if ctx.ID() == (uuid.UUID{}) {
		t.Fatal("fallback AppContext.ID() is zero UUID")
	}
}
