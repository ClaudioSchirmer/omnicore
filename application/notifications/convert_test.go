package notifications

import (
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/translation"
	"github.com/ClaudioSchirmer/omnicore/domain"
)

type usernameDuplicateNotification struct{ domain.DomainNotificationBase }

func (usernameDuplicateNotification) Semantic() domain.NotificationSemantic {
	return domain.SemanticConflict
}

func TestSemanticPropagatesToDTO(t *testing.T) {
	ctx := domain.NewNotificationContext("User")
	ctx.AddNotificationMessage(domain.NotificationMessage{
		FieldName:    "username",
		Notification: usernameDuplicateNotification{},
	})
	ctx.AddNotificationMessage(domain.NotificationMessage{
		FieldName:    "name",
		Notification: domain.RequiredFieldNotification{},
	})

	dtos := ToContextDTOs(translation.Default(), configuration.LangPTBR, []*domain.NotificationContext{ctx})

	if len(dtos) != 1 {
		t.Fatalf("expected 1 DTO context, got %d", len(dtos))
	}
	msgs := dtos[0].Messages
	if len(msgs) != 2 {
		t.Fatalf("expected 2 DTO messages, got %d", len(msgs))
	}
	if msgs[0].Semantic != domain.SemanticConflict {
		t.Errorf("expected first message Semantic=Conflict, got %v", msgs[0].Semantic)
	}
	if msgs[1].Semantic != domain.SemanticValidation {
		t.Errorf("expected second message Semantic=Validation, got %v", msgs[1].Semantic)
	}
}

func TestRecordNotFoundPropagatesSemantic(t *testing.T) {
	ctx := domain.NewNotificationContext("User")
	ctx.AddNotificationMessage(domain.NotificationMessage{
		FieldName:    "id",
		FieldValue:   "abc",
		Notification: domain.RecordNotFoundNotification{},
	})
	dtos := ToContextDTOs(translation.Default(), configuration.LangPTBR, []*domain.NotificationContext{ctx})
	if len(dtos) != 1 || len(dtos[0].Messages) != 1 {
		t.Fatalf("expected 1×1 DTO shape, got %#v", dtos)
	}
	if got := dtos[0].Messages[0].Semantic; got != domain.SemanticNotFound {
		t.Fatalf("expected SemanticNotFound, got %v", got)
	}
}

func TestToContextDTOs_SkipsNilContexts(t *testing.T) {
	ctx := domain.NewNotificationContext("User")
	ctx.AddNotificationMessage(domain.NotificationMessage{
		FieldName:    "name",
		Notification: domain.RequiredFieldNotification{},
	})
	// A nil entry interleaved with a real context must be skipped, not panic.
	dtos := ToContextDTOs(translation.Default(), configuration.LangPTBR,
		[]*domain.NotificationContext{nil, ctx, nil})
	if len(dtos) != 1 {
		t.Fatalf("expected nil contexts skipped → 1 DTO, got %d", len(dtos))
	}
	if dtos[0].Context == "" || len(dtos[0].Messages) != 1 {
		t.Fatalf("expected the non-nil context rendered, got %#v", dtos[0])
	}
}
