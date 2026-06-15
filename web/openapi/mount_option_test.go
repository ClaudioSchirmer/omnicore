package openapi

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
)

// resetGate restores no-gate state after a test. Required because SetGate
// mutates package-level state; without resetting, parallel/subsequent tests
// would observe stale gates.
func resetGate(t *testing.T) {
	t.Helper()
	prev := resolveGate()
	t.Cleanup(func() { SetGate(prev) })
}

// noopGate is the smallest valid Gate — passes the handler through unchanged.
// Used by tests that need a registered gate without exercising real auth.
func noopGate(handler fiber.Handler, _ string) fiber.Handler { return handler }

// blockingGate returns a 403-like 999 status so tests can detect that the
// gate, not the underlying handler, served the request.
func blockingGate(_ fiber.Handler, _ string) fiber.Handler {
	return func(c fiber.Ctx) error { return c.SendStatus(999) }
}

func TestRequirePermission_EmptyPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("RequirePermission(\"\") must panic")
		}
	}()
	RequirePermission("")
}

func TestRequirePermission_NoColonPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("RequirePermission(\"usersread\") must panic")
		}
	}()
	RequirePermission("usersread")
}

func TestRequirePermission_WildcardPanics(t *testing.T) {
	cases := []string{"users:*", "*:read", "*:*", "us*ers:read"}
	for _, p := range cases {
		t.Run(p, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("RequirePermission(%q) must panic", p)
				}
			}()
			RequirePermission(p)
		})
	}
}

func TestRequirePermission_DuplicatePanics(t *testing.T) {
	resetGate(t)
	SetGate(noopGate)
	app := fiber.New()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("duplicate RequirePermission must panic")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, "duplicate") {
			t.Errorf("panic message should mention 'duplicate', got %q", msg)
		}
	}()
	Mount(nil, app, fiber.MethodGet, "/x",
		func(c fiber.Ctx) error { return c.SendStatus(200) },
		RouteSpec{}, Doc{},
		RequirePermission("users:read"), RequirePermission("users:write"))
}

func TestMount_PublicPlusRequirePermissionPanics(t *testing.T) {
	resetGate(t)
	SetGate(noopGate)
	app := fiber.New()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("Public:true + RequirePermission must panic")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, "Public") {
			t.Errorf("panic message should mention 'Public', got %q", msg)
		}
	}()
	Mount(nil, app, fiber.MethodGet, "/x",
		func(c fiber.Ctx) error { return c.SendStatus(200) },
		RouteSpec{}, Doc{Public: true},
		RequirePermission("users:read"))
}

func TestMount_NoGateRegisteredPanics(t *testing.T) {
	resetGate(t)
	SetGate(nil)
	app := fiber.New()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("RequirePermission without a registered Gate must panic")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, "Gate") {
			t.Errorf("panic message should mention 'Gate', got %q", msg)
		}
	}()
	Mount(nil, app, fiber.MethodGet, "/x",
		func(c fiber.Ctx) error { return c.SendStatus(200) },
		RouteSpec{}, Doc{},
		RequirePermission("users:read"))
}

func TestMount_RequirePermission_PatchesSpec(t *testing.T) {
	resetGate(t)
	SetGate(noopGate)
	app := fiber.New()
	reg := NewRegistry()
	Mount(reg, app, fiber.MethodGet, "/x",
		func(c fiber.Ctx) error { return c.SendStatus(200) },
		RouteSpec{SuccessStatus: 200}, Doc{},
		RequirePermission("users:read"))

	ops := reg.Operations()
	if len(ops) != 1 {
		t.Fatalf("got %d ops, want 1", len(ops))
	}
	if got := ops[0].Spec.RequiredPermission; got != "users:read" {
		t.Errorf("Spec.RequiredPermission = %q, want %q", got, "users:read")
	}
}

func TestMount_RequirePermission_WrapsHandler(t *testing.T) {
	resetGate(t)
	SetGate(blockingGate)
	app := fiber.New()
	Mount(nil, app, fiber.MethodGet, "/x",
		func(c fiber.Ctx) error { return c.SendStatus(200) },
		RouteSpec{}, Doc{},
		RequirePermission("users:read"))

	req := httptest.NewRequest(fiber.MethodGet, "/x", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != 999 {
		t.Errorf("gate did not wrap handler — got status %d, want 999", resp.StatusCode)
	}
}

func TestMount_NoOptions_HandlerRunsAsUsual(t *testing.T) {
	resetGate(t)
	SetGate(blockingGate) // gate would block IF wired, but no option = no wrap
	app := fiber.New()
	Mount(nil, app, fiber.MethodGet, "/x",
		func(c fiber.Ctx) error { return c.SendStatus(204) },
		RouteSpec{}, Doc{})

	req := httptest.NewRequest(fiber.MethodGet, "/x", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != 204 {
		t.Errorf("Mount without options must pass handler through unchanged — got %d, want 204", resp.StatusCode)
	}
}

func TestMountRaw_RequirePermission_PatchesSpec(t *testing.T) {
	resetGate(t)
	SetGate(noopGate)
	app := fiber.New()
	reg := NewRegistry()
	MountRaw(reg, app, fiber.MethodGet, "/x",
		func(c fiber.Ctx) error { return c.SendStatus(200) },
		RawSpec{Summary: "demo"},
		RequirePermission("users:read"))

	ops := reg.Operations()
	if len(ops) != 1 {
		t.Fatalf("got %d ops, want 1", len(ops))
	}
	if ops[0].Raw == nil {
		t.Fatal("Raw spec should be set on MountRaw operation")
	}
	if got := ops[0].Raw.RequiredPermission; got != "users:read" {
		t.Errorf("Raw.RequiredPermission = %q, want %q", got, "users:read")
	}
}

func TestMountRaw_PublicPlusRequirePermissionPanics(t *testing.T) {
	resetGate(t)
	SetGate(noopGate)
	app := fiber.New()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("MountRaw with Public:true + RequirePermission must panic")
		}
	}()
	MountRaw(nil, app, fiber.MethodGet, "/x",
		func(c fiber.Ctx) error { return c.SendStatus(200) },
		RawSpec{Public: true},
		RequirePermission("users:read"))
}

func TestSetGate_RoundTrips(t *testing.T) {
	resetGate(t)
	SetGate(noopGate)
	if g := resolveGate(); g == nil {
		t.Error("SetGate did not store the gate")
	}
	SetGate(nil)
	if g := resolveGate(); g != nil {
		t.Error("SetGate(nil) should clear")
	}
}
