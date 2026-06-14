package exception

import (
	"fmt"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

type ApplicationError struct {
	Contexts []*domain.NotificationContext
}

func NewApplicationError(contexts []*domain.NotificationContext) *ApplicationError {
	return &ApplicationError{Contexts: contexts}
}

func NewApplicationErrorWith(contextName string, msg domain.NotificationMessage) *ApplicationError {
	ctx := domain.NewNotificationContext(contextName)
	ctx.AddNotificationMessage(msg)
	return &ApplicationError{Contexts: []*domain.NotificationContext{ctx}}
}

func (e *ApplicationError) Error() string {
	return fmt.Sprintf("application error: %d context(s)", len(e.Contexts))
}

func (e *ApplicationError) NotificationContexts() []*domain.NotificationContext {
	return e.Contexts
}
