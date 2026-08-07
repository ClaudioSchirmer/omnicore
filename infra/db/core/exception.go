package core

import (
	"fmt"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

type InfrastructureError struct {
	Contexts []*domain.NotificationContext
}

func NewInfrastructureError(contexts []*domain.NotificationContext) *InfrastructureError {
	return &InfrastructureError{Contexts: contexts}
}

func NewInfrastructureErrorWith(contextName string, msg domain.NotificationMessage) *InfrastructureError {
	ctx := domain.NewNotificationContext(contextName)
	ctx.AddNotificationMessage(msg)
	return &InfrastructureError{Contexts: []*domain.NotificationContext{ctx}}
}

func (e *InfrastructureError) Error() string {
	return fmt.Sprintf("infrastructure error: %d context(s)", len(e.Contexts))
}

func (e *InfrastructureError) NotificationContexts() []*domain.NotificationContext {
	return e.Contexts
}

// SingleNotificationError packages a single notification into *InfrastructureError.
func SingleNotificationError(contextName, fieldName string, n domain.Notification) *InfrastructureError {
	return NewInfrastructureErrorWith(contextName, domain.NotificationMessage{
		FieldName: fieldName, Notification: n,
	})
}

// FieldErrorWithCause includes a raw error as cause (typical for constraint catch).
func FieldErrorWithCause(contextName, fieldName string, cause error, n domain.Notification) *InfrastructureError {
	return NewInfrastructureErrorWith(contextName, domain.NotificationMessage{
		FieldName: fieldName, Err: cause, Notification: n,
	})
}

// LimitExceededError packages a per-view "page size too high" rejection in the
// canonical Schema envelope. FieldName names the directional control that
// carried the size — "first" forward, "last" backward — so the 400 points at
// the exact wire key the consumer sent; FieldValue carries the effective
// ceiling so the consumer surfaces "max is X" without parsing the translated
// message. Wired to SemanticSchema → 400 via the kernel
// LimitExceededNotification.
func LimitExceededError(maxLimit int64, backward bool) *InfrastructureError {
	field := "first"
	if backward {
		field = "last"
	}
	return NewInfrastructureErrorWith("Schema", domain.NotificationMessage{
		FieldName:    field,
		FieldValue:   fmt.Sprintf("%d", maxLimit),
		Notification: domain.LimitExceededNotification{},
	})
}

// InvalidCursorError packages a keyset-cursor rejection (undecodable, tuple-
// length mismatch, or context-hash mismatch) in the canonical Schema envelope.
// FieldName is "cursor"; the cause is carried for diagnostics. Wired to
// SemanticSchema → 400-equivalent via the kernel SchemaViolationNotification —
// the SAME notification the REST wrapper's pre-dispatch check emits, so both
// surfaces report an identical notificationKey. The reader reaches this only
// on surfaces that do not pre-validate the cursor (the REST wrapper rejects it
// before dispatch); without it, a stale/foreign cursor would surface as a
// plain error → 500/Internal instead of a legible Schema rejection.
func InvalidCursorError(cause error) *InfrastructureError {
	return NewInfrastructureErrorWith("Schema", domain.NotificationMessage{
		FieldName:    "cursor",
		Err:          cause,
		Notification: domain.SchemaViolationNotification{},
	})
}
