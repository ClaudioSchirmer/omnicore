package graphql

import (
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/notifications"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra"
)

// TestExecute_InfrastructureFailureSurfacesLegibleNotification pins the legible
// error path: when a read handler returns a typed NotificationCarrier error
// (here infra.InvalidCursorError, the keyset-cursor rejection a non-pre-
// validating surface like GraphQL reaches), the resolver must surface it as a
// structured GraphQL error carrying the notification's semantic / key / field —
// NOT the opaque "internal server error" / Internal envelope reserved for plain
// (untyped) Go errors. This is the direct regression guard for a stale/foreign
// cursor showing up as a 500-equivalent.
func TestExecute_InfrastructureFailureSurfacesLegibleNotification(t *testing.T) {
	h := &fakeReadHandler{err: infra.InvalidCursorError(nil)}
	reg, ctx := newExecRegistry(h)

	resp := reg.Execute(ctx, `query { users(first: 1) { edges { node { id } } } }`, nil, "")

	if len(resp.Errors) == 0 {
		t.Fatal("expected a populated errors[]; got none")
	}
	e := resp.Errors[0]
	if e.Message == "" || e.Message == "internal server error" {
		t.Errorf("message = %q, want a legible translated message (not the opaque internal-error string)", e.Message)
	}
	if e.Extensions == nil {
		t.Fatal("expected extensions with semantic/notificationKey/field; got nil")
	}
	if got := e.Extensions["semantic"]; got != "Schema" {
		t.Errorf("extensions.semantic = %v, want Schema", got)
	}
	if got := e.Extensions["notificationKey"]; got != "SchemaViolationNotification" {
		t.Errorf("extensions.notificationKey = %v, want SchemaViolationNotification", got)
	}
	if got := e.Extensions["field"]; got != "cursor" {
		t.Errorf("extensions.field = %v, want cursor", got)
	}
}

// TestFromNotifications_MirrorsRESTErrorMessage pins that the GraphQL error
// extensions carry the SAME message fields the REST ErrorMessage does
// (web/from_notifications.go): the translated human label (fieldLabel, from the
// `labelKey` tag), the echoed value, and funcName — not only
// semantic/notificationKey/field. The data already lives on the shared
// notifications.MessageDTO; this guards the GraphQL mapping reading all of it.
func TestFromNotifications_MirrorsRESTErrorMessage(t *testing.T) {
	in := []notifications.ContextDTO{{
		Context: "User",
		Messages: []notifications.MessageDTO{{
			NotificationKey: "NameTooShortNotification",
			FieldName:       "name",
			FieldLabel:      "Nome",
			FieldValue:      "Al",
			FuncName:        "BuildRules",
			Message:         "Name is too short.",
			Semantic:        domain.SemanticValidation,
		}},
	}}

	out := fromNotifications(in)
	if len(out) != 1 {
		t.Fatalf("len(out) = %d, want 1", len(out))
	}
	ext := out[0].Extensions
	cases := map[string]any{
		"semantic":        domain.SemanticValidation.String(),
		"notificationKey": "NameTooShortNotification",
		"field":           "name",
		"fieldLabel":      "Nome",
		"value":           "Al",
		"funcName":        "BuildRules",
	}
	for key, want := range cases {
		if got := ext[key]; got != want {
			t.Errorf("extensions[%q] = %v, want %v", key, got, want)
		}
	}
	if out[0].Message != "Name is too short." {
		t.Errorf("message = %q, want %q", out[0].Message, "Name is too short.")
	}
}

// TestFromNotifications_OmitsEmptyFields pins the omitempty parity: a message
// with no label/value/funcName must NOT introduce those keys, so services that
// don't use them keep a byte-identical envelope.
func TestFromNotifications_OmitsEmptyFields(t *testing.T) {
	in := []notifications.ContextDTO{{
		Context: "User",
		Messages: []notifications.MessageDTO{{
			NotificationKey: "RequiredFieldNotification",
			FieldName:       "email",
			Message:         "Required field.",
			Semantic:        domain.SemanticValidation,
		}},
	}}

	ext := fromNotifications(in)[0].Extensions
	for _, key := range []string{"fieldLabel", "value", "funcName"} {
		if _, present := ext[key]; present {
			t.Errorf("extensions[%q] present, want absent (omitempty parity)", key)
		}
	}
}
