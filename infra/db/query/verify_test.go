package query

import (
	"context"
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
