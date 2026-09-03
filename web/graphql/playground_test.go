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

// The Headers tab is this page's Authorize button (Swagger UI gets one from the
// spec's bearerAuth scheme). Without persistence the token is retyped on every
// reload, which is not parity with a Swagger UI that remembers it.
func TestPlayground_PersistsHeadersForBearerParity(t *testing.T) {
	page := graphiqlHTML("/graphql")

	if !strings.Contains(page, "shouldPersistHeaders: true") {
		t.Error("the playground must persist request headers so a pasted bearer survives a reload")
	}
	if !strings.Contains(page, "Authorization") {
		t.Error("the playground should seed the Headers tab with the Authorization key")
	}
}
