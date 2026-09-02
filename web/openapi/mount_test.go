package openapi

import (
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/gofiber/fiber/v3"
)

func TestMount_NilRegistry_StillRegistersOnFiber(t *testing.T) {
	app := fiber.New()
	called := false
	Mount(nil, app, fiber.MethodGet, "/ping", func(c fiber.Ctx) error {
		called = true
		return c.SendStatus(fiber.StatusNoContent)
	}, RouteSpec{}, Doc{})

	req := httptest.NewRequest(fiber.MethodGet, "/ping", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != fiber.StatusNoContent {
		t.Fatalf("status: got %d, want 204", resp.StatusCode)
	}
	if !called {
		t.Fatal("handler should have been invoked despite nil registry")
	}
}

func TestMount_RegisterOnAppUsesPathVerbatim(t *testing.T) {
	app := fiber.New()
	reg := NewRegistry()
	Mount(reg, app, fiber.MethodGet, "/health",
		func(c fiber.Ctx) error { return c.SendStatus(200) },
		RouteSpec{SuccessStatus: 200},
		Doc{Summary: "Liveness probe"})

	ops := reg.Operations()
	if len(ops) != 1 {
		t.Fatalf("got %d ops, want 1", len(ops))
	}
	if ops[0].Method != "GET" || ops[0].Path != "/health" {
		t.Fatalf("got %s %s, want GET /health", ops[0].Method, ops[0].Path)
	}
	if ops[0].Doc.Summary != "Liveness probe" {
		t.Fatalf("doc not propagated: got %+v", ops[0].Doc)
	}
}

func TestMount_RegisterOnGroupPrependsPrefix(t *testing.T) {
	app := fiber.New()
	reg := NewRegistry()
	users := app.Group("/users")
	Mount(reg, users, fiber.MethodPost, "/",
		func(c fiber.Ctx) error { return c.SendStatus(201) },
		RouteSpec{SuccessStatus: 201},
		Doc{Summary: "Create a user"})
	Mount(reg, users, fiber.MethodPatch, "/:id/archive",
		func(c fiber.Ctx) error { return c.SendStatus(200) },
		RouteSpec{SuccessStatus: 200, HasPathID: true},
		Doc{Summary: "Archive a user"})

	ops := reg.Operations()
	if len(ops) != 2 {
		t.Fatalf("got %d ops, want 2", len(ops))
	}
	if ops[0].Path != "/users/" {
		t.Fatalf("op[0] path: got %q, want /users/", ops[0].Path)
	}
	if ops[1].Path != "/users/:id/archive" {
		t.Fatalf("op[1] path: got %q, want /users/:id/archive", ops[1].Path)
	}
}

func TestMount_PropagatesSpec(t *testing.T) {
	app := fiber.New()
	reg := NewRegistry()
	type reqT struct {
		Name string `json:"name"`
	}
	type respT struct {
		ID string `json:"id"`
	}
	spec := RouteSpec{
		RequestType:   reflect.TypeOf(reqT{}),
		ResponseType:  reflect.TypeOf(respT{}),
		SuccessStatus: 201,
		Strict:        true,
		HasPathID:     false,
	}
	Mount(reg, app, fiber.MethodPost, "/things",
		func(c fiber.Ctx) error { return c.SendStatus(201) },
		spec, Doc{Tags: []string{"Things"}})

	op := reg.Operations()[0]
	if op.Spec.RequestType == nil || op.Spec.RequestType.Name() != "reqT" {
		t.Fatalf("RequestType not propagated: got %+v", op.Spec.RequestType)
	}
	if op.Spec.ResponseType == nil || op.Spec.ResponseType.Name() != "respT" {
		t.Fatalf("ResponseType not propagated: got %+v", op.Spec.ResponseType)
	}
	if !op.Spec.Strict {
		t.Fatal("Strict not propagated")
	}
	if op.Spec.SuccessStatus != 201 {
		t.Fatalf("SuccessStatus: got %d, want 201", op.Spec.SuccessStatus)
	}
}

func TestJoinPath_AppReturnsPathVerbatim(t *testing.T) {
	app := fiber.New()
	if got := JoinPath(app, "/x"); got != "/x" {
		t.Fatalf("got %q, want /x", got)
	}
}

func TestJoinPath_GroupConcatenates(t *testing.T) {
	app := fiber.New()
	g := app.Group("/api")
	if got := JoinPath(g, "/users"); got != "/api/users" {
		t.Fatalf("got %q, want /api/users", got)
	}
}

func TestJoinPath_GroupWithTrailingSlash(t *testing.T) {
	app := fiber.New()
	g := app.Group("/api/")
	if got := JoinPath(g, "/users"); got != "/api/users" {
		t.Fatalf("got %q, want /api/users", got)
	}
}

// TestStandardErrors_ByIDRouteDeclaresBothRefusals pins the document against
// what a by-id route now answers for a malformed `:id`: 404 on a read, 400 on
// a write. The bodyless commands (archive / unarchive / delete) reach that 400
// with no body at all, so the body-shaped rule alone would have left it
// undeclared — a status the service emits and the contract does not name.
func TestStandardErrors_ByIDRouteDeclaresBothRefusals(t *testing.T) {
	s := &Spec{}
	got := s.standardErrors(Operation{Spec: RouteSpec{SuccessStatus: 200, HasPathID: true}})

	for _, want := range []int{400, 404} {
		if _, ok := got[want]; !ok {
			t.Errorf("status %d must be declared on a by-id route, got %v", want, got)
		}
	}
}

// A route with no path id and no body declares neither: nothing on it can
// produce those refusals.
func TestStandardErrors_BodylessRootRouteDeclaresNoPathRefusals(t *testing.T) {
	s := &Spec{}
	got := s.standardErrors(Operation{Spec: RouteSpec{SuccessStatus: 200}})

	for _, absent := range []int{400, 404} {
		if _, ok := got[absent]; ok {
			t.Errorf("status %d must not be declared here, got %v", absent, got)
		}
	}
}
