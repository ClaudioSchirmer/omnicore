package domain

import "fmt"

type DomainError struct {
	Contexts []*NotificationContext
}

func NewDomainError(contexts []*NotificationContext) *DomainError {
	return &DomainError{Contexts: contexts}
}

func (e *DomainError) Error() string {
	if len(e.Contexts) == 0 {
		return "domain validation error"
	}
	return fmt.Sprintf("domain validation error: %d context(s) with errors", len(e.Contexts))
}

func (e *DomainError) NotificationContexts() []*NotificationContext {
	return e.Contexts
}

func (e *DomainError) HasErrors() bool {
	for _, c := range e.Contexts {
		if c.HasErrors() {
			return true
		}
	}
	return false
}

// NewDomainErrorWith is the 1-context+1-message form of the constructor.
func NewDomainErrorWith(contextName string, msg NotificationMessage) *DomainError {
	ctx := NewNotificationContext(contextName)
	ctx.AddNotificationMessage(msg)
	return &DomainError{Contexts: []*NotificationContext{ctx}}
}

// SingleNotificationError wraps a single notification in *DomainError.
func SingleNotificationError(contextName, fieldName string, n Notification) *DomainError {
	return NewDomainErrorWith(contextName, NotificationMessage{
		FieldName: fieldName, Notification: n,
	})
}

// NotFoundError is the canonical helper for record not found, using RecordNotFoundNotification.
func NotFoundError(contextName, fieldName, fieldValue string) *DomainError {
	return NewDomainErrorWith(contextName, NotificationMessage{
		FieldName: fieldName, FieldValue: fieldValue,
		Notification: RecordNotFoundNotification{},
	})
}

// FieldErrorWithCause includes a raw error as the cause.
func FieldErrorWithCause(contextName, fieldName string, cause error, n Notification) *DomainError {
	return NewDomainErrorWith(contextName, NotificationMessage{
		FieldName: fieldName, Err: cause, Notification: n,
	})
}
