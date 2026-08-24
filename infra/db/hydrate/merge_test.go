package hydrate

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// ─── siblings ────────────────────────────────────────────────────────────────

// A sibling partitions the owner's row, so its columns land FLAT on the owner —
// never nested — and the shared ID is not re-copied.
func TestMergeOwnerSiblings_MergesFlatAndSkipsTheSharedID(t *testing.T) {
	h := New(newScripted(map[string][]map[string]any{
		"order_channels": {{"id": "SHOULD-NOT-OVERWRITE", "channel": "web"}},
	}))
	doc := Document{"id": "o1", "name": "first"}
	if err := h.MergeOwnerSiblings(context.Background(), doc, rootSchema(), "o1", false); err != nil {
		t.Fatalf("MergeOwnerSiblings: %v", err)
	}
	if doc["channel"] != "web" {
		t.Errorf("the sibling column must land flat, got %#v", doc["channel"])
	}
	if doc["id"] != "o1" {
		t.Errorf("the shared ID must not be re-copied from the sibling, got %#v", doc["id"])
	}
}

// An absent sibling row leaves its fields OMITTED — never forced to an empty
// value, which a reader would render as "present and blank".
func TestMergeOwnerSiblings_AbsentRowOmitsTheFields(t *testing.T) {
	h := New(newScripted(nil))
	doc := Document{"id": "o1"}
	if err := h.MergeOwnerSiblings(context.Background(), doc, rootSchema(), "o1", false); err != nil {
		t.Fatalf("MergeOwnerSiblings: %v", err)
	}
	if _, present := doc["channel"]; present {
		t.Error("an absent sibling must leave its columns absent")
	}
}

func TestMergeOwnerSiblings_NoSiblingsIsANoOp(t *testing.T) {
	eng := newScripted(nil)
	doc := Document{"id": "n1"}
	if err := New(eng).MergeOwnerSiblings(context.Background(), doc, flatSchema(), "n1", true); err != nil {
		t.Fatalf("MergeOwnerSiblings: %v", err)
	}
	if len(eng.sqls) != 0 {
		t.Errorf("a schema with no siblings must issue no read, got %d", len(eng.sqls))
	}
}

func TestMergeOwnerSiblings_PropagatesTheReadError(t *testing.T) {
	eng := newScripted(nil)
	eng.err = errors.New("boom")
	err := New(eng).MergeOwnerSiblings(context.Background(), Document{"id": "o1"}, rootSchema(), "o1", true)
	if err == nil {
		t.Fatal("a failed sibling read must surface")
	}
}

// ─── own children ────────────────────────────────────────────────────────────

// Own children nest under the segment the DOMAIN declares, joined
// root.id -> child.ParentID, and each child row gets its own siblings merged.
func TestMergeOwnChildren_NestsUnderTheDeclaredSegment(t *testing.T) {
	h := New(newScripted(map[string][]map[string]any{
		"lines": {{"id": "l1", "label": "a", "order_id": "o1"}},
	}))
	doc := Document{"id": "o1"}
	if err := h.MergeOwnChildren(context.Background(), doc, rootSchema(), false); err != nil {
		t.Fatalf("MergeOwnChildren: %v", err)
	}
	rows, ok := doc["Lines"].([]Document)
	if !ok || len(rows) != 1 || rows[0]["label"] != "a" {
		t.Fatalf("children must nest under the domain-declared segment, got %#v", doc["Lines"])
	}
}

// No ID on the doc means nothing to join on: the read is skipped rather than
// issued with an empty key.
func TestMergeOwnChildren_WithoutTheRootIDIssuesNoRead(t *testing.T) {
	eng := newScripted(nil)
	if err := New(eng).MergeOwnChildren(context.Background(), Document{}, rootSchema(), true); err != nil {
		t.Fatalf("MergeOwnChildren: %v", err)
	}
	if len(eng.sqls) != 0 {
		t.Errorf("a doc with no id must issue no child read, got %d", len(eng.sqls))
	}
}

func TestMergeOwnChildren_NoChildrenIsANoOp(t *testing.T) {
	eng := newScripted(nil)
	if err := New(eng).MergeOwnChildren(context.Background(), Document{"id": "n1"}, flatSchema(), true); err != nil {
		t.Fatalf("MergeOwnChildren: %v", err)
	}
	if len(eng.sqls) != 0 {
		t.Errorf("a schema with no children must issue no read, got %d", len(eng.sqls))
	}
}

// ─── shared base ─────────────────────────────────────────────────────────────

// The base's business fields land FLAT on the role, and the base's MANAGED
// columns never overwrite the role's own — the document represents the ROLE,
// whose lifecycle is authoritative.
func TestMergeSharedBase_RoleManagedColumnsWin(t *testing.T) {
	h := New(newScripted(map[string][]map[string]any{
		"persons": {{
			"id": "p1", "name": "ana", "revision": int64(3),
			"deleted_at": nil, "created_at": "BASE-TIME",
		}},
	}))
	doc := Document{
		"id": "s1", "person_id": "p1", "grade": "A",
		"deleted_at": "ROLE-ARCHIVED", "created_at": "ROLE-TIME",
	}
	if err := h.MergeSharedBase(context.Background(), doc, roleSchema(), true); err != nil {
		t.Fatalf("MergeSharedBase: %v", err)
	}
	if doc["name"] != "ana" {
		t.Errorf("the base's business field must land flat, got %#v", doc["name"])
	}
	if doc["deleted_at"] != "ROLE-ARCHIVED" {
		t.Errorf("the base's NULL deleted_at must NOT hide the role's archival, got %#v", doc["deleted_at"])
	}
	if doc["created_at"] != "ROLE-TIME" {
		t.Errorf("the role's own timestamps must win, got %#v", doc["created_at"])
	}
	if doc["id"] != "s1" {
		t.Errorf("the base id must not overwrite the role id, got %#v", doc["id"])
	}
	if doc[BaseRevisionField] != int64(3) {
		t.Errorf("the base revision must ride its own watermark, got %#v", doc[BaseRevisionField])
	}
}

func TestMergeSharedBase_NoBaseOrNoLinkValueIsANoOp(t *testing.T) {
	eng := newScripted(nil)
	// A plain aggregate has no shared base at all.
	if err := New(eng).MergeSharedBase(context.Background(), Document{"id": "o1"}, rootSchema(), true); err != nil {
		t.Fatalf("MergeSharedBase: %v", err)
	}
	// A role whose link column is absent has nothing to join on.
	if err := New(eng).MergeSharedBase(context.Background(), Document{"id": "s1"}, roleSchema(), true); err != nil {
		t.Fatalf("MergeSharedBase: %v", err)
	}
	if len(eng.sqls) != 0 {
		t.Errorf("neither case may issue a read, got %d", len(eng.sqls))
	}
}

// The base's NATIVE children nest at the role's root, keyed by the base id the
// role's link column already carries.
func TestMergeSharedBaseChildren_NestsTheBaseCollections(t *testing.T) {
	eng := newScripted(map[string][]map[string]any{
		"addresses": {{"id": "a1", "city": "porto", "person_id": "p1"}},
	})
	doc := Document{"id": "s1", "person_id": "p1"}
	if err := New(eng).MergeSharedBaseChildren(context.Background(), doc, roleSchema(), false); err != nil {
		t.Fatalf("MergeSharedBaseChildren: %v", err)
	}
	rows, ok := doc["Addresses"].([]Document)
	if !ok || len(rows) != 1 {
		t.Fatalf("base children must nest under their segment, got %#v", doc["Addresses"])
	}
	if !strings.Contains(eng.sqls[0], "person_id = $1") {
		t.Errorf("base children join on the base id: %q", eng.sqls[0])
	}
}

func TestMergeSharedBaseChildren_NoBaseIsANoOp(t *testing.T) {
	eng := newScripted(nil)
	if err := New(eng).MergeSharedBaseChildren(context.Background(), Document{"id": "o1"}, rootSchema(), true); err != nil {
		t.Fatalf("MergeSharedBaseChildren: %v", err)
	}
	if len(eng.sqls) != 0 {
		t.Errorf("a schema with no shared base must issue no read, got %d", len(eng.sqls))
	}
}
