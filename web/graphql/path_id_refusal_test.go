package graphql

import (
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/application/results"
	"github.com/ClaudioSchirmer/omnicore/application/translation"
)

// This surface always answers HTTP 200, so the semantic is data: a refusal is
// only legible if it carries the typed triple. A malformed `id` argument used
// to reach the reader and come back as internalError() — `semantic: "Internal"`
// with NO notificationKey at all, indistinguishable from a genuine server
// fault. These pin the typed answer.

func extOf(t *testing.T, errs []GraphQLError) map[string]any {
	t.Helper()
	if len(errs) == 0 {
		t.Fatal("expected an error, got none")
	}
	if errs[0].Extensions == nil {
		t.Fatalf("error carries no extensions: %+v", errs[0])
	}
	return errs[0].Extensions
}

func TestQueryByID_MalformedIDIsTypedNotFound(t *testing.T) {
	h := &fakeByIDHandler{}
	reg, ctx := newByIDRegistry(&fakeReadHandler{}, h)

	resp := reg.Execute(ctx, `{ user(id: "not-a-uuid") { id } }`, nil, "")
	ext := extOf(t, resp.Errors)
	if ext["notificationKey"] != "UnknownIDAddressNotification" {
		t.Errorf("notificationKey = %v, want UnknownIDAddressNotification", ext["notificationKey"])
	}
	if ext["semantic"] != "NotFound" {
		t.Errorf("semantic = %v, want NotFound", ext["semantic"])
	}
	if ext["field"] != "id" {
		t.Errorf("field = %v, want id", ext["field"])
	}
	if ext["value"] != "not-a-uuid" {
		t.Errorf("value = %v, want the rejected argument echoed", ext["value"])
	}
	if h.capturedID != "" {
		t.Errorf("the handler must not run, captured %q", h.capturedID)
	}
}

func TestMutationByID_MalformedIDIsTypedSchema(t *testing.T) {
	pipe := pipeline.New(translation.Default())
	reg := New(pipe).Register(
		MutationByID[delCmd, *delCmd, results.None]("deleteThing", &fakeDelHandler{}),
	)
	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG)

	resp := reg.Execute(ctx, `mutation { deleteThing(id: "not-a-uuid") { success } }`, nil, "")
	ext := extOf(t, resp.Errors)
	if ext["notificationKey"] != "MalformedIDNotification" {
		t.Errorf("notificationKey = %v, want MalformedIDNotification", ext["notificationKey"])
	}
	if ext["semantic"] != "Schema" {
		t.Errorf("semantic = %v, want Schema (a mutation states an intention about a record)", ext["semantic"])
	}
}
