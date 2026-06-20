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
			dto := MessageDTO{
				NotificationKey: key,
				FieldName:       m.ResolveFieldName(),
				FieldValue:      m.FieldValue,
				FuncName:        m.FuncName,
				Message:         t.Render(lang, key, domain.MessageVars(m)),
				Semantic:        m.Notification.Semantic(),
			}
			// Render the field's human label when the source struct declared
			// a `labelKey:"<catalogKey>"` tag (resolved at emit time by
			// Rules.AddNotification and carried on m.LabelKey). Translator.Render
			// applies its existing fallback-to-key + warn-once-per-(lang, key)
			// posture on catalog miss, identical to how Message is handled
			// one line above. Empty LabelKey skips the render — omitempty
			// then elides FieldLabel from the wire.
			if m.LabelKey != "" {
				dto.FieldLabel = t.Render(lang, m.LabelKey, nil)
			}
			dtoMsgs = append(dtoMsgs, dto)
		}
		out = append(out, ContextDTO{
			Context:  t.Render(lang, ctx.Context(), ctx.ContextVars()),
			Messages: dtoMsgs,
		})
	}
	return out
}
