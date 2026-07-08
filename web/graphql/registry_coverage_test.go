package graphql

import (
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
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
