package notifications

import (
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/translation"
	"github.com/ClaudioSchirmer/omnicore/domain"
)

// labelCatalogModule wires the catalog entries the FieldLabel render needs
// for each test case. Implements translation.Module by composing a fresh
// map per language under test; the convert layer pulls translations via the
// Translator constructed with this module.
type labelCatalogModule struct {
	lang  configuration.Language
	table map[string]string
}

func (m labelCatalogModule) Language() configuration.Language { return m.lang }
func (m labelCatalogModule) Translations() map[string]string  { return m.table }

func newLabelTranslator(modules ...translation.Module) *translation.Translator {
	t := translation.New()
	t.Import(modules...)
	return t
}

func TestToContextDTOs_PopulatesFieldLabelWhenLabelKeyPresentAndCatalogHits(t *testing.T) {
	ctx := domain.NewNotificationContext("User")
	ctx.AddNotificationMessage(domain.NotificationMessage{
		Path:         []domain.PathSegment{{Name: "Name"}},
		LabelKey:     "UserNameField",
		Notification: domain.RequiredFieldNotification{},
	})

	tr := newLabelTranslator(labelCatalogModule{
		lang: configuration.LangPTBR,
		table: map[string]string{
			"UserNameField": "Nome",
			// keep RequiredFieldNotification rendered via the framework default
			// (we want the test to focus on FieldLabel; Message can stay key)
		},
	})

	dtos := ToContextDTOs(tr, configuration.LangPTBR, []*domain.NotificationContext{ctx})
	if len(dtos) != 1 || len(dtos[0].Messages) != 1 {
		t.Fatalf("envelope shape mismatch: %+v", dtos)
	}
	if got := dtos[0].Messages[0].FieldLabel; got != "Nome" {
		t.Errorf("FieldLabel = %q, want Nome", got)
	}
}

func TestToContextDTOs_FieldLabelEmptyWhenLabelKeyAbsent(t *testing.T) {
	ctx := domain.NewNotificationContext("User")
	ctx.AddNotificationMessage(domain.NotificationMessage{
		Path:         []domain.PathSegment{{Name: "Name"}},
		Notification: domain.RequiredFieldNotification{},
		// LabelKey deliberately left empty
	})

	tr := newLabelTranslator(labelCatalogModule{
		lang: configuration.LangPTBR,
		table: map[string]string{
			"UserNameField": "Nome", // present in catalog but irrelevant — message did not carry the key
		},
	})

	dtos := ToContextDTOs(tr, configuration.LangPTBR, []*domain.NotificationContext{ctx})
	if got := dtos[0].Messages[0].FieldLabel; got != "" {
		t.Errorf("FieldLabel = %q, want empty (no LabelKey on message)", got)
	}
}

func TestToContextDTOs_FieldLabelFallsBackToKeyOnCatalogMiss(t *testing.T) {
	ctx := domain.NewNotificationContext("User")
	ctx.AddNotificationMessage(domain.NotificationMessage{
		Path:         []domain.PathSegment{{Name: "Name"}},
		LabelKey:     "UnknownLabelKey",
		Notification: domain.RequiredFieldNotification{},
	})

	tr := newLabelTranslator(labelCatalogModule{
		lang:  configuration.LangPTBR,
		table: map[string]string{}, // empty catalog — every key misses
	})

	dtos := ToContextDTOs(tr, configuration.LangPTBR, []*domain.NotificationContext{ctx})
	if got := dtos[0].Messages[0].FieldLabel; got != "UnknownLabelKey" {
		t.Errorf("FieldLabel on miss = %q, want UnknownLabelKey (raw key fallback)", got)
	}
}

func TestToContextDTOs_FieldLabelHonorsActorLocale(t *testing.T) {
	ctx := domain.NewNotificationContext("User")
	ctx.AddNotificationMessage(domain.NotificationMessage{
		Path:         []domain.PathSegment{{Name: "Email"}},
		LabelKey:     "UserEmailField",
		Notification: domain.RequiredFieldNotification{},
	})

	tr := newLabelTranslator(
		labelCatalogModule{
			lang:  configuration.LangPTBR,
			table: map[string]string{"UserEmailField": "E-mail (PT)"},
		},
		labelCatalogModule{
			lang:  configuration.LangENG,
			table: map[string]string{"UserEmailField": "Email"},
		},
	)

	dtosPT := ToContextDTOs(tr, configuration.LangPTBR, []*domain.NotificationContext{ctx})
	if got := dtosPT[0].Messages[0].FieldLabel; got != "E-mail (PT)" {
		t.Errorf("PT FieldLabel = %q, want E-mail (PT)", got)
	}

	dtosEN := ToContextDTOs(tr, configuration.LangENG, []*domain.NotificationContext{ctx})
	if got := dtosEN[0].Messages[0].FieldLabel; got != "Email" {
		t.Errorf("EN FieldLabel = %q, want Email", got)
	}
}

func TestToContextDTOs_OtherFieldsUntouchedByLabelAddition(t *testing.T) {
	// Regression — ensure adding FieldLabel did not drop / corrupt the existing
	// fields the wire envelope relies on.
	ctx := domain.NewNotificationContext("User")
	ctx.AddNotificationMessage(domain.NotificationMessage{
		Path:         []domain.PathSegment{{Name: "Name"}},
		FieldValue:   "the-input",
		FuncName:     "Validate",
		LabelKey:     "UserNameField",
		Notification: domain.RequiredFieldNotification{},
	})

	tr := newLabelTranslator(labelCatalogModule{
		lang:  configuration.LangPTBR,
		table: map[string]string{"UserNameField": "Nome"},
	})

	dto := ToContextDTOs(tr, configuration.LangPTBR, []*domain.NotificationContext{ctx})[0].Messages[0]
	if dto.NotificationKey != "RequiredFieldNotification" {
		t.Errorf("NotificationKey = %q", dto.NotificationKey)
	}
	if dto.FieldName != "name" {
		t.Errorf("FieldName = %q, want name (camelCase of Path)", dto.FieldName)
	}
	if dto.FieldValue != "the-input" {
		t.Errorf("FieldValue = %q", dto.FieldValue)
	}
	if dto.FuncName != "Validate" {
		t.Errorf("FuncName = %q", dto.FuncName)
	}
	if dto.FieldLabel != "Nome" {
		t.Errorf("FieldLabel = %q", dto.FieldLabel)
	}
}
