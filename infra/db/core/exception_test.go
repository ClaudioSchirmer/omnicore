package core

import (
	"errors"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

func TestSingleNotificationError(t *testing.T) {
	err := SingleNotificationError("User", "email", domain.RecordNotFoundNotification{})
	if err == nil {
		t.Fatal("expected non-nil *InfrastructureError")
	}
	if len(err.Contexts) != 1 {
		t.Fatalf("expected 1 context, got %d", len(err.Contexts))
	}
	ctx := err.Contexts[0]
	if ctx.Context() != "User" {
		t.Errorf("expected context name 'User', got %q", ctx.Context())
	}
	msgs := ctx.Messages()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0].FieldName != "email" {
		t.Errorf("expected FieldName 'email', got %q", msgs[0].FieldName)
	}
	if key := domain.NotificationKey(msgs[0].Notification); key != "RecordNotFoundNotification" {
		t.Errorf("expected NotificationKey 'RecordNotFoundNotification', got %q", key)
	}
}

func TestFieldErrorWithCause(t *testing.T) {
	cause := errors.New("pg: unique violation")
	err := FieldErrorWithCause("User", "email", cause, domain.RecordNotFoundNotification{})
	if err == nil {
		t.Fatal("expected non-nil *InfrastructureError")
	}
	msgs := err.Contexts[0].Messages()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0].Err != cause {
		t.Errorf("expected message.Err to equal cause, got %v", msgs[0].Err)
	}
}

func TestInfrastructureErrorImplementsCarrier(t *testing.T) {
	var _ domain.NotificationCarrier = (*InfrastructureError)(nil)
}

// The uncovered half of the family: the plain constructor, the error string,
// the carrier accessor, and the two canonical Schema envelopes.

func TestInfrastructureError_ErrorAndAccessor(t *testing.T) {
	ctx := domain.NewNotificationContext("User")
	e := NewInfrastructureError([]*domain.NotificationContext{ctx})
	if got := e.Error(); !strings.Contains(got, "1 context(s)") {
		t.Errorf("Error() must count contexts, got %q", got)
	}
	if got := e.NotificationContexts(); len(got) != 1 || got[0] != ctx {
		t.Errorf("NotificationContexts must return the carried slice, got %v", got)
	}
	var carrier domain.NotificationCarrier
	if !errors.As(error(e), &carrier) {
		t.Fatal("errors.As must catch the carrier across layers")
	}
}

func TestLimitExceededError_Shape(t *testing.T) {
	e := LimitExceededError(50)
	if e.Contexts[0].Context() != "Schema" {
		t.Fatalf("envelope context = %q, want Schema", e.Contexts[0].Context())
	}
	m := e.Contexts[0].Messages()[0]
	if m.FieldName != "limit" || m.FieldValue != "50" {
		t.Errorf("the effective ceiling must ride FieldValue, got %+v", m)
	}
	if _, ok := m.Notification.(domain.LimitExceededNotification); !ok {
		t.Errorf("kernel notification drifted: %T", m.Notification)
	}
}

func TestInvalidCursorError_Shape(t *testing.T) {
	cause := errors.New("bad tuple")
	e := InvalidCursorError(cause)
	m := e.Contexts[0].Messages()[0]
	if e.Contexts[0].Context() != "Schema" || m.FieldName != "cursor" || m.Err != cause {
		t.Fatalf("wrong envelope: ctx=%q msg=%+v", e.Contexts[0].Context(), m)
	}
	// Same notificationKey as the REST wrapper's pre-dispatch rejection — both
	// surfaces must report identically.
	if _, ok := m.Notification.(domain.SchemaViolationNotification); !ok {
		t.Errorf("kernel notification drifted: %T", m.Notification)
	}
}
