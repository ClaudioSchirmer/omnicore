package graphql

import (
	"fmt"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/application/results"
	"github.com/ClaudioSchirmer/omnicore/application/translation"
	"github.com/vektah/gqlparser/v2/ast"
)

// ── RequirePermission: malformed declarations panic at wiring time ───────────

func covMustPanic(t *testing.T, name string, fn func()) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Fatal("expected a panic")
			}
		}()
		fn()
	})
}

func TestRequirePermission_MalformedDeclarationsPanic(t *testing.T) {
	covMustPanic(t, "empty", func() { RequirePermission("") })
	covMustPanic(t, "no colon", func() { RequirePermission("usersread") })
	covMustPanic(t, "wildcard", func() { RequirePermission("users:*") })
	covMustPanic(t, "duplicate on one field", func() {
		f := Field{}
		RequirePermission("users:read")(&f)
		RequirePermission("users:write")(&f)
	})
}

func TestRequirePermission_ValidDeclarationSticks(t *testing.T) {
	f := Field{}
	RequirePermission("users:read")(&f)
	if f.requiredPermission != "users:read" {
		t.Errorf("requiredPermission = %q, want users:read", f.requiredPermission)
	}
}

// ── SDL: empty registry stub + build-error surfacing + caching ───────────────

func TestSDL_EmptyRegistryEmitsQueryStub(t *testing.T) {
	reg := New(pipeline.New(translation.Default()))
	sdl, err := reg.SDL()
	if err != nil {
		t.Fatalf("empty registry must still build, got %v", err)
	}
	if !strings.Contains(sdl, "_empty: Boolean") {
		t.Errorf("query stub missing:\n%s", sdl)
	}
	if strings.Contains(sdl, "type Mutation") {
		t.Errorf("no mutations registered → no Mutation root:\n%s", sdl)
	}
	// Second call hits the cached schema (build short-circuit).
	again, err := reg.SDL()
	if err != nil || again != sdl {
		t.Errorf("cached SDL must be identical, err=%v", err)
	}
}

// covBrokenField emits syntactically invalid SDL so gqlparser fails the load.
func covBrokenField() Field {
	return Field{
		name:    "broken",
		sdlLine: func(*sdlBuilder) string { return "  broken(: !!" },
		makeResolve: func(*pipeline.Pipeline) resolver {
			return func(*configuration.AppContext, map[string]any, ast.SelectionSet, ast.FragmentDefinitionList) (any, []GraphQLError) {
				return nil, nil
			}
		},
	}
}

func TestSDL_BuildErrorSurfacesAndIsCached(t *testing.T) {
	reg := New(pipeline.New(translation.Default())).Register(covBrokenField())
	if _, err := reg.SDL(); err == nil {
		t.Fatal("invalid SDL must surface the load error")
	}
	// The build error is cached: a second call returns it without rebuilding.
	_, err := reg.SDL()
	if err == nil {
		t.Fatal("cached build error must persist")
	}
	// Execute reports the same fault instead of running.
	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG)
	resp := reg.Execute(ctx, `{ broken }`, nil, "")
	if len(resp.Errors) == 0 || !strings.Contains(resp.Errors[0].Message, "schema build failed") {
		t.Errorf("Execute on a broken schema must report the build failure, got %+v", resp.Errors)
	}
}

// ── name gates: grammar at Register time, collisions at build time ──────────

func TestMustValidName_ConstructorGates(t *testing.T) {
	h := &fakeReadHandler{}
	covMustPanic(t, "field name with space", func() {
		QueryWithParams[execRequest]("us ers", "User", execResponse{}.FromResult, h)
	})
	covMustPanic(t, "empty field name", func() {
		QueryWithParams[execRequest]("", "User", execResponse{}.FromResult, h)
	})
	covMustPanic(t, "empty entity", func() {
		QueryWithParams[execRequest]("users", "", execResponse{}.FromResult, h)
	})
	covMustPanic(t, "leading digit", func() {
		QueryWithParams[execRequest]("1users", "User", execResponse{}.FromResult, h)
	})
	covMustPanic(t, "by-id bad entity", func() {
		QueryByID[bareRequest]("user", "Us-er", byIDResponse{}.FromResult, &fakeByIDHandler{})
	})
	covMustPanic(t, "mutation bad name", func() {
		MutationByID[delCmd, *delCmd, results.None]("delete thing", &fakeDelHandler{})
	})
}

// TestBuild_EntityInfraCollisionPanics — an entity name landing on a derived/
// infrastructure type (or the reverse) is a silent-garbage schema without the
// guard; with it, the boot-time build panics with the offending name.
func TestBuild_EntityInfraCollisionPanics(t *testing.T) {
	covMustPanic(t, "entity claims PageInfo", func() {
		reg := New(pipeline.New(translation.Default())).Register(
			QueryWithParams[execRequest]("users", "PageInfo", execResponse{}.FromResult, &fakeReadHandler{}),
		)
		_, _ = reg.SDL()
	})
	covMustPanic(t, "entity claims a sibling's Connection", func() {
		reg := New(pipeline.New(translation.Default())).Register(
			QueryWithParams[execRequest]("users", "User", execResponse{}.FromResult, &fakeReadHandler{}),
		).Register(
			QueryByID[bareRequest]("gadget", "UserConnection", byIDResponse{}.FromResult, &fakeByIDHandler{}),
		)
		_, _ = reg.SDL()
	})
	// The legitimate shared-entity mapping (users/user both on "User") stays.
	reg := New(pipeline.New(translation.Default())).Register(
		QueryWithParams[execRequest]("users", "User", execResponse{}.FromResult, &fakeReadHandler{}),
	).Register(
		QueryByID[bareRequest]("user", "User", byIDResponse{}.FromResult, &fakeByIDHandler{}),
	)
	if _, err := reg.SDL(); err != nil {
		t.Fatalf("shared entity name must keep building: %v", err)
	}
}

// ── shared-entity wire-alignment boot guard ──────────────────────────────────

// misalignedResponse shares the "User" entity with execResponse but drops the
// `age` wire field and adds `email` — the shape the boot guard must reject.
type misalignedResponse struct {
	ID    *string `json:"id,omitempty"`
	Name  *string `json:"name,omitempty"`
	Email *string `json:"email,omitempty"`
}

func (misalignedResponse) FromResult(r execResult) misalignedResponse {
	return misalignedResponse{ID: r.ID, Name: r.Name}
}

// TestBuild_SharedEntityAlignedPairBuilds — two DISTINCT Response DTO Go types
// with the SAME wire field set may share one entity name: the first defines the
// node object, the second maps onto it, and the schema builds (the previous
// honor-system behavior, now explicitly guarded).
func TestBuild_SharedEntityAlignedPairBuilds(t *testing.T) {
	reg := New(pipeline.New(translation.Default())).Register(
		QueryWithParams[execRequest]("users", "User", execResponse{}.FromResult, &fakeReadHandler{}),
	).Register(
		QueryByID[bareRequest]("user", "User", byIDResponse{}.FromResult, &fakeByIDHandler{}),
	)
	sdl, err := reg.SDL()
	if err != nil {
		t.Fatalf("wire-aligned Response DTOs sharing an entity must build: %v", err)
	}
	if strings.Count(sdl, "type User {") != 1 {
		t.Errorf("expected exactly one User node type:\n%s", sdl)
	}
}

// TestBuild_SharedEntityMisalignedPairPanics — the NEW boot guard: two Response
// DTOs sharing an entity name with DIFFERENT wire field sets panic at schema
// build, and the message names the differing fields (both directions) and the
// offending DTO type.
func TestBuild_SharedEntityMisalignedPairPanics(t *testing.T) {
	reg := New(pipeline.New(translation.Default())).Register(
		QueryWithParams[execRequest]("users", "User", execResponse{}.FromResult, &fakeReadHandler{}),
	).Register(
		QueryByID[bareRequest]("user", "User", misalignedResponse{}.FromResult, &fakeByIDHandler{}),
	)
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("wire-misaligned Response DTOs sharing an entity must panic at build")
		}
		msg := fmt.Sprint(r)
		for _, want := range []string{`"User"`, `"email"`, `"age"`, "misalignedResponse"} {
			if !strings.Contains(msg, want) {
				t.Errorf("panic message must name %s; got:\n%s", want, msg)
			}
		}
	}()
	_, _ = reg.SDL()
}
