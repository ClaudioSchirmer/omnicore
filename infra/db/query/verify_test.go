package query

import (
	"context"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// shapedDoc returns a shadow document with the aliveRoot field shape plus _id,
// so the value-sample pass matches a fresh compose.
func shapedDoc(id string) map[string]any {
	return map[string]any{"_id": id, "id": id, "name": "n-" + id, "deleted_at": nil}
}

func TestVerifyShadow_ReverseDeletesResurrectedOrphan(t *testing.T) {
	view := rebuildView()
	shadow := &fakeColl{docs: []any{shapedDoc("a"), shapedDoc("ghost")}}
	s := scriptSyncEngine(newScriptEngine([]string{"a"}, aliveRoot), newFakeMongo(shadow), []*ViewDefinition{view})
	if err := s.verifyShadow(context.Background(), view, pc("shadow")); err != nil {
		t.Fatalf("verifyShadow: %v", err)
	}
	if len(shadow.deletes) != 1 || shadow.deletes[0] != "ghost" {
		t.Errorf("reverse pass must delete the resurrected orphan, got deletes=%v", shadow.deletes)
	}
}

func TestVerifyShadow_ForwardAutoCorrectsGap(t *testing.T) {
	view := rebuildView()
	// The shadow holds "a" but the source has "a" and "b" — b's dual-apply write
	// was dropped and the forward pass must recompose it into the shadow.
	shadow := &fakeColl{docs: []any{shapedDoc("a")}}
	s := scriptSyncEngine(newScriptEngine([]string{"a", "b"}, aliveRoot), newFakeMongo(shadow), []*ViewDefinition{view})
	if err := s.verifyShadow(context.Background(), view, pc("shadow")); err != nil {
		t.Fatalf("verifyShadow: %v", err)
	}
	if len(shadow.updates) == 0 {
		t.Error("forward pass must recompose the missing id into the shadow")
	}
}

func TestVerifyShadow_AbortsOnShapeRegression(t *testing.T) {
	view := rebuildView()
	// The stored doc for "a" is missing the NON-null composed field "name" → a
	// real structural drop that survives the single re-check → abort. (A missing
	// nullable field, e.g. deleted_at, reads as null and is NOT drift — covered
	// by TestSameFieldShape_NullKeyEqualsAbsent.)
	shadow := &fakeColl{docs: []any{map[string]any{"_id": "a", "id": "a", "deleted_at": nil}}}
	s := scriptSyncEngine(newScriptEngine([]string{"a"}, aliveRoot), newFakeMongo(shadow), []*ViewDefinition{view})
	err := s.verifyShadow(context.Background(), view, pc("shadow"))
	if err == nil || !strings.Contains(err.Error(), "diverges in shape") {
		t.Fatalf("expected a shape-divergence abort, got %v", err)
	}
}

func TestVerifyShadow_SnapshotErrorPropagates(t *testing.T) {
	view := rebuildView()
	shadow := &fakeColl{findErr: errFake} // SnapshotDocumentIDs fails
	s := scriptSyncEngine(newScriptEngine([]string{"a"}, aliveRoot), newFakeMongo(shadow), []*ViewDefinition{view})
	if err := s.verifyShadow(context.Background(), view, pc("shadow")); err == nil {
		t.Fatal("expected the shadow snapshot error to propagate")
	}
}

func TestVerifyShadow_SourceScanErrorPropagates(t *testing.T) {
	view := rebuildView()
	shadow := &fakeColl{docs: []any{shapedDoc("a")}}
	eng := newScriptEngine([]string{"a"}, aliveRoot)
	eng.q.(*fakeQuerier).queryFn = func(string, []any) (core.Rows, error) { return nil, errFake }
	s := scriptSyncEngine(eng, newFakeMongo(shadow), []*ViewDefinition{view})
	if err := s.verifyShadow(context.Background(), view, pc("shadow")); err == nil {
		t.Fatal("expected the source-scan error to propagate")
	}
}

func TestVerifyShadow_SourceSchemaNilErrors(t *testing.T) {
	// A view with no root .Schema(...) has no PK to scan → sourceIDSet errors,
	// which verify surfaces.
	bare := View("bare").Version(1)
	shadow := &fakeColl{docs: []any{shapedDoc("a")}}
	s := scriptSyncEngine(newScriptEngine([]string{"a"}, aliveRoot), newFakeMongo(shadow), []*ViewDefinition{bare})
	if err := s.verifyShadow(context.Background(), bare, pc("shadow")); err == nil {
		t.Fatal("expected the no-schema error from the source scan")
	}
}

// A clean shadow (matches the source exactly) passes all three passes untouched.
func TestVerifyShadow_CleanPasses(t *testing.T) {
	view := rebuildView()
	shadow := &fakeColl{docs: []any{shapedDoc("a"), shapedDoc("b")}}
	s := scriptSyncEngine(newScriptEngine([]string{"a", "b"}, aliveRoot), newFakeMongo(shadow), []*ViewDefinition{view})
	if err := s.verifyShadow(context.Background(), view, pc("shadow")); err != nil {
		t.Fatalf("clean verify must pass, got %v", err)
	}
	if len(shadow.deletes) != 0 {
		t.Errorf("clean verify must delete nothing, got %v", shadow.deletes)
	}
}

// A rebuild that adds a nullable column: a fresh compose emits it as an explicit
// null key, while a mid-rebuild writer on the PREVIOUS binary (schema without
// the column) creates the shadow document WITHOUT the key. Both read as null —
// the shape check must treat "key present = null" and "key absent" as the same.
func TestSameFieldShape_NullKeyEqualsAbsent(t *testing.T) {
	fresh := Document{"_id": "1", "name": "Ann", "nickname": nil} // V2 compose: explicit null
	stored := Document{"_id": "1", "name": "Ann"}                 // V1-created shadow: no key
	if !sameFieldShape(fresh, stored) {
		t.Error("a null-valued fresh key must be equivalent to an absent stored key")
	}
	// Symmetric — an explicit null on the stored side too (e.g. a removed column).
	if !sameFieldShape(fresh, Document{"_id": "1", "name": "Ann", "nickname": nil}) {
		t.Error("explicit null on both sides must match")
	}
}

// The fix must NOT mask a real drop: a field with a NON-null value present on
// one side and absent on the other is genuine shape drift.
func TestSameFieldShape_NonNullDropIsStillDrift(t *testing.T) {
	fresh := Document{"_id": "1", "name": "Ann", "nickname": "Annie"}
	stored := Document{"_id": "1", "name": "Ann"} // dropped a real, non-null value
	if sameFieldShape(fresh, stored) {
		t.Error("a non-null field missing from the stored doc must be flagged as drift")
	}
}

// deleted_at is the soft-delete gate: null=alive, timestamp=archived. The
// absent≡null tolerance equates two ALIVE representations (null and absent), but
// the sensitive alive↔archived transition is a null↔non-null difference and MUST
// still be flagged — a doc archived in the source whose shadow reads as alive
// (or vice-versa) is real drift, not a benign representation difference.
func TestSameFieldShape_SoftDeleteTransitionIsStillDrift(t *testing.T) {
	archived := Document{"_id": "1", "name": "Ann", "deleted_at": "2026-01-01T00:00:00Z"}
	alive := Document{"_id": "1", "name": "Ann"} // deleted_at absent → alive
	if sameFieldShape(archived, alive) {
		t.Error("archived (non-null deleted_at) vs alive (absent) must be drift")
	}
	if sameFieldShape(alive, archived) {
		t.Error("alive (absent) vs archived (non-null deleted_at) must be drift")
	}
	// Two ALIVE representations — null and absent — are equivalent.
	if !sameFieldShape(Document{"_id": "1", "name": "Ann", "deleted_at": nil}, alive) {
		t.Error("alive-null and alive-absent must be equivalent")
	}
}

// A key PRESENT on both sides but with different values — e.g. the shadow stored
// an embed array as `items: null` while a fresh compose materialized it as a
// populated array (a later event fills it) — is a VALUE difference on a present
// key, NOT shape drift. The verify is value-blind for present keys, so it must
// not abort the rebuild on this (regression guard: an over-broad null-skip would
// drop the stored null key and mis-read it as fresh-only).
func TestSameFieldShape_PresentNullVsPopulatedIsNotDrift(t *testing.T) {
	fresh := Document{"_id": "1", "name": "Ann", "items": []any{map[string]any{"id": "x"}}}
	stored := Document{"_id": "1", "name": "Ann", "items": nil} // present, but null
	if !sameFieldShape(fresh, stored) {
		t.Error("present-null vs present-populated is a value diff, not shape drift")
	}
	if !sameFieldShape(stored, fresh) {
		t.Error("symmetric: present-null vs present-populated must not be drift")
	}
}
