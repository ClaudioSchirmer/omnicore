package bootstrap

import (
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/web/graphql"
	"github.com/ClaudioSchirmer/omnicore/web/openapi"
)

func TestBuildApp_GraphQLMounted_NotInSwagger(t *testing.T) {
	d := silentDepsWithRegistry()
	d.Config.GraphQL.Path = "/graphql"
	reg := graphql.New(d.Pipeline) // empty registry — stub Query, valid schema

	app, err := buildApp(context.Background(), d, Wiring{
		Features: []Feature{&writeOnlyFeature{}},
		OpenAPI:  &openapi.Config{Title: "T", Version: "1.0.0"},
		GraphQL:  reg,
	})
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}

	// The GraphQL endpoint is mounted and answers (200 — GraphQL convention),
	// proving it is its own surface served at the configured path.
	req := httptest.NewRequest("POST", "/graphql", strings.NewReader(`{"query":"{ __typename }"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("POST /graphql: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("POST /graphql = %d, want 200 (endpoint mounted)", resp.StatusCode)
	}

	// It must NOT appear in the OpenAPI/Swagger document.
	specResp, err := app.Test(httptest.NewRequest("GET", "/openapi.json", nil))
	if err != nil {
		t.Fatalf("GET /openapi.json: %v", err)
	}
	raw, _ := io.ReadAll(specResp.Body)
	if strings.Contains(string(raw), "/graphql") {
		t.Errorf("the GraphQL route must not appear in the OpenAPI spec, but it did:\n%s", raw)
	}
}

func TestBuildApp_GraphQLPlaygroundAndIntrospection(t *testing.T) {
	d := silentDepsWithRegistry()
	d.Config.GraphQL.Path = "/graphql"
	d.Config.GraphQL.UIPath = "/graphql/ui"
	d.Config.GraphQL.Playground = true
	d.Config.GraphQL.Introspection = true
	reg := graphql.New(d.Pipeline)

	app, err := buildApp(context.Background(), d, Wiring{
		Features: []Feature{&writeOnlyFeature{}},
		OpenAPI:  &openapi.Config{Title: "T", Version: "1.0.0"},
		GraphQL:  reg,
	})
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}

	// Playground page served at UIPath.
	uiResp, err := app.Test(httptest.NewRequest("GET", "/graphql/ui", nil))
	if err != nil {
		t.Fatalf("GET /graphql/ui: %v", err)
	}
	if uiResp.StatusCode != 200 {
		t.Fatalf("playground = %d, want 200", uiResp.StatusCode)
	}

	// Introspection answered (bootstrap enabled it from config).
	req := httptest.NewRequest("POST", "/graphql",
		strings.NewReader(`{"query":"{ __schema { queryType { name } } }"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("POST /graphql: %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(raw), "__schema") || strings.Contains(string(raw), "no resolver") {
		t.Errorf("introspection should be answered when enabled, got:\n%s", raw)
	}
}

func TestBuildApp_BothRootRedirectsPanic(t *testing.T) {
	d := silentDepsWithRegistry()
	d.Config.GraphQL.Path = "/graphql"
	d.Config.OpenAPI.RootRedirect = true
	d.Config.GraphQL.RootRedirect = true
	reg := graphql.New(d.Pipeline)

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected a boot panic when both openapi.rootRedirect and graphql.rootRedirect are enabled")
		}
		if msg, _ := r.(string); !strings.Contains(msg, "rootRedirect") || !strings.Contains(msg, "GET /") {
			t.Errorf("panic message should name the rootRedirect conflict on GET /, got: %v", r)
		}
	}()

	_, _ = buildApp(context.Background(), d, Wiring{
		Features: []Feature{&writeOnlyFeature{}},
		OpenAPI:  &openapi.Config{Title: "T", Version: "1.0.0"},
		GraphQL:  reg,
	})
}

func TestBuildApp_GraphQLDisabled_NoEndpoint(t *testing.T) {
	app, err := buildApp(context.Background(), silentDeps(), Wiring{Features: []Feature{&writeOnlyFeature{}}})
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}
	req := httptest.NewRequest("POST", "/graphql", strings.NewReader(`{"query":"{ __typename }"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != 404 && resp.StatusCode != 405 {
		t.Errorf("GraphQL disabled: POST /graphql = %d, want 404/405", resp.StatusCode)
	}
}
