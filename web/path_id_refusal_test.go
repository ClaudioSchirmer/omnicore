package web

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http/httptest"
	"testing"

	fwresults "github.com/ClaudioSchirmer/omnicore/application/results"
	"github.com/ClaudioSchirmer/omnicore/web/responses"
	"github.com/gofiber/fiber/v3"
)

// A `:id` segment that is not a UUID never reaches the handler. What the
// consumer hears depends on what they asked for, not on where the view is
// stored: a read names no record (404), a write violated the request shape
// (400). Before this, both answers were the driver's — SQLSTATE 22P02 rendered
// as a 500 on a relational backing, while the same route over a Mongo view
// answered 404 because the string simply matched no _id.

// refusalOf runs the request and returns (status, first notificationKey).
func refusalOf(t *testing.T, app *fiber.App, method, url string, body []byte) (int, string) {
	t.Helper()
	var req = httptest.NewRequest(method, url, nil)
	if body != nil {
		req = httptest.NewRequest(method, url, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	var env struct {
		Errors []struct {
			Context  string `json:"context"`
			Messages []struct {
				NotificationKey string `json:"notificationKey"`
				Field           string `json:"field"`
				Value           string `json:"value"`
			} `json:"messages"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal %s: %v", raw, err)
	}
	if len(env.Errors) == 0 || len(env.Errors[0].Messages) == 0 {
		t.Fatalf("no notification on the envelope: %s", raw)
	}
	m := env.Errors[0].Messages[0]
	if m.Field != "id" {
		t.Errorf("field = %q, want the wire spelling of the segment", m.Field)
	}
	if m.Value != "not-a-uuid" {
		t.Errorf("value = %q, want the rejected segment echoed back", m.Value)
	}
	if env.Errors[0].Context != "Request" {
		t.Errorf("context = %q, want Request", env.Errors[0].Context)
	}
	return resp.StatusCode, m.NotificationKey
}

func TestQueryByID_MalformedIDIsTyped404(t *testing.T) {
	app := fiber.New()
	h := &capturingIDHandler{}
	app.Get("/users/:id", QueryByID(newTestPipeline(), testFindIDRequest{}, rawItem, h))

	status, key := refusalOf(t, app, "GET", "/users/not-a-uuid", nil)
	if status != fiber.StatusNotFound {
		t.Errorf("status = %d, want 404", status)
	}
	if key != "UnknownIDAddressNotification" {
		t.Errorf("notificationKey = %q, want UnknownIDAddressNotification", key)
	}
	if h.got != nil {
		t.Error("the handler must not run for an address that is not an address")
	}
}

func TestCommandByID_MalformedIDIsTyped400(t *testing.T) {
	app := fiber.New()
	cap := &capturingNoBodyHandler{}
	app.Patch("/things/:id/archive", CommandByID(newTestPipeline(),
		func(_ fwresults.None) responses.None { return responses.None{} },
		cap, fiber.StatusOK))

	status, key := refusalOf(t, app, "PATCH", "/things/not-a-uuid/archive", nil)
	if status != fiber.StatusBadRequest {
		t.Errorf("status = %d, want 400", status)
	}
	if key != "MalformedIDNotification" {
		t.Errorf("notificationKey = %q, want MalformedIDNotification", key)
	}
}

func TestCommandWithBodyID_MalformedIDIsTyped400(t *testing.T) {
	app := fiber.New()
	h := &capturingUpdateHandler{}
	app.Put("/things/:id", CommandWithBodyID(newTestPipeline(), testUpdateRequest{},
		responses.NoBody, h, fiber.StatusOK))

	body, _ := json.Marshal(map[string]string{"name": "x", "email": "y@z.w"})
	status, key := refusalOf(t, app, "PUT", "/things/not-a-uuid", body)
	if status != fiber.StatusBadRequest {
		t.Errorf("status = %d, want 400", status)
	}
	if key != "MalformedIDNotification" {
		t.Errorf("notificationKey = %q, want MalformedIDNotification", key)
	}
	if h.gotPathID != "" {
		t.Errorf("the command must never be built, got pathID %q", h.gotPathID)
	}
}

// An EMPTY segment is a wiring mistake, not a consumer error: the route was
// mounted without `:id`. It keeps reaching the handler, where RequirePathID's
// developer panic diagnoses it — answering 400 there would blame the caller
// for something only the developer can fix.
func TestCommandWithBodyID_EmptySegmentIsNotARefusal(t *testing.T) {
	app := fiber.New()
	h := &capturingUpdateHandler{}
	app.Put("/things", CommandWithBodyID(newTestPipeline(), testUpdateRequest{},
		responses.NoBody, h, fiber.StatusOK))

	body, _ := json.Marshal(map[string]string{"name": "x", "email": "y@z.w"})
	req := httptest.NewRequest("PUT", "/things", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, _ := app.Test(req)
	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200 (body %s)", resp.StatusCode, raw)
	}
	if h.gotPathID != "" {
		t.Errorf("expected the empty path id to pass through, got %q", h.gotPathID)
	}
}
