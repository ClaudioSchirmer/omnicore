package openapi

import (
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/gofiber/fiber/v3"
)

type authSampleResponse struct {
	ID string `json:"id"`
}

func authRegistryWithRoutes(t *testing.T) *Registry {
	t.Helper()
	reg := NewRegistry()
	app := fiber.New()
	// Protected canonical route.
	Mount(reg, app, fiber.MethodGet, "/users",
		func(c fiber.Ctx) error { return nil },
		RouteSpec{
			ResponseType:  reflect.TypeOf(authSampleResponse{}),
			SuccessStatus: 200,
		},
		Doc{Summary: "List users", Tags: []string{"Users"}})
	// Canonical route flagged Public via Doc.
	Mount(reg, app, fiber.MethodGet, "/version",
		func(c fiber.Ctx) error { return nil },
		RouteSpec{
			ResponseType:  reflect.TypeOf(authSampleResponse{}),
			SuccessStatus: 200,
		},
		Doc{Summary: "Version", Public: true})
	// Raw route NOT public; framework allowlist will mark it so.
	MountRaw(reg, app, fiber.MethodGet, "/health",
		func(c fiber.Ctx) error { return nil },
		RawSpec{Summary: "Liveness probe"})
	// Raw route Public via RawSpec.Public.
	MountRaw(reg, app, fiber.MethodGet, "/whoami",
		func(c fiber.Ctx) error { return nil },
		RawSpec{Summary: "Whoami", Public: true})
	return reg
}

func TestSpec_AuthDisabled_NoSecuritySchemeNoOperationSecurity(t *testing.T) {
	reg := authRegistryWithRoutes(t)
	spec := NewSpec(Config{Title: "T", Version: "1"}, reg)
	out := marshalSpec(t, spec)

	components, ok := out["components"].(map[string]any)
	if ok {
		if _, exists := components["securitySchemes"]; exists {
			t.Fatal("securitySchemes must be absent when WithAuth is not applied")
		}
	}
	users := out["paths"].(map[string]any)["/users"].(map[string]any)["get"].(map[string]any)
	if _, exists := users["security"]; exists {
		t.Fatal("operation security must be absent when WithAuth is not applied")
	}
	// 401 must NOT be in the responses when auth is disabled.
	resp := users["responses"].(map[string]any)
	if _, exists := resp["401"]; exists {
		t.Fatal("401 must not auto-appear when auth is disabled")
	}
}

func TestSpec_AuthEnabled_AddsBearerAuthSchemeAndPerOperationSecurity(t *testing.T) {
	reg := authRegistryWithRoutes(t)
	spec := NewSpec(Config{Title: "T", Version: "1"}, reg)
	spec.auth = &AuthContext{PublicRoutes: []string{"GET /health"}}
	out := marshalSpec(t, spec)

	// Components carry the bearerAuth scheme.
	components := out["components"].(map[string]any)
	schemes, ok := components["securitySchemes"].(map[string]any)
	if !ok {
		t.Fatalf("securitySchemes missing; got %+v", components)
	}
	bearer := schemes["bearerAuth"].(map[string]any)
	if bearer["type"] != "http" || bearer["scheme"] != "bearer" || bearer["bearerFormat"] != "JWT" {
		t.Fatalf("bearerAuth scheme malformed; got %+v", bearer)
	}

	paths := out["paths"].(map[string]any)

	// Protected canonical → carries security + 401.
	users := paths["/users"].(map[string]any)["get"].(map[string]any)
	sec, ok := users["security"].([]any)
	if !ok || len(sec) != 1 {
		t.Fatalf("/users should have security entry; got %+v", users["security"])
	}
	if _, ok := users["responses"].(map[string]any)["401"]; !ok {
		t.Fatal("/users should auto-add 401 when auth is enabled")
	}

	// Canonical with Doc.Public → no security, no 401.
	version := paths["/version"].(map[string]any)["get"].(map[string]any)
	if _, exists := version["security"]; exists {
		t.Fatal("Doc.Public route should NOT carry security")
	}
	if _, exists := version["responses"].(map[string]any)["401"]; exists {
		t.Fatal("Doc.Public route should NOT auto-add 401")
	}

	// Raw NOT public via flag, but listed in PublicRoutes → no security.
	health := paths["/health"].(map[string]any)["get"].(map[string]any)
	if _, exists := health["security"]; exists {
		t.Fatal("/health (in PublicRoutes) should NOT carry security")
	}

	// Raw with RawSpec.Public → no security.
	whoami := paths["/whoami"].(map[string]any)["get"].(map[string]any)
	if _, exists := whoami["security"]; exists {
		t.Fatal("RawSpec.Public route should NOT carry security")
	}
}

func TestSpec_WithAuthOption_AttachesAuthToSpec(t *testing.T) {
	// Confirm the public Register path wires WithAuth correctly.
	app := fiber.New()
	reg := authRegistryWithRoutes(t)
	Register(app, Config{Title: "T", Version: "1"}, reg,
		WithAuth(AuthContext{PublicRoutes: []string{"GET /health"}}))

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/openapi.json", nil))
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
}
