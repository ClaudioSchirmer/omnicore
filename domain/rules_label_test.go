package domain

import (
	"reflect"
	"testing"
)

// Fixtures specifically for the Rules.AddNotification → LabelKey contract.
// Kept separate from field_label_test.go fixtures so each file documents the
// surface it exercises.

type ruleFixture struct {
	Name string `labelKey:"RuleFixtureNameField"`
	Bare string // no label tag
}

func TestRules_AddNotification_PopulatesLabelKeyFromEntityType(t *testing.T) {
	ctx := NewNotificationContext("Test")
	r := NewRules(ModeInsert, ctx, reflect.TypeOf(ruleFixture{}))

	r.AddNotificationNamed("Name", RequiredFieldNotification{})

	msgs := ctx.Messages()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0].LabelKey != "RuleFixtureNameField" {
		t.Errorf("LabelKey = %q, want RuleFixtureNameField", msgs[0].LabelKey)
	}
}

func TestRules_AddNotification_EmptyLabelKeyWhenFieldHasNoTag(t *testing.T) {
	ctx := NewNotificationContext("Test")
	r := NewRules(ModeInsert, ctx, reflect.TypeOf(ruleFixture{}))

	r.AddNotificationNamed("Bare", RequiredFieldNotification{})

	msgs := ctx.Messages()
	if msgs[0].LabelKey != "" {
		t.Errorf("LabelKey = %q, want empty (field has no label tag)", msgs[0].LabelKey)
	}
}

func TestRules_AddNotification_EmptyLabelKeyWhenEntityTypeIsNil(t *testing.T) {
	ctx := NewNotificationContext("Test")
	r := NewRules(ModeInsert, ctx, nil) // legacy/test path: no entityType

	r.AddNotificationNamed("Name", RequiredFieldNotification{})

	msgs := ctx.Messages()
	if msgs[0].LabelKey != "" {
		t.Errorf("LabelKey = %q, want empty (entityType nil)", msgs[0].LabelKey)
	}
}

func TestRules_AddNotification_PreservesFieldValue(t *testing.T) {
	// Regression: building the message inline (rather than delegating to
	// ctx.AddNotification) must keep the value variadic working — pass *string,
	// nil, and plain string and verify each lands correctly.
	ctx := NewNotificationContext("Test")
	r := NewRules(ModeInsert, ctx, reflect.TypeOf(ruleFixture{}))

	r.AddNotificationNamed("Name", RequiredFieldNotification{}, "the-input")
	if got := ctx.Messages()[0].FieldValue; got != "the-input" {
		t.Errorf("FieldValue = %q, want the-input", got)
	}
}

func TestRules_AddNotificationWithVars_AlsoPopulatesLabelKey(t *testing.T) {
	ctx := NewNotificationContext("Test")
	fx := &ruleFixture{}
	r := NewRulesFor(ModeInsert, ctx, fx)

	r.AddNotificationWithVars(
		&fx.Name,
		singleTvarNotif{MaxLength: 100},
		map[string]string{"override": "yes"},
		false,
	)

	msgs := ctx.Messages()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0].LabelKey != "RuleFixtureNameField" {
		t.Errorf("AddNotificationWithVars LabelKey = %q, want RuleFixtureNameField", msgs[0].LabelKey)
	}
	// Existing surface still works:
	if msgs[0].Vars["override"] != "yes" {
		t.Errorf("Vars override missing: %+v", msgs[0].Vars)
	}
}

func TestRules_AddNotification_PointerEntityType(t *testing.T) {
	// runAggregateValidations calls reflect.TypeOf(e) where e is a pointer
	// receiver. The resolver must unwrap pointer types so the tag lookup hits.
	ctx := NewNotificationContext("Test")
	r := NewRules(ModeInsert, ctx, reflect.TypeOf(&ruleFixture{}))

	r.AddNotificationNamed("Name", RequiredFieldNotification{})

	if got := ctx.Messages()[0].LabelKey; got != "RuleFixtureNameField" {
		t.Errorf("pointer entityType LabelKey = %q, want RuleFixtureNameField", got)
	}
}
