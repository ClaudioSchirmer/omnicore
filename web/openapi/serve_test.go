package openapi

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
)

type serveSampleResponse struct {
	ID string `json:"id"`
}

func TestRegister_OpenAPIJsonReturnsValidSpec(t *testing.T) {
	app := fiber.New()
	reg := NewRegistry()
	Mount(reg, app, fiber.MethodGet, "/users",
		func(c *fiber.Ctx) error { return c.SendStatus(200) },
		RouteSpec{
			ResponseType:  reflect.TypeOf(serveSampleResponse{}),
			SuccessStatus: 200,
		},
		Doc{Summary: "List users", Tags: []string{"Users"}})
	Register(app, Config{Title: "Test API", Version: "1.0.0"}, reg)

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/openapi.json", nil))
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	ct := resp.Header.Get(fiber.HeaderContentType)
	if !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type: got %q, want application/json", ct)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var spec map[string]any
	if err := json.Unmarshal(body, &spec); err != nil {
		t.Fatalf("body is not JSON: %v\n%s", err, string(body))
	}
	if spec["openapi"] != "3.1.0" {
		t.Fatalf("openapi: got %v, want 3.1.0", spec["openapi"])
	}
	paths := spec["paths"].(map[string]any)
	if _, ok := paths["/users"]; !ok {
		t.Fatalf("/users path missing; got %+v", paths)
	}
}

func TestRegister_DocsReturnsHTML(t *testing.T) {
	app := fiber.New()
	reg := NewRegistry()
	Register(app, Config{Title: "Test API", Version: "1.0.0"}, reg)

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/docs", nil))
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	ct := resp.Header.Get(fiber.HeaderContentType)
	if !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content-type: got %q, want text/html", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	html := string(body)
	if !strings.Contains(html, "<title>Test API</title>") {
		t.Fatalf("title not in HTML; body=%s", html)
	}
	if !strings.Contains(html, "/openapi.json") {
		t.Fatalf("HTML must reference /openapi.json; body=%s", html)
	}
}

func TestSwaggerUIHTML_EscapesTitle(t *testing.T) {
	html := SwaggerUIHTML("<script>alert(1)</script>", SpecPath, nil)
	if strings.Contains(html, "<script>alert(1)</script>") {
		t.Fatalf("title not escaped; html=%s", html)
	}
	if !strings.Contains(html, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Fatalf("expected escaped title in html=%s", html)
	}
}

func TestSwaggerUIHTML_EmptyTitleFallsBack(t *testing.T) {
	html := SwaggerUIHTML("", SpecPath, nil)
	if !strings.Contains(html, "<title>API Docs</title>") {
		t.Fatalf("empty title should fall back to 'API Docs'; html=%s", html)
	}
}

func TestSwaggerUIHTML_SpecPathRenderedVerbatim(t *testing.T) {
	html := SwaggerUIHTML("API", "/internal/openapi.json", nil)
	if !strings.Contains(html, "url: '/internal/openapi.json'") {
		t.Fatalf("spec path not rendered verbatim; html=%s", html)
	}
}

func TestRegister_CustomUIPath(t *testing.T) {
	app := fiber.New()
	reg := NewRegistry()
	Register(app, Config{Title: "Custom", Version: "1.0.0"}, reg, WithUIPath("/swagger"))

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/swagger", nil))
	if err != nil {
		t.Fatalf("Test /swagger: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("custom UI path: got %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "<title>Custom</title>") {
		t.Fatalf("custom UI path body missing title; body=%s", string(body))
	}

	resp2, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/docs", nil))
	if err != nil {
		t.Fatalf("Test /docs: %v", err)
	}
	if resp2.StatusCode != 404 {
		t.Fatalf("with custom UI path, /docs must NOT be registered; got %d", resp2.StatusCode)
	}

	resp3, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/openapi.json", nil))
	if err != nil {
		t.Fatalf("Test /openapi.json: %v", err)
	}
	if resp3.StatusCode != 200 {
		t.Fatalf("/openapi.json must stay canonical even with custom UI path; got %d", resp3.StatusCode)
	}
}
