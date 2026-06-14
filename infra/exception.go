package infra

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

// LimitExceededError packages a per-view "limit too high" rejection in the
// canonical Schema envelope. FieldName is "limit"; FieldValue carries the
// effective ceiling so the consumer surfaces "max is X" without parsing the
// translated message. Wired to SemanticSchema → 400 via the kernel
// LimitExceededNotification.
func LimitExceededError(maxLimit int64) *InfrastructureError {
	return NewInfrastructureErrorWith("Schema", domain.NotificationMessage{
		FieldName:    "limit",
		FieldValue:   fmt.Sprintf("%d", maxLimit),
		Notification: domain.LimitExceededNotification{},
	})
}
