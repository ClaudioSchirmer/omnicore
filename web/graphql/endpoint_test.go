package graphql

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/application/translation"
	"github.com/gofiber/fiber/v3"
)

func newGraphQLApp(h *fakeReadHandler) *fiber.App {
	pipe := pipeline.New(translation.Default())
	reg := New(pipe).Register(
		Query[execRequest, *execQuery, execResponse]("users", "User", h),
	)
	app := fiber.New()
	app.Post("/graphql", reg.Handler())
	return app
}

func postGraphQL(t *testing.T, app *fiber.App, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest("POST", "/graphql", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	var parsed map[string]any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &parsed)
	}
	return resp.StatusCode, parsed
}

func TestEndpoint_PostQueryReturnsData(t *testing.T) {
	h := &fakeReadHandler{page: queries.Page{
		Items:       []map[string]any{{"ID": "u1", "Name": "alice"}},
		ItemCursors: []string{"c1"},
		Total:       1,
	}}
	app := newGraphQLApp(h)

	status, parsed := postGraphQL(t, app,
		`{"query":"{ users { edges { node { id name } cursor } totalCount } }"}`)
	if status != fiber.StatusOK {
		t.Fatalf("expected 200 (GraphQL convention), got %d", status)
	}
	data, ok := parsed["data"].(map[string]any)
	if !ok {
		t.Fatalf("no data in response: %v", parsed)
	}
	users := data["users"].(map[string]any)
	if users["totalCount"] != float64(1) {
		t.Errorf("totalCount = %v, want 1", users["totalCount"])
	}
	node := users["edges"].([]any)[0].(map[string]any)["node"].(map[string]any)
	if node["name"] != "alice" || node["id"] != "u1" {
		t.Errorf("node = %v", node)
	}
}

func TestEndpoint_ValidationErrorIn200Envelope(t *testing.T) {
	app := newGraphQLApp(&fakeReadHandler{page: queries.Page{}})
	status, parsed := postGraphQL(t, app,
		`{"query":"{ users(where: { name: { contains: \"x\" } }) { totalCount } }"}`)
	if status != fiber.StatusOK {
		t.Fatalf("GraphQL errors still return 200, got %d", status)
	}
	if _, ok := parsed["errors"].([]any); !ok {
		t.Errorf("expected an errors array, got %v", parsed)
	}
}

func TestEndpoint_MalformedBodyReturns400(t *testing.T) {
	app := newGraphQLApp(&fakeReadHandler{})
	status, _ := postGraphQL(t, app, `not json`)
	if status != fiber.StatusBadRequest {
		t.Errorf("malformed body → 400, got %d", status)
	}
}

func TestEndpoint_MissingQueryReturns400(t *testing.T) {
	app := newGraphQLApp(&fakeReadHandler{})
	status, _ := postGraphQL(t, app, `{"variables":{}}`)
	if status != fiber.StatusBadRequest {
		t.Errorf("missing query → 400, got %d", status)
	}
}
