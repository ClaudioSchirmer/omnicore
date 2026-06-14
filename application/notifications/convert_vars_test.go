package notifications

import (
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/translation"
	"github.com/ClaudioSchirmer/omnicore/domain"
)

type maxLengthNotif struct {
	domain.DomainNotificationBase
	MaxLength int `tvar:"maxLength"`
}

type plainNotif struct{ domain.DomainNotificationBase }

func newTestTranslator() *translation.Translator {
	tr := translation.New()
	tr.Import(stubModule{
		lang: configuration.LangENG,
		entries: map[string]string{
			"maxLengthNotif": "Name exceeds {maxLength} characters.",
			"plainNotif":     "Static message.",
			"UserOf{tenantId}": "User of {tenantId}",
		},
	})
	return tr
}

type stubModule struct {
	lang    configuration.Language
	entries map[string]string
}

func (m stubModule) Language() configuration.Language { return m.lang }
func (m stubModule) Translations() map[string]string  { return m.entries }

func TestToContextDTOs_RendersTagDerivedVarsOnMessage(t *testing.T) {
	tr := newTestTranslator()

	ctx := domain.NewNotificationContext("User")
	ctx.AddNotificationMessage(domain.NotificationMessage{
		FieldName:    "name",
		Notification: maxLengthNotif{MaxLength: 100},
	})

	dtos := ToContextDTOs(tr, configuration.LangENG, []*domain.NotificationContext{ctx})
	if len(dtos) != 1 || len(dtos[0].Messages) != 1 {
		t.Fatalf("expected 1×1 DTO shape, got %#v", dtos)
	}
	got := dtos[0].Messages[0].Message
	if got != "Name exceeds 100 characters." {
		t.Errorf("Message = %q, want substitution applied", got)
	}
}

func TestToContextDTOs_PerMessageVarsOverrideNotifVars(t *testing.T) {
	tr := newTestTranslator()

	ctx := domain.NewNotificationContext("User")
	ctx.AddNotificationMessage(domain.NotificationMessage{
		FieldName:    "name",
		Notification: maxLengthNotif{MaxLength: 100},
		Vars:         map[string]string{"maxLength": "OVERRIDE"},
	})

	dtos := ToContextDTOs(tr, configuration.LangENG, []*domain.NotificationContext{ctx})
	got := dtos[0].Messages[0].Message
	if got != "Name exceeds OVERRIDE characters." {
		t.Errorf("per-message Vars must win, got %q", got)
	}
}

func TestToContextDTOs_RendersContextVarsOnLabel(t *testing.T) {
	tr := newTestTranslator()

	ctx := domain.NewNotificationContext("UserOf{tenantId}")
	ctx.SetVars(map[string]string{"tenantId": "acme"})
	ctx.AddNotificationMessage(domain.NotificationMessage{
		FieldName:    "name",
		Notification: plainNotif{},
	})

	dtos := ToContextDTOs(tr, configuration.LangENG, []*domain.NotificationContext{ctx})
	if len(dtos) != 1 {
		t.Fatalf("expected 1 DTO context, got %d", len(dtos))
	}
	if got := dtos[0].Context; got != "User of acme" {
		t.Errorf("Context label = %q, want substitution applied", got)
	}
}

func TestToContextDTOs_NoVarsNoPlaceholders_PreviousBehaviorPreserved(t *testing.T) {
	tr := newTestTranslator()

	ctx := domain.NewNotificationContext("User")
	ctx.AddNotificationMessage(domain.NotificationMessage{
		FieldName:    "name",
		Notification: plainNotif{},
	})

	// Reset warn-once so a stale state from sibling tests does not mask the
	// fresh signal we'd want to inspect here. Not asserting silence; we
	// just want to be deterministic.
	translation.ResetWarnOnceForTesting()

	dtos := ToContextDTOs(tr, configuration.LangENG, []*domain.NotificationContext{ctx})
	if got := dtos[0].Messages[0].Message; got != "Static message." {
		t.Errorf("Message without vars = %q, want %q", got, "Static message.")
	}
	if got := dtos[0].Context; got != "User" {
		t.Errorf("Context label without vars and without catalog entry should fall back to key, got %q", got)
	}
}
