package web

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/translation"
	"github.com/gofiber/fiber/v2"
)

func newTestApp() *fiber.App {
	app := fiber.New()
	app.Use(AppContextMiddleware())
	return app
}

// withAuthzEnabled flips the gate's master switch on for the duration of a
// test and restores the previous value via t.Cleanup. The package-level
// flag is otherwise off by default (matching the framework's rollout
// stance), so per-test opt-in is the natural shape.
func withAuthzEnabled(t *testing.T) {
	t.Helper()
	prev := authorizationEnabled()
	SetAuthorizationEnabled(true)
	t.Cleanup(func() { SetAuthorizationEnabled(prev) })
}

func attachIdentity(perms ...string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		AppContext(c).SetIdentity(&configuration.Identity{
			Subject: "tester",
			Claims:  map[string]any{"permissions": perms},
		})
		return c.Next()
	}
}

func TestPermissionGate_PassesWhenIdentityHasPermission(t *testing.T) {
	withAuthzEnabled(t)
	gate := PermissionGate(translation.Default())
	app := newTestApp()
	app.Use(attachIdentity("users:read"))
	app.Get("/x", gate(func(c *fiber.Ctx) error {
		return c.SendStatus(204)
	}, "users:read"))

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/x", nil))
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != 204 {
		t.Errorf("got %d, want 204", resp.StatusCode)
	}
}

func TestPermissionGate_BlocksWhenIdentityMissingPermission(t *testing.T) {
	withAuthzEnabled(t)
	gate := PermissionGate(translation.Default())
	app := newTestApp()
	app.Use(attachIdentity("users:read"))
	app.Get("/x", gate(func(c *fiber.Ctx) error {
		t.Fatal("handler must not be invoked when permission is missing")
		return nil
	}, "users:write"))

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/x", nil))
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != fiber.StatusForbidden {
		t.Errorf("got %d, want 403", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	var env Response
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("envelope unmarshal: %v\nbody=%s", err, body)
	}
	if env.Success {
		t.Error("envelope.success must be false")
	}
	if env.Status != fiber.StatusForbidden {
		t.Errorf("envelope.status = %d, want 403", env.Status)
	}
	if len(env.Errors) != 1 || env.Errors[0].Context != "Authorization" {
		t.Fatalf("expected single Authorization error, got %+v", env.Errors)
	}
	msg := env.Errors[0].Messages[0]
	if msg.NotificationKey != "MissingPermissionNotification" {
		t.Errorf("notificationKey = %q", msg.NotificationKey)
	}
	if msg.Field != "permission" || msg.Value != "users:write" {
		t.Errorf("field/value = %q/%q, want permission/users:write", msg.Field, msg.Value)
	}
	if msg.Semantic != "Forbidden" {
		t.Errorf("semantic = %q, want Forbidden", msg.Semantic)
	}
	if msg.Message != "Missing required permission." {
		t.Errorf("message = %q, want English translation", msg.Message)
	}
}

func TestPermissionGate_BlocksWhenNoIdentity(t *testing.T) {
	withAuthzEnabled(t)
	gate := PermissionGate(translation.Default())
	app := newTestApp()
	// no attachIdentity middleware — Identity stays nil
	app.Get("/x", gate(func(c *fiber.Ctx) error {
		t.Fatal("handler must not run when Identity is nil")
		return nil
	}, "users:read"))

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/x", nil))
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != fiber.StatusForbidden {
		t.Errorf("got %d, want 403", resp.StatusCode)
	}
}

func TestPermissionGate_HonorsAcceptLanguageHeader(t *testing.T) {
	withAuthzEnabled(t)
	gate := PermissionGate(translation.Default())
	app := newTestApp()
	app.Use(attachIdentity("users:read"))
	app.Get("/x", gate(func(c *fiber.Ctx) error {
		return c.SendStatus(204)
	}, "users:write"))

	req := httptest.NewRequest(fiber.MethodGet, "/x", nil)
	req.Header.Set("Accept-Language", "pt-BR")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != fiber.StatusForbidden {
		t.Fatalf("got %d, want 403", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var env Response
	_ = json.Unmarshal(body, &env)
	got := env.Errors[0].Messages[0].Message
	want := "Permissão necessária ausente."
	if got != want {
		t.Errorf("translated message = %q, want %q", got, want)
	}
}

func TestPermissionGate_NoOpsWhenAuthorizationDisabled(t *testing.T) {
	// authorizationEnabled defaults to false; do NOT call withAuthzEnabled.
	prev := authorizationEnabled()
	SetAuthorizationEnabled(false)
	t.Cleanup(func() { SetAuthorizationEnabled(prev) })

	gate := PermissionGate(translation.Default())
	app := newTestApp()
	// no identity at all — would normally trigger 403; with authz off,
	// gate must pass through to the handler.
	called := false
	app.Get("/x", gate(func(c *fiber.Ctx) error {
		called = true
		return c.SendStatus(204)
	}, "users:write"))

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/x", nil))
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != 204 {
		t.Errorf("authz off must no-op the gate; got status %d, want 204", resp.StatusCode)
	}
	if !called {
		t.Error("handler must run when authz off, regardless of permission")
	}
}

func TestPermissionGate_NilTranslatorFallsBackToEnglish(t *testing.T) {
	withAuthzEnabled(t)
	gate := PermissionGate(nil)
	app := newTestApp()
	app.Use(attachIdentity("users:read"))
	app.Get("/x", gate(func(c *fiber.Ctx) error { return c.SendStatus(204) }, "users:write"))

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/x", nil))
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	var env Response
	_ = json.Unmarshal(body, &env)
	if env.Errors[0].Messages[0].Message != "Missing required permission." {
		t.Errorf("expected English fallback, got %q", env.Errors[0].Messages[0].Message)
	}
}
