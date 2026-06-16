package web

import (
	"net/http"

	"github.com/ClaudioSchirmer/omnicore/application/notifications"
	"github.com/ClaudioSchirmer/omnicore/domain"
)

// ResponseFromContextDTOs builds a Response from already-translated DTOs
// (typically produced by the Pipeline).
func ResponseFromContextDTOs(dtos []notifications.ContextDTO, status int, description string) Response {
	if description == "" {
		description = http.StatusText(status)
	}
	errors := make([]Error, 0, len(dtos))
	for _, dto := range dtos {
		messages := make([]ErrorMessage, 0, len(dto.Messages))
		for _, m := range dto.Messages {
			messages = append(messages, ErrorMessage{
				NotificationKey: m.NotificationKey,
				Field:           m.FieldName,
				FieldLabel:      m.FieldLabel,
				Value:           m.FieldValue,
				FuncName:        m.FuncName,
				Message:         m.Message,
				Semantic:        m.Semantic.String(),
			})
		}
		errors = append(errors, Error{
			Context:  dto.Context,
			Messages: messages,
		})
	}
	return Response{
		Success:     false,
		Status:      status,
		Description: description,
		Errors:      errors,
	}
}

// ResponseFromContexts builds a Response from raw domain notification contexts
// without translation. The message uses NotificationKey directly — useful when
// no translator is configured or for debugging.
func ResponseFromContexts(contexts []*domain.NotificationContext, status int, description string) Response {
	if description == "" {
		description = http.StatusText(status)
	}
	errors := make([]Error, 0, len(contexts))
	for _, ctx := range contexts {
		if ctx == nil {
			continue
		}
		msgs := ctx.Messages()
		messages := make([]ErrorMessage, 0, len(msgs))
		for _, m := range msgs {
			key := domain.NotificationKey(m.Notification)
			messages = append(messages, ErrorMessage{
				NotificationKey: key,
				Field:           m.ResolveFieldName(),
				FieldLabel:      m.LabelKey, // no-translator path: raw key parallel to Message=key
				Value:           m.FieldValue,
				FuncName:        m.FuncName,
				Message:         key,
				Semantic:        m.Notification.Semantic().String(),
			})
		}
		errors = append(errors, Error{
			Context:  ctx.Context(),
			Messages: messages,
		})
	}
	return Response{
		Success:     false,
		Status:      status,
		Description: description,
		Errors:      errors,
	}
}
