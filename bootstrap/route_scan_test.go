package bootstrap

import (
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/web/openapi"
	"github.com/gofiber/fiber/v3"
)

// resetGate clears any registered openapi.Gate. The scans don't invoke it,
// but RequirePermission options used by some tests need it set.
func resetGate(t *testing.T) {
	t.Helper()
	prev := getRegisteredGate()
	t.Cleanup(func() { openapi.SetGate(prev) })
}

// getRegisteredGate returns whatever Gate openapi has registered today.
// Used to roundtrip in resetGate; openapi exposes no public getter, so we
// take the round-trip via SetGate(nil) + the test's own captured nil.
func getRegisteredGate() openapi.Gate {
	// Best-effort: there is no public read API, but tests below either
	// register their own gate (which they restore) or rely on nil.
	return nil
}

// --- scanAuthorization ------------------------------------------------------

func TestScanAuthorization_NilRegistry_NoOp(t *testing.T) {
	// Must not panic with nil registry.
	scanAuthorization(nil, nil)
}

func TestScanAuthorization_EveryRouteGated_NoPanic(t *testing.T) {
	resetGate(t)
	openapi.SetGate(func(h fiber.Handler, _ string) fiber.Handler { return h })
	reg := openapi.NewRegistry()
	app := fiber.New()

	openapi.Mount(reg, app, fiber.MethodPost, "/a",
		func(c fiber.Ctx) error { return nil },
		openapi.RouteSpec{SuccessStatus: 201}, openapi.Doc{Summary: "a"},
		openapi.RequirePermission("users:write"))

	scanAuthorization(reg, nil) // must not panic
}

func TestScanAuthorization_UngatedNonPublicRoute_Panics(t *testing.T) {
	reg := openapi.NewRegistry()
	app := fiber.New()
	openapi.Mount(reg, app, fiber.MethodGet, "/u",
		func(c fiber.Ctx) error { return nil },
		openapi.RouteSpec{SuccessStatus: 200}, openapi.Doc{Summary: "u"})

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic for ungated non-public route")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, "GET /u") {
			t.Errorf("panic message must list offender; got %q", msg)
		}
	}()
	scanAuthorization(reg, nil)
}

func TestScanAuthorization_PublicViaDoc_NoPanic(t *testing.T) {
	reg := openapi.NewRegistry()
	app := fiber.New()
	openapi.Mount(reg, app, fiber.MethodGet, "/p",
		func(c fiber.Ctx) error { return nil },
		openapi.RouteSpec{SuccessStatus: 200},
		openapi.Doc{Summary: "p", Public: true})

	scanAuthorization(reg, nil) // public via Doc.Public — bypasses requirement
}

func TestScanAuthorization_PublicViaAllowlist_NoPanic(t *testing.T) {
	reg := openapi.NewRegistry()
	app := fiber.New()
	openapi.Mount(reg, app, fiber.MethodGet, "/p",
		func(c fiber.Ctx) error { return nil },
		openapi.RouteSpec{SuccessStatus: 200}, openapi.Doc{Summary: "p"})

	scanAuthorization(reg, []string{"GET /p"}) // public via the publicRoutes list
}

func TestScanAuthorization_PublicRawViaSpec_NoPanic(t *testing.T) {
	reg := openapi.NewRegistry()
	app := fiber.New()
	openapi.MountRaw(reg, app, fiber.MethodGet, "/r",
		func(c fiber.Ctx) error { return nil },
		openapi.RawSpec{Summary: "r", Public: true})

	scanAuthorization(reg, nil)
}

func TestScanAuthorization_MixedRoutes_PanicListsAll(t *testing.T) {
	resetGate(t)
	openapi.SetGate(func(h fiber.Handler, _ string) fiber.Handler { return h })
	reg := openapi.NewRegistry()
	app := fiber.New()

	openapi.Mount(reg, app, fiber.MethodGet, "/a",
		func(c fiber.Ctx) error { return nil },
		openapi.RouteSpec{SuccessStatus: 200}, openapi.Doc{Summary: "a"})
	openapi.Mount(reg, app, fiber.MethodPost, "/b",
		func(c fiber.Ctx) error { return nil },
		openapi.RouteSpec{SuccessStatus: 201}, openapi.Doc{Summary: "b"})
	openapi.Mount(reg, app, fiber.MethodPut, "/c",
		func(c fiber.Ctx) error { return nil },
		openapi.RouteSpec{SuccessStatus: 200}, openapi.Doc{Summary: "c"},
		openapi.RequirePermission("c:write"))
	openapi.Mount(reg, app, fiber.MethodGet, "/d",
		func(c fiber.Ctx) error { return nil },
		openapi.RouteSpec{SuccessStatus: 200}, openapi.Doc{Summary: "d", Public: true})

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, "GET /a") || !strings.Contains(msg, "POST /b") {
			t.Errorf("panic must list both offenders; got %q", msg)
		}
		if strings.Contains(msg, "PUT /c") || strings.Contains(msg, "GET /d") {
			t.Errorf("panic must NOT list gated or public; got %q", msg)
		}
	}()
	scanAuthorization(reg, nil)
}

// --- scanRouteRegistration --------------------------------------------------

func TestScanRouteRegistration_NilRegistry_NoOp(t *testing.T) {
	app := fiber.New()
	scanRouteRegistration(app, nil)
}

func TestScanRouteRegistration_EmptyApp_NoPanic(t *testing.T) {
	app := fiber.New()
	reg := openapi.NewRegistry()
	scanRouteRegistration(app, reg)
}

func TestScanRouteRegistration_AllRoutesViaMount_NoPanic(t *testing.T) {
	app := fiber.New()
	reg := openapi.NewRegistry()
	openapi.MountRaw(reg, app, fiber.MethodGet, "/x",
		func(c fiber.Ctx) error { return nil },
		openapi.RawSpec{Summary: "x", Public: true})

	scanRouteRegistration(app, reg)
}

func TestScanRouteRegistration_RouteOutsideMount_Panics(t *testing.T) {
	app := fiber.New()
	reg := openapi.NewRegistry()
	// Off-canon: direct app.Get bypassing the registry.
	app.Get("/off-canon", func(c fiber.Ctx) error { return nil })

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic for off-canon route")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, "GET /off-canon") {
			t.Errorf("panic must list /off-canon; got %q", msg)
		}
	}()
	scanRouteRegistration(app, reg)
}

func TestScanRouteRegistration_MixedRoutes_PanicListsOffenders(t *testing.T) {
	app := fiber.New()
	reg := openapi.NewRegistry()
	openapi.MountRaw(reg, app, fiber.MethodGet, "/ok",
		func(c fiber.Ctx) error { return nil },
		openapi.RawSpec{Summary: "ok", Public: true})
	app.Get("/bad1", func(c fiber.Ctx) error { return nil })
	app.Post("/bad2", func(c fiber.Ctx) error { return nil })

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, "GET /bad1") {
			t.Errorf("missing /bad1: %q", msg)
		}
		if !strings.Contains(msg, "POST /bad2") {
			t.Errorf("missing /bad2: %q", msg)
		}
		if strings.Contains(msg, "GET /ok") {
			t.Errorf("/ok should not be flagged: %q", msg)
		}
	}()
	scanRouteRegistration(app, reg)
}
