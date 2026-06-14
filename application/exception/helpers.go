package exception

import "github.com/ClaudioSchirmer/omnicore/domain"

// SingleNotificationError wraps a single notification in *ApplicationError.
func SingleNotificationError(contextName, fieldName string, n domain.Notification) *ApplicationError {
	ctx := domain.NewNotificationContext(contextName)
	ctx.AddNotificationMessage(domain.NotificationMessage{FieldName: fieldName, Notification: n})
	return &ApplicationError{Contexts: []*domain.NotificationContext{ctx}}
}

// FieldErrorWithCause includes a raw error as the cause of the notification.
func FieldErrorWithCause(contextName, fieldName string, cause error, n domain.Notification) *ApplicationError {
	ctx := domain.NewNotificationContext(contextName)
	ctx.AddNotificationMessage(domain.NotificationMessage{FieldName: fieldName, Err: cause, Notification: n})
	return &ApplicationError{Contexts: []*domain.NotificationContext{ctx}}
}
