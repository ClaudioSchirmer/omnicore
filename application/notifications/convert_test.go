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
