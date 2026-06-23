package graphql

import (
	"testing"

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
