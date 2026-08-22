package hydrate

import (
	"context"
	"errors"
	"testing"
)

// Every read this package issues can fail, and none of the failures may be
// swallowed into an empty result — an empty aggregate and an unreachable
// database must not look alike to the caller.
func TestReads_SurfaceTheEngineError(t *testing.T) {
	boom := errors.New("connection reset")
	h := func() *Hydrator {
		e := newScripted(nil)
		e.err = boom
		return New(e)
	}
	ctx := context.Background()

	if _, err := h().FetchWhere(ctx, lineSchema(), "lines", "order_id", "o1", "", true); err == nil {
		t.Error("FetchWhere must surface the read error")
	}
	if _, err := h().FetchAll(ctx, flatSchema(), "notes", "", true); err == nil {
		t.Error("FetchAll must surface the read error")
	}
	if _, err := h().FetchByIDs(ctx, flatSchema(), "notes", "id", []string{"n1"}, "", true); err == nil {
		t.Error("FetchByIDs must surface the read error")
	}
	if _, err := h().FetchInGrouped(ctx, lineSchema(), "lines", "order_id", []string{"o1"}, "", true); err == nil {
		t.Error("FetchInGrouped must surface the read error")
	}
	if _, err := h().FetchLatestArchived(ctx, roleSchema(), "person_id", "p1", "deleted_at"); err == nil {
		t.Error("FetchLatestArchived must surface the read error")
	}
	if err := h().MergeOwnChildren(ctx, Document{"id": "o1"}, rootSchema(), true); err == nil {
		t.Error("MergeOwnChildren must surface the read error")
	}
	if err := h().MergeSharedBase(ctx, Document{"id": "s1", "person_id": "p1"}, roleSchema(), true); err == nil {
		t.Error("MergeSharedBase must surface the read error")
	}
	if err := h().MergeSharedBaseChildren(ctx, Document{"id": "s1", "person_id": "p1"}, roleSchema(), true); err == nil {
		t.Error("MergeSharedBaseChildren must surface the read error")
	}
}

// A shared base whose ROLE declares none of the managed columns has nothing to
// shadow: the base's own land on the document. The skip is about resolving a
// COLLISION, not about hiding the base.
func TestMergeSharedBase_WithoutRoleManagedColumnsTheBasesLand(t *testing.T) {
	h := New(newScripted(map[string][]map[string]any{
		"persons": {{
			"id": "p1", "name": "ana",
			"deleted_at": "BASE-ARCHIVED", "created_at": "BASE-TIME",
		}},
	}))
	doc := Document{"id": "i1", "person_id": "p1"}
	if err := h.MergeSharedBase(context.Background(), doc, bareRoleSchema(), true); err != nil {
		t.Fatalf("MergeSharedBase: %v", err)
	}
	if doc["deleted_at"] != "BASE-ARCHIVED" || doc["created_at"] != "BASE-TIME" {
		t.Fatalf("with nothing to shadow, the base's managed columns must land: %v", doc)
	}
}

// A base row that simply is not there leaves the role document untouched.
func TestMergeSharedBase_AbsentBaseRowChangesNothing(t *testing.T) {
	h := New(newScripted(nil))
	doc := Document{"id": "s1", "person_id": "p1"}
	if err := h.MergeSharedBase(context.Background(), doc, roleSchema(), true); err != nil {
		t.Fatalf("MergeSharedBase: %v", err)
	}
	if len(doc) != 2 {
		t.Fatalf("an absent base must add nothing, got %v", doc)
	}
}

// The base link column present but NULL is the same as absent: nothing to join.
func TestMergeSharedBaseChildren_NullLinkIssuesNoRead(t *testing.T) {
	eng := newScripted(nil)
	doc := Document{"id": "s1", "person_id": nil}
	if err := New(eng).MergeSharedBaseChildren(context.Background(), doc, roleSchema(), true); err != nil {
		t.Fatalf("MergeSharedBaseChildren: %v", err)
	}
	if len(eng.sqls) != 0 {
		t.Errorf("a NULL link must issue no read, got %d", len(eng.sqls))
	}
}
