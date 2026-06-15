package web

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/notifications"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/gofiber/fiber/v3"
)

func TestStatusFromValidationOnly(t *testing.T) {
	dtos := []notifications.ContextDTO{{
		Context: "User",
		Messages: []notifications.MessageDTO{
			{NotificationKey: "RequiredFieldNotification", Semantic: domain.SemanticValidation},
			{NotificationKey: "InvalidIDUUIDNotification", Semantic: domain.SemanticValidation},
		},
	}}
	if got := statusFromNotifications(dtos); got != fiber.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d", got)
	}
}

func TestStatusFromMixedFavorNonValidation(t *testing.T) {
	dtos := []notifications.ContextDTO{{
		Context: "User",
		Messages: []notifications.MessageDTO{
			{NotificationKey: "RequiredFieldNotification", Semantic: domain.SemanticValidation},
			{NotificationKey: "EmailAlreadyExistsNotification", Semantic: domain.SemanticConflict},
		},
	}}
	if got := statusFromNotifications(dtos); got != fiber.StatusConflict {
		t.Fatalf("expected 409, got %d", got)
	}
}

func TestStatusFromSchema(t *testing.T) {
	dtos := []notifications.ContextDTO{{
		Context: "Schema",
		Messages: []notifications.MessageDTO{
			{NotificationKey: "SchemaViolationNotification", Semantic: domain.SemanticSchema},
		},
	}}
	if got := statusFromNotifications(dtos); got != fiber.StatusBadRequest {
		t.Fatalf("expected 400, got %d", got)
	}
}

func TestStatusFromSchemaMixedFavorsNonValidation(t *testing.T) {
	dtos := []notifications.ContextDTO{{
		Context: "Schema",
		Messages: []notifications.MessageDTO{
			{NotificationKey: "RequiredFieldNotification", Semantic: domain.SemanticValidation},
			{NotificationKey: "RequiredFieldNotification", Semantic: domain.SemanticSchema},
		},
	}}
	if got := statusFromNotifications(dtos); got != fiber.StatusBadRequest {
		t.Fatalf("expected 400, got %d", got)
	}
}

func TestStatusFromNotFound(t *testing.T) {
	dtos := []notifications.ContextDTO{{
		Context: "User",
		Messages: []notifications.MessageDTO{
			{NotificationKey: "RecordNotFoundNotification", Semantic: domain.SemanticNotFound},
		},
	}}
	if got := statusFromNotifications(dtos); got != fiber.StatusNotFound {
		t.Fatalf("expected 404, got %d", got)
	}
}

func TestStatusFromForbidden(t *testing.T) {
	dtos := []notifications.ContextDTO{{
		Context: "User",
		Messages: []notifications.MessageDTO{
			{NotificationKey: "DeleteNotAllowedNotification", Semantic: domain.SemanticForbidden},
		},
	}}
	if got := statusFromNotifications(dtos); got != fiber.StatusForbidden {
		t.Fatalf("expected 403, got %d", got)
	}
}

func TestStatusFromEmpty(t *testing.T) {
	if got := statusFromNotifications(nil); got != fiber.StatusUnprocessableEntity {
		t.Fatalf("expected 422 fallback, got %d", got)
	}
}

// TestRespondFromResult_SuccessBodyCarriesID locks the contract documented in
// CLAUDE.md: a successful write response carries the created entity's ID as a
// plain string in `data`. The fix that makes this pass lives in
// domain.ID.MarshalJSON — without it, a struct-typed ID serializes to `{}`.
func TestRespondFromResult_SuccessBodyCarriesID(t *testing.T) {
	app := fiber.New()
	id := domain.NewID("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")

	app.Post("/things", func(c fiber.Ctx) error {
		return RespondFromResult(c, pipeline.Success(id), fiber.StatusCreated)
	})

	req := httptest.NewRequest("POST", "/things", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusCreated {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}

	raw, _ := io.ReadAll(resp.Body)
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("invalid JSON %s: %v", raw, err)
	}

	data, ok := body["data"].(string)
	if !ok {
		t.Fatalf("expected `data` to be a JSON string, got %T (%v) in body %s", body["data"], body["data"], raw)
	}
	if data != id.Value() {
		t.Fatalf("expected data=%q, got %q", id.Value(), data)
	}
	if body["success"] != true {
		t.Fatalf("expected success=true, got %v", body["success"])
	}
}

// TestRespondFromResult_ExceptionBranchCarriesCanonicalEnvelope proves the
// Exception branch (panic caught by pipeline.Run's defer/recover) lands in
// the canonical envelope shape carrying InternalServerErrorNotification —
// same JSON every other failure path emits. The message is the English
// default (no Pipeline/Translator involved here; see RespondWithInternalServerError
// for the rationale).
func TestRespondFromResult_ExceptionBranchCarriesCanonicalEnvelope(t *testing.T) {
	app := fiber.New()
	app.Get("/boom", func(c fiber.Ctx) error {
		return RespondFromResult(c, pipeline.Exception[any](errInjected{}), fiber.StatusOK)
	})

	req := httptest.NewRequest("GET", "/boom", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", resp.StatusCode)
	}

	raw, _ := io.ReadAll(resp.Body)
	var body Response
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("invalid JSON %s: %v", raw, err)
	}
	if body.Success {
		t.Fatalf("expected success=false, got true")
	}
	if body.Status != fiber.StatusInternalServerError {
		t.Fatalf("expected envelope status 500, got %d", body.Status)
	}
	if len(body.Errors) != 1 {
		t.Fatalf("expected 1 errors entry, got %d (body %s)", len(body.Errors), raw)
	}
	if body.Errors[0].Context != "Server" {
		t.Fatalf("expected context=\"Server\", got %q", body.Errors[0].Context)
	}
	if len(body.Errors[0].Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(body.Errors[0].Messages))
	}
	msg := body.Errors[0].Messages[0]
	if msg.NotificationKey != "InternalServerErrorNotification" {
		t.Fatalf("expected NotificationKey=InternalServerErrorNotification, got %q", msg.NotificationKey)
	}
	if msg.Semantic != "Internal" {
		t.Fatalf("expected Semantic=Internal, got %q", msg.Semantic)
	}
	if msg.Message != "Internal server error." {
		t.Fatalf("expected Message=\"Internal server error.\", got %q", msg.Message)
	}
}

type errInjected struct{}

func (errInjected) Error() string { return "injected exception" }
