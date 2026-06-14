package notifications

import (
	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/translation"
	"github.com/ClaudioSchirmer/omnicore/domain"
)

func ToContextDTOs(
	t *translation.Translator,
	lang configuration.Language,
	contexts []*domain.NotificationContext,
) []ContextDTO {
	out := make([]ContextDTO, 0, len(contexts))
	for _, ctx := range contexts {
		if ctx == nil {
			continue
		}
		msgs := ctx.Messages()
		dtoMsgs := make([]MessageDTO, 0, len(msgs))
		for _, m := range msgs {
			key := domain.NotificationKey(m.Notification)
			dtoMsgs = append(dtoMsgs, MessageDTO{
				NotificationKey: key,
				FieldName:       m.ResolveFieldName(),
				FieldValue:      m.FieldValue,
				FuncName:        m.FuncName,
				Message:         t.Render(lang, key, domain.MessageVars(m)),
				Semantic:        m.Notification.Semantic(),
			})
		}
		out = append(out, ContextDTO{
			Context:  t.Render(lang, ctx.Context(), ctx.ContextVars()),
			Messages: dtoMsgs,
		})
	}
	return out
}
