package web

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/notifications"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/web/openapi"
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

func TestStatusFromGatewayTimeout(t *testing.T) {
	dtos := []notifications.ContextDTO{{
		Context: "Request",
		Messages: []notifications.MessageDTO{
			{NotificationKey: "RequestTimeoutNotification", Semantic: domain.SemanticGatewayTimeout},
		},
	}}
	if got := statusFromNotifications(dtos); got != fiber.StatusGatewayTimeout {
		t.Fatalf("expected 504, got %d", got)
	}
}

func TestStatusFromReadTimeout(t *testing.T) {
	dtos := []notifications.ContextDTO{{
		Context: "Request",
		Messages: []notifications.MessageDTO{
			{NotificationKey: "ReadTimeoutNotification", Semantic: domain.SemanticRequestTimeout},
		},
	}}
	if got := statusFromNotifications(dtos); got != fiber.StatusRequestTimeout {
		t.Fatalf("expected 408, got %d", got)
	}
}

func TestStatusFromStateConflict(t *testing.T) {
	dtos := []notifications.ContextDTO{{
		Context: "Gadget",
		Messages: []notifications.MessageDTO{
			{NotificationKey: "EntityIsNotActiveNotification", Semantic: domain.SemanticStateConflict},
		},
	}}
	if got := statusFromNotifications(dtos); got != fiber.StatusConflict {
		t.Fatalf("expected 409, got %d", got)
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

// TestStatusFromBasicHTTPFamily locks Semantic→status for the six everyday
// statuses the framework maps but does not emit itself. A miss in
// semanticToStatus does not fail loudly — statusFromNotifications simply
// walks past it and returns 422 — so the table entry IS the contract and
// this test is what defends it.
func TestStatusFromBasicHTTPFamily(t *testing.T) {
	cases := []struct {
		key  string
		sem  domain.NotificationSemantic
		want int
	}{
		{"ResourceGoneNotification", domain.SemanticGone, fiber.StatusGone},
		{"PreconditionFailedNotification", domain.SemanticPreconditionFailed, fiber.StatusPreconditionFailed},
		{"UnsupportedMediaTypeNotification", domain.SemanticUnsupportedMediaType, fiber.StatusUnsupportedMediaType},
		{"TooManyRequestsNotification", domain.SemanticTooManyRequests, fiber.StatusTooManyRequests},
		{"NotImplementedNotification", domain.SemanticNotImplemented, fiber.StatusNotImplemented},
		{"BadGatewayNotification", domain.SemanticBadGateway, fiber.StatusBadGateway},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			dtos := []notifications.ContextDTO{{
				Context:  "Request",
				Messages: []notifications.MessageDTO{{NotificationKey: tc.key, Semantic: tc.sem}},
			}}
			if got := statusFromNotifications(dtos); got != tc.want {
				t.Fatalf("%s: expected %d, got %d", tc.key, tc.want, got)
			}
		})
	}
}

// TestResponseFromContextDTOs_CarriesNewSemanticLabels proves the envelope's
// `semantic` slot renders the new labels instead of degrading to
// "Validation" — the failure mode an out-of-vocabulary semantic produces.
func TestResponseFromContextDTOs_CarriesNewSemanticLabels(t *testing.T) {
	dtos := []notifications.ContextDTO{{
		Context: "Request",
		Messages: []notifications.MessageDTO{
			{NotificationKey: "TooManyRequestsNotification", Semantic: domain.SemanticTooManyRequests},
		},
	}}
	resp := ResponseFromContextDTOs(dtos, statusFromNotifications(dtos), "")
	if resp.Status != fiber.StatusTooManyRequests {
		t.Fatalf("expected status 429, got %d", resp.Status)
	}
	if got := resp.Errors[0].Messages[0].Semantic; got != "TooManyRequests" {
		t.Fatalf("expected semantic TooManyRequests, got %q", got)
	}
	if resp.Description != "Too Many Requests" {
		t.Fatalf("expected description \"Too Many Requests\", got %q", resp.Description)
	}
}

// TestStatusFromClosingFamily locks Semantic→status for the eight statuses
// added to close the vocabulary. Same reason as the basic family above: a miss
// in semanticToStatus is silent (the walk just continues and 422 comes out),
// so the table entry IS the contract.
func TestStatusFromClosingFamily(t *testing.T) {
	cases := []struct {
		key  string
		sem  domain.NotificationSemantic
		want int
	}{
		{"PaymentRequiredNotification", domain.SemanticPaymentRequired, fiber.StatusPaymentRequired},
		{"NotAcceptableNotification", domain.SemanticNotAcceptable, fiber.StatusNotAcceptable},
		{"RangeNotSatisfiableNotification", domain.SemanticRangeNotSatisfiable, fiber.StatusRequestedRangeNotSatisfiable},
		{"ResourceLockedNotification", domain.SemanticLocked, fiber.StatusLocked},
		{"PreconditionRequiredNotification", domain.SemanticPreconditionRequired, fiber.StatusPreconditionRequired},
		{"UnavailableForLegalReasonsNotification", domain.SemanticUnavailableForLegalReasons, fiber.StatusUnavailableForLegalReasons},
		{"InsufficientStorageNotification", domain.SemanticInsufficientStorage, fiber.StatusInsufficientStorage},
		{"RequestHeaderFieldsTooLargeNotification", domain.SemanticRequestHeaderFieldsTooLarge, fiber.StatusRequestHeaderFieldsTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			dtos := []notifications.ContextDTO{{
				Context:  "Request",
				Messages: []notifications.MessageDTO{{NotificationKey: tc.key, Semantic: tc.sem}},
			}}
			if got := statusFromNotifications(dtos); got != tc.want {
				t.Fatalf("%s: expected %d, got %d", tc.key, tc.want, got)
			}
		})
	}
}

// TestEverySemanticHasADocumentedExample is the guard that keeps the two
// registries from drifting: every status the Semantic table can produce must
// have an openapi.DefaultErrorExample, otherwise a route DECLARING that status
// renders the 500-shaped fallback in its spec while the runtime emits the real
// envelope. Adding a semantic without its example is exactly the kind of
// half-landed change this fails on.
func TestEverySemanticHasADocumentedExample(t *testing.T) {
	for sem, status := range semanticToStatus {
		if _, ok := openapi.DefaultErrorExample(status); !ok {
			t.Errorf("semantic %s maps to %d, which has no openapi.DefaultErrorExample", sem, status)
		}
	}
}

// TestEverySemanticIsMappedAndLabelled asserts the enum has no member the HTTP
// table forgot, and none whose String() silently fell back to "Validation" —
// the two ways a new semantic half-lands and degrades to 422 on the wire
// without anything failing.
func TestEverySemanticIsMappedAndLabelled(t *testing.T) {
	for sem := domain.SemanticValidation; sem <= domain.SemanticRequestHeaderFieldsTooLarge; sem++ {
		if _, ok := semanticToStatus[sem]; !ok {
			t.Errorf("semantic %d has no entry in semanticToStatus", int(sem))
		}
		if sem != domain.SemanticValidation && sem.String() == "Validation" {
			t.Errorf("semantic %d has no String() case — it renders as %q on the wire", int(sem), "Validation")
		}
	}
}
