package web

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	fwresults "github.com/ClaudioSchirmer/omnicore/application/results"
	"github.com/ClaudioSchirmer/omnicore/web/responses"
	"github.com/gofiber/fiber/v3"
)

// ---- test types -----------------------------------------------------------

type testInsertCmd struct {
	pipeline.CommandBase
	Name  string
	Email string
	Phone *string
}

type testInsertRequest struct {
	Name  string  `json:"name"`
	Email string  `json:"email"`
	Phone *string `json:"phone,omitempty"`
}

func (r testInsertRequest) ToCommand() *testInsertCmd {
	return &testInsertCmd{Name: r.Name, Email: r.Email, Phone: r.Phone}
}

type testUpdateCmd struct {
	pipeline.CommandByIDBase
	Name  string
	Email string
}

type testUpdateRequest struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

func (r testUpdateRequest) ToCommand() *testUpdateCmd {
	return &testUpdateCmd{Name: r.Name, Email: r.Email}
}

type testNestedItem struct {
	Label string `json:"label"`
}

type testInsertWithNestedCmd struct {
	pipeline.CommandBase
	Name  string
	Items []testNestedItem
}

type testInsertWithNestedRequest struct {
	Name  string           `json:"name"`
	Items []testNestedItem `json:"items"`
}

func (r testInsertWithNestedRequest) ToCommand() *testInsertWithNestedCmd {
	return &testInsertWithNestedCmd{Name: r.Name, Items: r.Items}
}

type testTypedFieldCmd struct {
	pipeline.CommandBase
	Age int
}

type testTypedFieldRequest struct {
	Age int `json:"age"`
}

func (r testTypedFieldRequest) ToCommand() *testTypedFieldCmd {
	return &testTypedFieldCmd{Age: r.Age}
}

// ---- handlers --------------------------------------------------------------

type capturingInsertHandler struct {
	gotCmd *testInsertCmd
}

func (h *capturingInsertHandler) Handle(ctx *configuration.AppContext, cmd *testInsertCmd) (fwresults.None, error) {
	h.gotCmd = cmd
	return fwresults.None{}, nil
}

type capturingUpdateHandler struct {
	pipeline.FullBody // strict: all fields required
	gotCmd            *testUpdateCmd
	gotPathID         string
}

func (h *capturingUpdateHandler) Handle(ctx *configuration.AppContext, cmd *testUpdateCmd) (fwresults.None, error) {
	h.gotCmd = cmd
	h.gotPathID = cmd.PathID()
	return fwresults.None{}, nil
}

type capturingInsertWithNestedHandler struct {
	gotCmd *testInsertWithNestedCmd
}

func (h *capturingInsertWithNestedHandler) Handle(ctx *configuration.AppContext, cmd *testInsertWithNestedCmd) (fwresults.None, error) {
	h.gotCmd = cmd
	return fwresults.None{}, nil
}

type capturingTypedHandler struct {
	gotAge int
}

func (h *capturingTypedHandler) Handle(ctx *configuration.AppContext, cmd *testTypedFieldCmd) (fwresults.None, error) {
	h.gotAge = cmd.Age
	return fwresults.None{}, nil
}

// testCtxCapturingCmd carries the language the handler saw — proves the
// AppContext threads through the wire boundary into the Handler.Handle.
// (ToCommand is ctx-free by design; the application layer — handler /
// cmd.ToEntity — is what interprets ctx into business-named fields.)
type testCtxCapturingCmd struct {
	pipeline.CommandBase
	Name string
}

type testCtxCapturingRequest struct {
	Name string `json:"name"`
}

func (r testCtxCapturingRequest) ToCommand() *testCtxCapturingCmd {
	return &testCtxCapturingCmd{Name: r.Name}
}

type capturingCtxHandler struct {
	gotCmd   *testCtxCapturingCmd
	seenLang configuration.Language
}

func (h *capturingCtxHandler) Handle(ctx *configuration.AppContext, cmd *testCtxCapturingCmd) (fwresults.None, error) {
	h.gotCmd = cmd
	h.seenLang = ctx.Language()
	return fwresults.None{}, nil
}

// ---- CommandWithBody --------------------------------------------------------

func TestHandleCommandWithBody_AppContextFlowsIntoHandler(t *testing.T) {
	app := fiber.New()
	app.Use(AppContextMiddleware())
	pipe := newTestPipeline()
	h := &capturingCtxHandler{}

	app.Post("/things", CommandWithBody(pipe, testCtxCapturingRequest{}, responses.NoBody, h, fiber.StatusCreated))

	body, _ := json.Marshal(map[string]string{"name": "alice"})
	req := httptest.NewRequest("POST", "/things", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Language", "en")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != fiber.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 201, got %d (body=%s)", resp.StatusCode, b)
	}
	if h.gotCmd == nil {
		t.Fatal("expected handler called")
	}
	if h.seenLang != configuration.LangENG {
		t.Errorf("expected Handler to see LangENG via ctx, got %v", h.seenLang)
	}
}

func TestHandleCommandWithBody_HappyPath(t *testing.T) {
	app := fiber.New()
	pipe := newTestPipeline()
	h := &capturingInsertHandler{}

	app.Post("/things", CommandWithBody(pipe, testInsertRequest{}, responses.NoBody, h, fiber.StatusCreated))

	body, _ := json.Marshal(map[string]string{"name": "alice", "email": "a@x.com"})
	req := httptest.NewRequest("POST", "/things", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test err: %v", err)
	}
	if resp.StatusCode != fiber.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 201, got %d (body=%s)", resp.StatusCode, b)
	}
	if h.gotCmd == nil || h.gotCmd.Name != "alice" || h.gotCmd.Email != "a@x.com" {
		t.Errorf("ToCommand did not transfer fields correctly: %+v", h.gotCmd)
	}
}

func TestHandleCommandWithBody_MalformedJSON_400(t *testing.T) {
	app := fiber.New()
	pipe := newTestPipeline()
	h := &capturingInsertHandler{}

	app.Post("/things", CommandWithBody(pipe, testInsertRequest{}, responses.NoBody, h, fiber.StatusCreated))

	req := httptest.NewRequest("POST", "/things", bytes.NewReader([]byte("{not json")))
	req.Header.Set("Content-Type", "application/json")
	resp, _ := app.Test(req)
	if resp.StatusCode != fiber.StatusBadRequest {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 400, got %d (body=%s)", resp.StatusCode, b)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "SchemaViolationNotification") {
		t.Errorf("expected SchemaViolationNotification, got: %s", body)
	}
	if !strings.Contains(string(body), `"context":"Schema"`) {
		t.Errorf("expected context Schema, got: %s", body)
	}
}

func TestHandleCommandWithBody_TypeMismatch_400(t *testing.T) {
	app := fiber.New()
	pipe := newTestPipeline()
	h := &capturingTypedHandler{}

	app.Post("/things", CommandWithBody(pipe, testTypedFieldRequest{}, responses.NoBody, h, fiber.StatusOK))

	// "age" should be int — pass string instead.
	body, _ := json.Marshal(map[string]string{"age": "twenty"})
	req := httptest.NewRequest("POST", "/things", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, _ := app.Test(req)
	if resp.StatusCode != fiber.StatusBadRequest {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 400, got %d (body=%s)", resp.StatusCode, b)
	}
	bodyResp, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(bodyResp), "SchemaViolationNotification") {
		t.Errorf("expected SchemaViolationNotification, got: %s", bodyResp)
	}
}

func TestHandleCommandWithBody_EmptyBody_Lenient_HitsHandler(t *testing.T) {
	app := fiber.New()
	pipe := newTestPipeline()
	h := &capturingInsertHandler{}

	app.Post("/things", CommandWithBody(pipe, testInsertRequest{}, responses.NoBody, h, fiber.StatusCreated))

	req := httptest.NewRequest("POST", "/things", nil)
	resp, _ := app.Test(req)
	// Lenient handler — empty body produces zero-value Request, dispatches normally.
	if resp.StatusCode != fiber.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 201 (lenient), got %d (body=%s)", resp.StatusCode, b)
	}
	if h.gotCmd == nil || h.gotCmd.Name != "" {
		t.Errorf("expected zero-value cmd, got %+v", h.gotCmd)
	}
}

func TestHandleCommandWithBody_FieldExtraIgnored(t *testing.T) {
	app := fiber.New()
	pipe := newTestPipeline()
	h := &capturingInsertHandler{}

	app.Post("/things", CommandWithBody(pipe, testInsertRequest{}, responses.NoBody, h, fiber.StatusCreated))

	body := []byte(`{"name":"alice","email":"a@x.com","unknownField":"ignored"}`)
	req := httptest.NewRequest("POST", "/things", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, _ := app.Test(req)
	if resp.StatusCode != fiber.StatusCreated {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}
	if h.gotCmd.Name != "alice" {
		t.Errorf("expected name=alice, got %q", h.gotCmd.Name)
	}
}

func TestHandleCommandWithBody_NestedHappyPath(t *testing.T) {
	app := fiber.New()
	pipe := newTestPipeline()
	h := &capturingInsertWithNestedHandler{}

	app.Post("/things", CommandWithBody(pipe, testInsertWithNestedRequest{}, responses.NoBody, h, fiber.StatusCreated))

	body := []byte(`{"name":"x","items":[{"label":"a"},{"label":"b"}]}`)
	req := httptest.NewRequest("POST", "/things", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, _ := app.Test(req)
	if resp.StatusCode != fiber.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 201, got %d (body=%s)", resp.StatusCode, b)
	}
	if len(h.gotCmd.Items) != 2 || h.gotCmd.Items[0].Label != "a" || h.gotCmd.Items[1].Label != "b" {
		t.Errorf("nested items not transferred: %+v", h.gotCmd.Items)
	}
}

func TestHandleCommandWithBody_EmptyArrayIsValid(t *testing.T) {
	app := fiber.New()
	pipe := newTestPipeline()
	h := &capturingInsertWithNestedHandler{}

	app.Post("/things", CommandWithBody(pipe, testInsertWithNestedRequest{}, responses.NoBody, h, fiber.StatusCreated))

	body := []byte(`{"name":"x","items":[]}`)
	req := httptest.NewRequest("POST", "/things", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, _ := app.Test(req)
	if resp.StatusCode != fiber.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 201, got %d (body=%s)", resp.StatusCode, b)
	}
	if len(h.gotCmd.Items) != 0 {
		t.Errorf("expected empty items, got %+v", h.gotCmd.Items)
	}
}

// ---- CommandWithBodyID (strict via FullBody) ---------------------------

func TestHandleCommandWithBodyID_HappyPath(t *testing.T) {
	app := fiber.New()
	pipe := newTestPipeline()
	h := &capturingUpdateHandler{}

	app.Put("/things/:id", CommandWithBodyID(pipe, testUpdateRequest{}, responses.NoBody, h, fiber.StatusOK))

	body, _ := json.Marshal(map[string]string{"name": "bob", "email": "b@x.com"})
	req := httptest.NewRequest("PUT", "/things/9a1f6e2c-8b47-4d3a-9c5e-1f0b7d2a6e34", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, _ := app.Test(req)
	if resp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d (body=%s)", resp.StatusCode, b)
	}
	if h.gotPathID != "9a1f6e2c-8b47-4d3a-9c5e-1f0b7d2a6e34" {
		t.Errorf("expected PathID=abc, got %q", h.gotPathID)
	}
	if h.gotCmd.Name != "bob" || h.gotCmd.Email != "b@x.com" {
		t.Errorf("ToCommand transfer failed: %+v", h.gotCmd)
	}
}

func TestHandleCommandWithBodyID_MissingField_400_SchemaContext(t *testing.T) {
	app := fiber.New()
	pipe := newTestPipeline()
	h := &capturingUpdateHandler{}

	app.Put("/things/:id", CommandWithBodyID(pipe, testUpdateRequest{}, responses.NoBody, h, fiber.StatusOK))

	body, _ := json.Marshal(map[string]string{"name": "bob"}) // missing email
	req := httptest.NewRequest("PUT", "/things/9a1f6e2c-8b47-4d3a-9c5e-1f0b7d2a6e34", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, _ := app.Test(req)
	if resp.StatusCode != fiber.StatusBadRequest {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 400, got %d (body=%s)", resp.StatusCode, b)
	}
	bodyResp, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(bodyResp), "RequiredFieldNotification") {
		t.Errorf("expected RequiredFieldNotification, got: %s", bodyResp)
	}
	if !strings.Contains(string(bodyResp), `"field":"email"`) {
		t.Errorf("expected field=email, got: %s", bodyResp)
	}
	if !strings.Contains(string(bodyResp), `"context":"Schema"`) {
		t.Errorf("expected context=Schema, got: %s", bodyResp)
	}
}

func TestHandleCommandWithBodyID_MissingMultipleFields_400(t *testing.T) {
	app := fiber.New()
	pipe := newTestPipeline()
	h := &capturingUpdateHandler{}

	app.Put("/things/:id", CommandWithBodyID(pipe, testUpdateRequest{}, responses.NoBody, h, fiber.StatusOK))

	req := httptest.NewRequest("PUT", "/things/9a1f6e2c-8b47-4d3a-9c5e-1f0b7d2a6e34", bytes.NewReader([]byte("{}")))
	req.Header.Set("Content-Type", "application/json")
	resp, _ := app.Test(req)
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	for _, want := range []string{`"field":"name"`, `"field":"email"`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("expected %s in body, got: %s", want, body)
		}
	}
}

func TestHandleCommandWithBodyID_EmptyBody_400(t *testing.T) {
	app := fiber.New()
	pipe := newTestPipeline()
	h := &capturingUpdateHandler{}

	app.Put("/things/:id", CommandWithBodyID(pipe, testUpdateRequest{}, responses.NoBody, h, fiber.StatusOK))

	req := httptest.NewRequest("PUT", "/things/9a1f6e2c-8b47-4d3a-9c5e-1f0b7d2a6e34", nil)
	resp, _ := app.Test(req)
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestHandleCommandWithBodyID_MalformedJSON_400(t *testing.T) {
	app := fiber.New()
	pipe := newTestPipeline()
	h := &capturingUpdateHandler{}

	app.Put("/things/:id", CommandWithBodyID(pipe, testUpdateRequest{}, responses.NoBody, h, fiber.StatusOK))

	req := httptest.NewRequest("PUT", "/things/9a1f6e2c-8b47-4d3a-9c5e-1f0b7d2a6e34", bytes.NewReader([]byte("{nope")))
	req.Header.Set("Content-Type", "application/json")
	resp, _ := app.Test(req)
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "SchemaViolationNotification") {
		t.Errorf("expected SchemaViolationNotification, got: %s", body)
	}
}

func TestHandleCommandWithBodyID_PathIDEmpty(t *testing.T) {
	app := fiber.New()
	pipe := newTestPipeline()
	h := &capturingUpdateHandler{}

	// Route without :id segment — Fiber returns "" for c.Params("id")
	app.Put("/things", CommandWithBodyID(pipe, testUpdateRequest{}, responses.NoBody, h, fiber.StatusOK))

	body, _ := json.Marshal(map[string]string{"name": "x", "email": "y@z.w"})
	req := httptest.NewRequest("PUT", "/things", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, _ := app.Test(req)
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if h.gotPathID != "" {
		t.Errorf("expected empty PathID, got %q", h.gotPathID)
	}
}
