package graphql

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/application/translation"
	"github.com/gofiber/fiber/v3"
)

func TestPlayground_ServesGraphiQLPointingAtEndpoint(t *testing.T) {
	reg := New(pipeline.New(translation.Default()))
	app := fiber.New()
	app.Get("/graphql/ui", reg.Playground("/graphql"))

	resp, err := app.Test(httptest.NewRequest("GET", "/graphql/ui", nil))
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("playground status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get(fiber.HeaderContentType); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type = %q, want text/html", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	page := string(body)
	if !strings.Contains(page, "GraphiQL") {
		t.Errorf("playground page should embed GraphiQL")
	}
	if !strings.Contains(page, "'/graphql'") {
		t.Errorf("playground page should target the endpoint path, got:\n%s", page)
	}
}
