package infra

import (
	"errors"
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
