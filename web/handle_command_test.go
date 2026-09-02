package web

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	fwresults "github.com/ClaudioSchirmer/omnicore/application/results"
	"github.com/ClaudioSchirmer/omnicore/application/translation"
	"github.com/ClaudioSchirmer/omnicore/web/responses"
	"github.com/gofiber/fiber/v3"
)

type testNoBodyCmd struct {
	pipeline.CommandByIDBase
}

// capturingNoBodyHandler captures only the PathID — used by CommandByID
// tests where no body parsing happens. Returns fwresults.None so the route
// declaration can pair with responses.NoBody (the canonical no-data shape).
type capturingNoBodyHandler struct {
	gotPathID string
}

func (h *capturingNoBodyHandler) Handle(ctx *configuration.AppContext, cmd *testNoBodyCmd) (fwresults.None, error) {
	h.gotPathID = cmd.PathID()
	return fwresults.None{}, nil
}

func newTestPipeline() *pipeline.Pipeline {
	return pipeline.New(translation.Default())
}

// CommandByID is the no-body, path-ID-only wrapper used by Archive,
// Unarchive and Delete endpoints. Body parsing belongs to
// CommandWithBody{,ID}; this one only injects the path ID and runs
// the projection on success.

func TestHandleCommandByID_InjectsPathID(t *testing.T) {
	app := fiber.New()
	pipe := newTestPipeline()
	cap := &capturingNoBodyHandler{}

	app.Patch("/things/:id/archive", CommandByID(pipe, responses.NoBody, cap, fiber.StatusOK))

	req := httptest.NewRequest("PATCH", "/things/9a1f6e2c-8b47-4d3a-9c5e-1f0b7d2a6e34/archive", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test err: %v", err)
	}
	if resp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d (body=%s)", resp.StatusCode, b)
	}
	if cap.gotPathID != "9a1f6e2c-8b47-4d3a-9c5e-1f0b7d2a6e34" {
		t.Errorf("expected PathID=xyz, got %q", cap.gotPathID)
	}
}

func TestHandleCommandByID_IgnoresBody(t *testing.T) {
	app := fiber.New()
	pipe := newTestPipeline()
	cap := &capturingNoBodyHandler{}

	app.Delete("/things/:id", CommandByID(pipe, responses.NoBody, cap, fiber.StatusNoContent))

	// Body is provided but the wrapper does not parse it — should still 204.
	req := httptest.NewRequest("DELETE", "/things/9a1f6e2c-8b47-4d3a-9c5e-1f0b7d2a6e34", nil)
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test err: %v", err)
	}
	if resp.StatusCode != fiber.StatusNoContent {
		t.Fatalf("expected 204, got %d", resp.StatusCode)
	}
	if cap.gotPathID != "9a1f6e2c-8b47-4d3a-9c5e-1f0b7d2a6e34" {
		t.Errorf("expected PathID=abc, got %q", cap.gotPathID)
	}
}

// TestHandleCommandByID_NoBodyOmitsDataField proves the wrapper detects
// responses.None at runtime and emits the success envelope WITHOUT a "data"
// key — matches the conventional "no body" shape for state-transition
// endpoints (DELETE 204, PATCH archive/unarchive 200).
func TestHandleCommandByID_NoBodyOmitsDataField(t *testing.T) {
	app := fiber.New()
	pipe := newTestPipeline()
	cap := &capturingNoBodyHandler{}

	app.Patch("/things/:id/archive", CommandByID(pipe, responses.NoBody, cap, fiber.StatusOK))

	req := httptest.NewRequest("PATCH", "/things/9a1f6e2c-8b47-4d3a-9c5e-1f0b7d2a6e34/archive", nil)
	resp, _ := app.Test(req)
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var envelope map[string]any
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("unmarshal envelope: %v (body=%s)", err, body)
	}
	if _, hasData := envelope["data"]; hasData {
		t.Errorf("expected envelope WITHOUT data field, got %s", body)
	}
}

// TestHandleCommandByID_CustomProjection proves a service-defined response
// projection lands on the wire envelope's "data" field — the customization
// path the framework opens up beyond the NoBody default.
type customResp struct {
	ID string `json:"id"`
}

func TestHandleCommandByID_CustomProjection(t *testing.T) {
	app := fiber.New()
	pipe := newTestPipeline()
	cap := &capturingNoBodyHandler{}

	app.Patch("/things/:id/archive", CommandByID(pipe,
		func(_ fwresults.None) customResp { return customResp{ID: "demo"} },
		cap, fiber.StatusOK))

	req := httptest.NewRequest("PATCH", "/things/9a1f6e2c-8b47-4d3a-9c5e-1f0b7d2a6e34/archive", nil)
	resp, _ := app.Test(req)
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var envelope map[string]any
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("unmarshal envelope: %v (body=%s)", err, body)
	}
	data, ok := envelope["data"].(map[string]any)
	if !ok {
		t.Fatalf("expected data object, got %v", envelope["data"])
	}
	if data["id"] != "demo" {
		t.Errorf("expected data.id=demo, got %v", data["id"])
	}
}

// ---- reflectExpectedJSONKeys ------------------------------------------------

type sampleReqForReflect struct {
	pipeline.CommandByIDBase         // anonymous embed: must be skipped
	Name                     string  `json:"name"`
	Email                    string  `json:"email,omitempty"`
	Secret                   string  `json:"-"`     // skipped
	Phone                    *string `json:"phone"` // pointer field still included
	hidden                   string  // unexported — skipped
	NoTag                    string  // no json tag → uses field name
}

func TestReflectExpectedJSONKeys(t *testing.T) {
	got := reflectExpectedJSONKeys(reflect.TypeOf(sampleReqForReflect{}))
	want := []string{"NoTag", "email", "name", "phone"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reflectExpectedJSONKeys mismatch:\n got: %v\nwant: %v", got, want)
	}
}

func TestReflectExpectedJSONKeys_Cached(t *testing.T) {
	typ := reflect.TypeOf(sampleReqForReflect{})
	first := reflectExpectedJSONKeys(typ)
	second := reflectExpectedJSONKeys(typ)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("cached result differs from fresh:\n first: %v\nsecond: %v", first, second)
	}
}

func TestMissingKeys(t *testing.T) {
	expected := []string{"email", "name", "phone", "username"}
	cases := []struct {
		raw  map[string]json.RawMessage
		want []string
	}{
		{map[string]json.RawMessage{"name": json.RawMessage(`"x"`), "email": nil, "username": nil, "phone": nil}, []string{}},
		{map[string]json.RawMessage{"name": nil, "email": nil, "username": nil}, []string{"phone"}},
		{map[string]json.RawMessage{}, []string{"email", "name", "phone", "username"}},
	}
	for i, tc := range cases {
		got := missingKeys(expected, tc.raw)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("case %d: got %v want %v", i, got, tc.want)
		}
	}
}
