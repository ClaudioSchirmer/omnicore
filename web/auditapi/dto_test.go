package auditapi

import (
	"reflect"
	"testing"
	"time"

	"github.com/ClaudioSchirmer/omnicore/application/audit"
)

// The DTO declares the window control and parseFirst reads it; the wire key is
// spelled in both places, and Go tags cannot reference a constant. This test is
// what keeps them from drifting — a rename on one side without the other turns
// the documented parameter into one the handler silently ignores.
func TestRequestDTO_WindowControlMatchesTheReaderKey(t *testing.T) {
	f, ok := reflect.TypeOf(FindAuditByAggregateRequest{}).FieldByName("First")
	if !ok {
		t.Fatal("the Request DTO must declare the window control")
	}
	if got := f.Tag.Get("query"); got != firstControl {
		t.Errorf("DTO query tag = %q, but the handler reads %q", got, firstControl)
	}
	if f.Type.Kind() != reflect.Pointer {
		t.Error("the window control must be pointer-typed so the spec renders it optional")
	}
}

// The path segments the route declares are the ones the DTO binds.
func TestRequestDTO_BindsBothPathSegments(t *testing.T) {
	tags := map[string]string{}
	rt := reflect.TypeOf(FindAuditByAggregateRequest{})
	for i := 0; i < rt.NumField(); i++ {
		if p := rt.Field(i).Tag.Get("path"); p != "" {
			tags[p] = rt.Field(i).Name
		}
	}
	for _, want := range []string{"entityType", "aggregateId"} {
		if _, ok := tags[want]; !ok {
			t.Errorf("the DTO must bind the %q path segment, got %v", want, tags)
		}
	}
}

// ─── projection ──────────────────────────────────────────────────────────────

func TestFromEvent_CarriesEveryScalarAndBothBlocks(t *testing.T) {
	when := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	ev := &audit.AuditEvent{
		ThreadID:    "thread-1",
		TraceID:     "4bf92f3577b34da6a3ce929d0e0e4736",
		EntityType:  "User",
		EntityID:    "agg-1",
		Verb:        "update",
		ActionName:  "GetUpdatable",
		Kind:        "delta",
		Actor:       "user-42",
		ActorIssuer: "https://idp.example",
		ActorClaims: map[string]any{"tenant_id": "acme"},
		TenantID:    "acme",
		DateTime:    when,
		Snapshot:    map[string]any{"name": "alice"},
		Changes:     []audit.FieldChange{{Field: "Name", FieldLabel: "Name", From: "a", To: "b"}},
		Children: map[string][]audit.ChildEvent{
			"Address": {{ID: "addr-1", Op: "updated", Changes: []audit.FieldChange{{Field: "ZipCode"}}}},
		},
	}

	got := fromEvent(ev)

	if got.ThreadID != ev.ThreadID || got.TraceID != ev.TraceID || got.EntityID != ev.EntityID {
		t.Errorf("identity fields drifted: %+v", got)
	}
	if got.Verb != "update" || got.ActionName != "GetUpdatable" || got.Kind != "delta" {
		t.Errorf("operation fields drifted: %+v", got)
	}
	if got.Actor != "user-42" || got.ActorIssuer != "https://idp.example" || got.TenantID != "acme" {
		t.Errorf("actor fields drifted: %+v", got)
	}
	if !got.DateTime.Equal(when) {
		t.Errorf("DateTime = %v, want %v", got.DateTime, when)
	}
	if got.ActorClaims["tenant_id"] != "acme" || got.Snapshot["name"] != "alice" {
		t.Errorf("free-form blocks drifted: claims=%v snapshot=%v", got.ActorClaims, got.Snapshot)
	}
	if len(got.Changes) != 1 || got.Changes[0].Field != "Name" || got.Changes[0].FieldLabel != "Name" {
		t.Errorf("changes drifted: %+v", got.Changes)
	}
	children := got.Children["Address"]
	if len(children) != 1 || children[0].ID != "addr-1" || children[0].Op != "updated" {
		t.Fatalf("children drifted: %+v", got.Children)
	}
	if len(children[0].Changes) != 1 || children[0].Changes[0].Field != "ZipCode" {
		t.Errorf("child changes drifted: %+v", children[0].Changes)
	}
}

// Both label slots travel, because which one is populated is the caller's
// choice (audit.endpoint.renderLabels), not this mapper's.
func TestFromEvent_CarriesWhicheverLabelSlotIsPopulated(t *testing.T) {
	rendered := fromEvent(&audit.AuditEvent{
		Changes: []audit.FieldChange{{Field: "Name", FieldLabel: "Nome"}},
	})
	if rendered.Changes[0].FieldLabel != "Nome" || rendered.Changes[0].FieldLabelKey != "" {
		t.Errorf("rendered label lost: %+v", rendered.Changes[0])
	}
	raw := fromEvent(&audit.AuditEvent{
		Changes: []audit.FieldChange{{Field: "Name", FieldLabelKey: "UserNameField"}},
	})
	if raw.Changes[0].FieldLabelKey != "UserNameField" || raw.Changes[0].FieldLabel != "" {
		t.Errorf("raw key lost: %+v", raw.Changes[0])
	}
}

// An event with no diffs must elide the key rather than emit an empty array —
// the shape omitempty promises.
func TestFromEvent_EmptyBlocksStayNil(t *testing.T) {
	got := fromEvent(&audit.AuditEvent{EntityType: "User", Kind: "transition"})
	if got.Changes != nil {
		t.Errorf("Changes must stay nil on a transition event, got %v", got.Changes)
	}
	if got.Children != nil {
		t.Errorf("Children must stay nil when the event cascaded nothing, got %v", got.Children)
	}
}

func TestFromEvent_NilEventIsTheZeroResponse(t *testing.T) {
	if got := fromEvent(nil); got.EntityType != "" || got.Changes != nil {
		t.Errorf("a nil event must project to the zero response, got %+v", got)
	}
}
