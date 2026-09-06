package exception

import (
	"errors"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

func TestSingleNotificationError(t *testing.T) {
	err := SingleNotificationError("User", "email", domain.RecordNotFoundNotification{})
	if err == nil {
		t.Fatal("expected non-nil *ApplicationError")
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
	if msgs[0].ResolveFieldName() != "email" {
		t.Errorf("expected FieldName 'email', got %q", msgs[0].ResolveFieldName())
	}
	if key := domain.NotificationKey(msgs[0].Notification); key != "RecordNotFoundNotification" {
		t.Errorf("expected NotificationKey 'RecordNotFoundNotification', got %q", key)
	}
}

func TestFieldErrorWithCause(t *testing.T) {
	cause := errors.New("downstream timeout")
	err := FieldErrorWithCause("User", "email", cause, domain.RecordNotFoundNotification{})
	if err == nil {
		t.Fatal("expected non-nil *ApplicationError")
	}
	msgs := err.Contexts[0].Messages()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0].Err != cause {
		t.Errorf("expected message.Err to equal cause, got %v", msgs[0].Err)
	}
}

func TestApplicationErrorImplementsCarrier(t *testing.T) {
	var _ domain.NotificationCarrier = (*ApplicationError)(nil)
}

func TestNewApplicationError(t *testing.T) {
	ctxA := domain.NewNotificationContext("A")
	ctxB := domain.NewNotificationContext("B")
	err := NewApplicationError([]*domain.NotificationContext{ctxA, ctxB})
	if err == nil {
		t.Fatal("expected non-nil *ApplicationError")
	}
	if got := len(err.Contexts); got != 2 {
		t.Fatalf("expected 2 contexts, got %d", got)
	}
	if err.Contexts[0] != ctxA || err.Contexts[1] != ctxB {
		t.Errorf("expected NewApplicationError to preserve the input slice verbatim")
	}
}

func TestNewApplicationErrorWithNilSlice(t *testing.T) {
	err := NewApplicationError(nil)
	if err == nil {
		t.Fatal("expected non-nil *ApplicationError even when contexts is nil")
	}
	if err.Contexts != nil {
		t.Errorf("expected Contexts to be nil, got %#v", err.Contexts)
	}
}

func TestNewApplicationErrorWith(t *testing.T) {
	msg := domain.NotificationMessage{
		Override:     "email",
		Notification: domain.RecordNotFoundNotification{},
	}
	err := NewApplicationErrorWith("User", msg)
	if err == nil {
		t.Fatal("expected non-nil *ApplicationError")
	}
	if got := len(err.Contexts); got != 1 {
		t.Fatalf("expected 1 context, got %d", got)
	}
	if name := err.Contexts[0].Context(); name != "User" {
		t.Errorf("expected context name 'User', got %q", name)
	}
	msgs := err.Contexts[0].Messages()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0].ResolveFieldName() != "email" {
		t.Errorf("expected FieldName 'email', got %q", msgs[0].ResolveFieldName())
	}
	if key := domain.NotificationKey(msgs[0].Notification); key != "RecordNotFoundNotification" {
		t.Errorf("expected NotificationKey 'RecordNotFoundNotification', got %q", key)
	}
}

func TestApplicationErrorErrorMessage(t *testing.T) {
	tests := []struct {
		name     string
		err      *ApplicationError
		expected string
	}{
		{
			name:     "no contexts",
			err:      NewApplicationError(nil),
			expected: "application error: 0 context(s)",
		},
		{
			name:     "single context",
			err:      NewApplicationError([]*domain.NotificationContext{domain.NewNotificationContext("A")}),
			expected: "application error: 1 context(s)",
		},
		{
			name: "three contexts",
			err: NewApplicationError([]*domain.NotificationContext{
				domain.NewNotificationContext("A"),
				domain.NewNotificationContext("B"),
				domain.NewNotificationContext("C"),
			}),
			expected: "application error: 3 context(s)",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.Error(); got != tc.expected {
				t.Errorf("Error()=%q, want %q", got, tc.expected)
			}
		})
	}
}

func TestApplicationErrorNotificationContexts(t *testing.T) {
	ctxA := domain.NewNotificationContext("A")
	ctxB := domain.NewNotificationContext("B")
	err := NewApplicationError([]*domain.NotificationContext{ctxA, ctxB})

	got := err.NotificationContexts()
	if len(got) != 2 {
		t.Fatalf("expected 2 contexts, got %d", len(got))
	}
	if got[0] != ctxA || got[1] != ctxB {
		t.Errorf("expected NotificationContexts to return the same pointers as the source slice")
	}
}

func TestApplicationErrorErrorsAsViaCarrier(t *testing.T) {
	src := NewApplicationErrorWith("User", domain.NotificationMessage{
		Override:     "email",
		Notification: domain.RecordNotFoundNotification{},
	})

	var carrier domain.NotificationCarrier
	if !errors.As(error(src), &carrier) {
		t.Fatalf("expected *ApplicationError to match domain.NotificationCarrier via errors.As")
	}
	if got := len(carrier.NotificationContexts()); got != 1 {
		t.Errorf("expected 1 context via carrier, got %d", got)
	}
}
