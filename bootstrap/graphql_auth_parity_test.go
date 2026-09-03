package bootstrap

import (
	"context"
	"io"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/web/openapi"
	"github.com/gofiber/fiber/v3"
)

// The two documentation surfaces must be reachable on the SAME terms under
// auth.mode=jwt. Swagger UI has always been: its page and its spec are appended
// to the bypass list at boot, so an operator who turns the docs on gets docs
// that open. These tests hold GraphQL to that promise — the playground page AND
// the schema behind it — and hold the line that stops there: data queries share
// the introspection endpoint and stay behind the bearer.

func jwtParityDeps(t *testing.T) Deps {
	t.Helper()
	d := silentDepsWithRegistry()
	d.Config.Auth = jwtAuthConfig(testPublicKeyPEM(t))
	d.Config.GraphQL.Path = "/graphql"
	d.Config.GraphQL.UIPath = "/graphql/ui"
	d.Config.GraphQL.Playground = true
	d.Config.GraphQL.Introspection = true
	return d
}

func parityWiring() Wiring {
	return Wiring{
		Features: []Feature{&writeOnlyFeature{}, gqlFieldFeature("users")},
		OpenAPI:  &openapi.Config{Title: "T", Version: "1.0.0"},
	}
}

// get/post fire tokenless requests against a built app.
func get(t *testing.T, app *fiber.App, path string) (int, string) {
	t.Helper()
	resp, err := app.Test(httptest.NewRequest("GET", path, nil))
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func postGraphQL(t *testing.T, app *fiber.App, query string) (int, string) {
	t.Helper()
	req := httptest.NewRequest("POST", "/graphql", strings.NewReader(`{"query":`+strconv.Quote(query)+`}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("POST /graphql: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// TestBuildApp_DocumentationSurfaceParityUnderJWT is the regression this whole
// change exists for: with auth.mode=jwt and NOTHING in auth.publicRoutes, both
// documentation surfaces open — page and schema alike — while a data query does
// not.
func TestBuildApp_DocumentationSurfaceParityUnderJWT(t *testing.T) {
	app, err := buildApp(context.Background(), jwtParityDeps(t), parityWiring())
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}

	// The pages.
	for _, path := range []string{"/docs", "/graphql/ui"} {
		if status, body := get(t, app, path); status != 200 {
			t.Errorf("anonymous GET %s = %d, want 200 (documentation pages are public on BOTH surfaces); body=%s",
				path, status, body)
		}
	}

	// The schema documents. REST publishes its own on a route; GraphQL answers
	// introspection through the endpoint, so the grant is shaped by the
	// document — but the OUTCOME an operator sees has to be identical.
	if status, body := get(t, app, "/openapi.json"); status != 200 {
		t.Errorf("anonymous GET /openapi.json = %d, want 200; body=%s", status, body)
	}
	status, body := postGraphQL(t, app, `{ __schema { queryType { name } } }`)
	if status != 200 {
		t.Fatalf("anonymous introspection = %d, want 200; body=%s", status, body)
	}
	if strings.Contains(body, "MissingAuthorizationNotification") {
		t.Fatalf("introspection was refused by the auth middleware: %s", body)
	}
	if !strings.Contains(body, "queryType") {
		t.Errorf("introspection should answer with the schema, got: %s", body)
	}
}

// Where the parity stops. The bypass is proven by what it refuses: the endpoint
// that answers introspection also serves data, and only the first is public.
func TestBuildApp_GraphQLDataQueryStaysBehindTheBearer(t *testing.T) {
	app, err := buildApp(context.Background(), jwtParityDeps(t), parityWiring())
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}

	for _, query := range []string{
		`{ users { totalCount } }`,
		`{ __schema { queryType { name } } users { totalCount } }`,
		`query I { __typename } query R { users { totalCount } }`,
	} {
		status, body := postGraphQL(t, app, query)
		if status != 401 {
			t.Errorf("anonymous %q = %d, want 401; body=%s", query, status, body)
		}
	}
}

// With introspection off there is no schema to publish, so the endpoint is
// wholly behind the bearer — the page still opens (a documentation page is
// public on both surfaces) and simply has nothing to render.
func TestBuildApp_IntrospectionOff_EndpointFullyGuarded(t *testing.T) {
	d := jwtParityDeps(t)
	d.Config.GraphQL.Introspection = false
	app, err := buildApp(context.Background(), d, parityWiring())
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}

	if status, body := get(t, app, "/graphql/ui"); status != 200 {
		t.Errorf("GET /graphql/ui = %d, want 200; body=%s", status, body)
	}
	status, body := postGraphQL(t, app, `{ __schema { queryType { name } } }`)
	if status != 401 {
		t.Errorf("introspection with graphql.introspection=false = %d, want 401; body=%s", status, body)
	}
}

// The playground is opt-in; not turning it on must not open the endpoint's path
// by some other route, and no page is published.
func TestBuildApp_PlaygroundOff_NoPublicUIPath(t *testing.T) {
	d := jwtParityDeps(t)
	d.Config.GraphQL.Playground = false
	app, err := buildApp(context.Background(), d, parityWiring())
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}
	if status, _ := get(t, app, "/graphql/ui"); status == 200 {
		t.Error("GET /graphql/ui answered 200 with graphql.playground=false")
	}
}

// The UI path the bypass names and the UI path the framework serves come from
// ONE variable — a custom uiPath must stay public, which is the drift that put
// the playground behind auth in the first place.
func TestBuildApp_CustomGraphQLUIPathIsPublic(t *testing.T) {
	d := jwtParityDeps(t)
	d.Config.GraphQL.UIPath = "/playground"
	app, err := buildApp(context.Background(), d, parityWiring())
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}
	if status, body := get(t, app, "/playground"); status != 200 {
		t.Errorf("anonymous GET /playground = %d, want 200; body=%s", status, body)
	}
}

// graphql.introspection can be on in the yaml while no feature declares the
// surface. There is then no registry, no endpoint and no schema to publish, so
// the predicate declines and the request is answered by the auth middleware
// rather than bypassing it to reach a 404.
func TestBuildApp_IntrospectionOnWithoutGraphQLFeature_NoBypass(t *testing.T) {
	d := jwtParityDeps(t)
	app, err := buildApp(context.Background(), d, Wiring{
		Features: []Feature{&writeOnlyFeature{}}, // no GraphQLFeature
		OpenAPI:  &openapi.Config{Title: "T", Version: "1.0.0"},
	})
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}
	status, body := postGraphQL(t, app, `{ __schema { queryType { name } } }`)
	if status != 401 {
		t.Errorf("POST /graphql with no GraphQL surface = %d, want 401; body=%s", status, body)
	}
}
