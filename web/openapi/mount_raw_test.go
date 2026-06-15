package openapi

import (
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/gofiber/fiber/v3"
)

type whoamiResponseFixture struct {
	Subject       string `json:"subject"`
	Authenticated bool   `json:"authenticated"`
}

func TestMountRaw_NilRegistry_StillRegistersOnFiber(t *testing.T) {
	app := fiber.New()
	called := false
	MountRaw(nil, app, fiber.MethodGet, "/whoami",
		func(c fiber.Ctx) error {
			called = true
			return c.SendStatus(fiber.StatusOK)
		},
		RawSpec{Summary: "Whoami"})

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/whoami", nil))
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	if !called {
		t.Fatal("handler should have been invoked despite nil registry")
	}
}

func TestMountRaw_RegisterStoresRawNotSpec(t *testing.T) {
	app := fiber.New()
	reg := NewRegistry()
	MountRaw(reg, app, fiber.MethodGet, "/whoami",
		func(c fiber.Ctx) error { return c.SendStatus(200) },
		RawSpec{
			Summary: "Returns the authenticated identity",
			Tags:    []string{"Auth"},
			Responses: map[int]ResponseSpec{
				200: {Type: reflect.TypeOf(whoamiResponseFixture{})},
			},
		})

	ops := reg.Operations()
	if len(ops) != 1 {
		t.Fatalf("got %d ops, want 1", len(ops))
	}
	op := ops[0]
	if op.Raw == nil {
		t.Fatal("MountRaw must populate Operation.Raw")
	}
	// Spec must stay at zero so the spec assembler can branch on
	// (Raw != nil) deterministically.
	zero := RouteSpec{}
	if op.Spec != zero {
		t.Fatalf("Spec must stay at zero on a Raw operation; got %+v", op.Spec)
	}
	if op.Raw.Summary != "Returns the authenticated identity" {
		t.Fatalf("Summary not propagated: got %q", op.Raw.Summary)
	}
	if len(op.Raw.Tags) != 1 || op.Raw.Tags[0] != "Auth" {
		t.Fatalf("Tags not propagated: got %v", op.Raw.Tags)
	}
	resp200, ok := op.Raw.Responses[200]
	if !ok {
		t.Fatal("Responses[200] missing")
	}
	if resp200.Type == nil || resp200.Type.Name() != "whoamiResponseFixture" {
		t.Fatalf("Responses[200].Type: got %+v, want whoamiResponseFixture", resp200.Type)
	}
}

func TestMountRaw_PathParameterAlwaysRequired(t *testing.T) {
	app := fiber.New()
	reg := NewRegistry()
	MountRaw(reg, app, fiber.MethodGet, "/showcase/keycloak/admin/:id",
		func(c fiber.Ctx) error { return c.SendStatus(200) },
		RawSpec{
			Summary: "Fetch a Keycloak user",
			Parameters: []Parameter{
				// Required omitted on purpose — MountRaw must force it.
				{In: InPath, Name: "id", Description: "Keycloak user id"},
				QueryParam("verbose", "Echo upstream payload", reflect.TypeOf(true)),
			},
		})
	op := reg.Operations()[0]
	if op.Raw == nil || len(op.Raw.Parameters) != 2 {
		t.Fatalf("expected 2 parameters, got %+v", op.Raw)
	}
	if !op.Raw.Parameters[0].Required {
		t.Fatal("path parameter must be normalized to Required=true")
	}
	if op.Raw.Parameters[1].Required {
		t.Fatal("query parameter must keep Required=false when not explicitly set")
	}
}

func TestMountRaw_GroupPrefixApplied(t *testing.T) {
	app := fiber.New()
	reg := NewRegistry()
	showcase := app.Group("/showcase")
	MountRaw(reg, showcase, fiber.MethodGet, "/keycloak/realm",
		func(c fiber.Ctx) error { return c.SendStatus(200) },
		RawSpec{Summary: "Keycloak realm info"})

	op := reg.Operations()[0]
	if op.Path != "/showcase/keycloak/realm" {
		t.Fatalf("group prefix not applied: got %q", op.Path)
	}
}

func TestMountRaw_HiddenAndPublicFlagsPropagate(t *testing.T) {
	app := fiber.New()
	reg := NewRegistry()
	MountRaw(reg, app, fiber.MethodGet, "/echo/sse",
		func(c fiber.Ctx) error { return c.SendStatus(200) },
		RawSpec{Hidden: true, Public: true, Deprecated: true})
	op := reg.Operations()[0]
	if op.Raw == nil {
		t.Fatal("Raw missing")
	}
	if !op.Raw.Hidden || !op.Raw.Public || !op.Raw.Deprecated {
		t.Fatalf("Hidden/Public/Deprecated must propagate, got %+v", op.Raw)
	}
}

func TestMountRaw_RequestBodyPropagates(t *testing.T) {
	app := fiber.New()
	reg := NewRegistry()
	type echoPayload struct {
		Body string `json:"body"`
	}
	MountRaw(reg, app, fiber.MethodPost, "/echo/signed",
		func(c fiber.Ctx) error { return c.SendStatus(200) },
		RawSpec{
			Summary: "HMAC-signed echo",
			RequestBody: &RequestBody{
				Description: "Arbitrary JSON to be echoed back with signing headers",
				Required:    true,
				Type:        reflect.TypeOf(echoPayload{}),
			},
		})
	op := reg.Operations()[0]
	if op.Raw == nil || op.Raw.RequestBody == nil {
		t.Fatalf("RequestBody missing, got %+v", op.Raw)
	}
	if op.Raw.RequestBody.Type == nil || op.Raw.RequestBody.Type.Name() != "echoPayload" {
		t.Fatalf("RequestBody.Type: got %+v", op.Raw.RequestBody.Type)
	}
	if !op.Raw.RequestBody.Required {
		t.Fatal("RequestBody.Required must propagate")
	}
}

func TestMountRaw_AndMountCoexistInSameRegistry(t *testing.T) {
	app := fiber.New()
	reg := NewRegistry()
	users := app.Group("/users")

	// Canonical Mount (RouteSpec branch).
	Mount(reg, users, fiber.MethodPost, "/",
		func(c fiber.Ctx) error { return c.SendStatus(201) },
		RouteSpec{SuccessStatus: 201}, Doc{Summary: "Create"})

	// MountRaw (RawSpec branch).
	MountRaw(reg, app, fiber.MethodGet, "/whoami",
		func(c fiber.Ctx) error { return c.SendStatus(200) },
		RawSpec{Summary: "Whoami"})

	ops := reg.Operations()
	if len(ops) != 2 {
		t.Fatalf("got %d ops, want 2", len(ops))
	}
	if ops[0].Raw != nil {
		t.Fatal("canonical Mount op[0] must not populate Raw")
	}
	if ops[1].Raw == nil {
		t.Fatal("raw MountRaw op[1] must populate Raw")
	}
	if ops[0].Doc.Summary != "Create" {
		t.Fatalf("canonical Doc not propagated: %+v", ops[0].Doc)
	}
	if ops[1].Doc.Summary != "" || len(ops[1].Doc.Tags) != 0 {
		t.Fatalf("Doc must stay zero on a Raw operation; got %+v", ops[1].Doc)
	}
}
