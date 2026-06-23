package bootstrap

import (
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

	app, err := buildApp(d, Wiring{
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

func TestBuildApp_GraphQLDisabled_NoEndpoint(t *testing.T) {
	app, err := buildApp(silentDeps(), Wiring{Features: []Feature{&writeOnlyFeature{}}})
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
