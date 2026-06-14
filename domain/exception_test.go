package domain

import (
	"errors"
	"testing"
)

func TestSingleNotificationError(t *testing.T) {
	err := SingleNotificationError("User", "email", InvalidIDUUIDNotification{})
	if err == nil {
		t.Fatal("expected non-nil *DomainError")
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
	if key := NotificationKey(msgs[0].Notification); key != "InvalidIDUUIDNotification" {
		t.Errorf("expected NotificationKey 'InvalidIDUUIDNotification', got %q", key)
	}
}

func TestNotFoundError(t *testing.T) {
	err := NotFoundError("User", "id", "abc-123")
	if err == nil {
		t.Fatal("expected non-nil *DomainError")
	}
	if len(err.Contexts) != 1 {
		t.Fatalf("expected 1 context, got %d", len(err.Contexts))
	}
	msgs := err.Contexts[0].Messages()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0].FieldName != "id" {
		t.Errorf("expected FieldName 'id', got %q", msgs[0].FieldName)
	}
	if msgs[0].FieldValue != "abc-123" {
		t.Errorf("expected FieldValue 'abc-123', got %q", msgs[0].FieldValue)
	}
	if key := NotificationKey(msgs[0].Notification); key != "RecordNotFoundNotification" {
		t.Errorf("expected NotificationKey 'RecordNotFoundNotification', got %q", key)
	}
}

func TestFieldErrorWithCause(t *testing.T) {
	cause := errors.New("pg: violation")
	err := FieldErrorWithCause("User", "email", cause, InvalidIDUUIDNotification{})
	if err == nil {
		t.Fatal("expected non-nil *DomainError")
	}
	msgs := err.Contexts[0].Messages()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0].Err != cause {
		t.Errorf("expected message.Err to equal cause, got %v", msgs[0].Err)
	}
}

func TestDomainErrorImplementsCarrier(t *testing.T) {
	var _ NotificationCarrier = (*DomainError)(nil)
}
