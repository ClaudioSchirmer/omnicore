package hydrate

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The batched merges must produce the IDENTICAL per-document result the per-row
// chain does — they differ only in round trips: one IN (...) per related table
// for the whole batch instead of one query per aggregate.

func TestMergeOwnerSiblingsBatch_MatchesThePerRowMergeInOneRead(t *testing.T) {
	eng := newScripted(map[string][]map[string]any{
		"order_channels": {
			{"id": "o1", "channel": "web"},
			{"id": "o2", "channel": "store"},
		},
	})
	docs := []Document{{"id": "o1"}, {"id": "o2"}}
	if err := New(eng).MergeOwnerSiblingsBatch(context.Background(), docs, rootSchema(), false); err != nil {
		t.Fatalf("MergeOwnerSiblingsBatch: %v", err)
	}
	if docs[0]["channel"] != "web" || docs[1]["channel"] != "store" {
		t.Fatalf("each doc must get its OWN sibling row: %v", docs)
	}
	if len(eng.sqls) != 1 {
		t.Errorf("a batch must pay ONE read per sibling table, got %d", len(eng.sqls))
	}
	if docs[0]["id"] != "o1" {
		t.Error("the shared ID must not be copied from the sibling row")
	}
}

func TestMergeOwnerSiblingsBatch_EmptyInputsAreNoOps(t *testing.T) {
	eng := newScripted(nil)
	h := New(eng)
	if err := h.MergeOwnerSiblingsBatch(context.Background(), nil, rootSchema(), true); err != nil {
		t.Fatalf("no docs: %v", err)
	}
	if err := h.MergeOwnerSiblingsBatch(context.Background(), []Document{{"id": "n1"}}, flatSchema(), true); err != nil {
		t.Fatalf("no siblings: %v", err)
	}
	if len(eng.sqls) != 0 {
		t.Errorf("neither case may read, got %d", len(eng.sqls))
	}
}

func TestMergeOwnChildrenBatch_GroupsAndNormalizesTheChildlessRoot(t *testing.T) {
	eng := newScripted(map[string][]map[string]any{
		"lines": {
			{"id": "l1", "label": "a", "order_id": "o1"},
			{"id": "l2", "label": "b", "order_id": "o1"},
		},
	})
	docs := []Document{{"id": "o1"}, {"id": "o2"}}
	if err := New(eng).MergeOwnChildrenBatch(context.Background(), docs, rootSchema(), false); err != nil {
		t.Fatalf("MergeOwnChildrenBatch: %v", err)
	}
	if got, _ := docs[0]["Lines"].([]Document); len(got) != 2 {
		t.Fatalf("o1 must carry both children, got %#v", docs[0]["Lines"])
	}
	// A root with no children composes an EMPTY array, never a missing field.
	got, ok := docs[1]["Lines"].([]Document)
	if !ok || got == nil || len(got) != 0 {
		t.Fatalf("a childless root must carry an empty array, got %#v", docs[1]["Lines"])
	}
}

func TestMergeOwnChildrenBatch_SkipsDocsWithNoKey(t *testing.T) {
	eng := newScripted(map[string][]map[string]any{
		"lines": {{"id": "l1", "label": "a", "order_id": "o1"}},
	})
	docs := []Document{{"id": "o1"}, {}}
	if err := New(eng).MergeOwnChildrenBatch(context.Background(), docs, rootSchema(), true); err != nil {
		t.Fatalf("MergeOwnChildrenBatch: %v", err)
	}
	if _, present := docs[1]["Lines"]; present {
		t.Error("a doc with no id must get no child segment at all")
	}
}

func TestMergeOwnChildrenBatch_EmptyInputsAreNoOps(t *testing.T) {
	eng := newScripted(nil)
	h := New(eng)
	if err := h.MergeOwnChildrenBatch(context.Background(), nil, rootSchema(), true); err != nil {
		t.Fatalf("no docs: %v", err)
	}
	if err := h.MergeOwnChildrenBatch(context.Background(), []Document{{"id": "n1"}}, flatSchema(), true); err != nil {
		t.Fatalf("no children: %v", err)
	}
	if len(eng.sqls) != 0 {
		t.Errorf("neither case may read, got %d", len(eng.sqls))
	}
}

// The batched shared-base merge applies the SAME managed-column skip as the
// per-row one — the guard lives in one place (sharedBaseSkipSet) precisely so
// the two cannot drift.
func TestMergeSharedBaseBatch_AppliesTheSameManagedSkip(t *testing.T) {
	eng := newScripted(map[string][]map[string]any{
		"persons": {{
			"id": "p1", "name": "ana", "revision": int64(9),
			"deleted_at": nil, "created_at": "BASE-TIME",
		}},
	})
	docs := []Document{{
		"id": "s1", "person_id": "p1",
		"deleted_at": "ROLE-ARCHIVED", "created_at": "ROLE-TIME",
	}}
	if err := New(eng).MergeSharedBaseBatch(context.Background(), docs, roleSchema(), true); err != nil {
		t.Fatalf("MergeSharedBaseBatch: %v", err)
	}
	if docs[0]["name"] != "ana" {
		t.Errorf("the base business field must merge flat, got %#v", docs[0]["name"])
	}
	if docs[0]["deleted_at"] != "ROLE-ARCHIVED" || docs[0]["created_at"] != "ROLE-TIME" {
		t.Errorf("the role's managed columns must survive the base merge: %v", docs[0])
	}
	if docs[0][BaseRevisionField] != int64(9) {
		t.Errorf("the base revision must ride its watermark, got %#v", docs[0][BaseRevisionField])
	}
	if _, still := docs[0]["revision"]; still {
		t.Error("the base's physical revision column must not survive as a field")
	}
}

func TestMergeSharedBaseBatch_NoBaseIsANoOp(t *testing.T) {
	eng := newScripted(nil)
	if err := New(eng).MergeSharedBaseBatch(context.Background(), []Document{{"id": "o1"}}, rootSchema(), true); err != nil {
		t.Fatalf("MergeSharedBaseBatch: %v", err)
	}
	if len(eng.sqls) != 0 {
		t.Errorf("a plain aggregate must issue no base read, got %d", len(eng.sqls))
	}
}

func TestMergeSharedBaseChildrenBatch_NestsPerDocAndSkipsTheUnlinked(t *testing.T) {
	eng := newScripted(map[string][]map[string]any{
		"addresses": {{"id": "a1", "city": "porto", "person_id": "p1"}},
	})
	docs := []Document{{"id": "s1", "person_id": "p1"}, {"id": "s2"}}
	if err := New(eng).MergeSharedBaseChildrenBatch(context.Background(), docs, roleSchema(), false); err != nil {
		t.Fatalf("MergeSharedBaseChildrenBatch: %v", err)
	}
	if got, _ := docs[0]["Addresses"].([]Document); len(got) != 1 {
		t.Fatalf("the linked role must carry the base children, got %#v", docs[0]["Addresses"])
	}
	if _, present := docs[1]["Addresses"]; present {
		t.Error("a role with no base link must get no base-child segment")
	}
	if !strings.Contains(eng.sqls[0], "deleted_at IS NULL") {
		t.Errorf("the archived gate must reach the batched read: %q", eng.sqls[0])
	}
}

func TestMergeSharedBaseChildrenBatch_NoBaseChildrenIsANoOp(t *testing.T) {
	eng := newScripted(nil)
	if err := New(eng).MergeSharedBaseChildrenBatch(context.Background(), []Document{{"id": "o1"}}, rootSchema(), true); err != nil {
		t.Fatalf("MergeSharedBaseChildrenBatch: %v", err)
	}
	if len(eng.sqls) != 0 {
		t.Errorf("a schema with no shared base must issue no read, got %d", len(eng.sqls))
	}
}

func TestBatchMerges_PropagateTheReadError(t *testing.T) {
	boom := errors.New("connection reset")
	h := func() *Hydrator {
		e := newScripted(nil)
		e.err = boom
		return New(e)
	}
	ctx := context.Background()
	if err := h().MergeOwnerSiblingsBatch(ctx, []Document{{"id": "o1"}}, rootSchema(), true); err == nil {
		t.Error("siblings batch must surface the read error")
	}
	if err := h().MergeOwnChildrenBatch(ctx, []Document{{"id": "o1"}}, rootSchema(), true); err == nil {
		t.Error("children batch must surface the read error")
	}
	if err := h().MergeSharedBaseBatch(ctx, []Document{{"id": "s1", "person_id": "p1"}}, roleSchema(), true); err == nil {
		t.Error("shared-base batch must surface the read error")
	}
	if err := h().MergeSharedBaseChildrenBatch(ctx, []Document{{"id": "s1", "person_id": "p1"}}, roleSchema(), true); err == nil {
		t.Error("base-children batch must surface the read error")
	}
}
