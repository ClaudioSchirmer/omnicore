package query

import (
	"context"
	"testing"
)

// bothSlotsMongo returns a fakeStore backed by one fakeColl per named collection,
// plus the map so a test can assert per-slot writes.
func bothSlotsMongo(names ...string) (*fakeStore, map[string]*fakeColl) {
	colls := make(map[string]*fakeColl, len(names))
	for _, n := range names {
		colls[n] = &fakeColl{}
	}
	return newFakeMongoFunc(func(name string) *fakeColl { return colls[name] }), colls
}

// shadowResolver builds a resolver whose "v" row records the given shadow slot,
// so ShadowActive("v") is true and Active("v") is bare (active_collection NULL).
func shadowResolver(t *testing.T, shadow string) (*ViewResolver, *fakeEngine) {
	t.Helper()
	eng := newFakeEngine(&fakeQuerier{queryFn: pointerRows("v", nil, &shadow)})
	r := NewViewResolver(eng)
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	return r, eng
}

func TestSyncEngine_DualApply_FansUpsertAndDeleteToBothSlots(t *testing.T) {
	ctx := context.Background()
	resolver, eng := shadowResolver(t, "v__0")
	mongo, colls := bothSlotsMongo("v", "v__0")
	s := &SyncEngine{eng: eng, mongo: mongo, resolver: resolver}

	if err := s.applyUpsert(ctx, "v", "id1", Document{"_id": "id1"}); err != nil {
		t.Fatalf("applyUpsert: %v", err)
	}
	if len(colls["v"].updates) != 1 || len(colls["v__0"].updates) != 1 {
		t.Errorf("upsert fanout: active=%d shadow=%d, want 1 and 1",
			len(colls["v"].updates), len(colls["v__0"].updates))
	}

	if err := s.applyDelete(ctx, "v", "id1", 0, 0); err != nil {
		t.Fatalf("applyDelete: %v", err)
	}
	if len(colls["v"].deletes) != 1 || len(colls["v__0"].deletes) != 1 {
		t.Errorf("delete fanout: active=%d shadow=%d, want 1 and 1",
			len(colls["v"].deletes), len(colls["v__0"].deletes))
	}
}

func TestSyncEngine_DualApply_ActiveOnlyWithoutRebuild(t *testing.T) {
	// No shadow recorded → ShadowActive false → only the active slot is written.
	mongo, colls := bothSlotsMongo("v")
	s := &SyncEngine{mongo: mongo, resolver: NewViewResolver(nil)}
	if err := s.applyUpsert(context.Background(), "v", "id1", Document{"_id": "id1"}); err != nil {
		t.Fatalf("applyUpsert: %v", err)
	}
	if len(colls["v"].updates) != 1 {
		t.Errorf("active writes = %d, want 1", len(colls["v"].updates))
	}
}

func TestSyncEngine_DualApply_ActiveFailFailsTheEvent(t *testing.T) {
	// An active-slot failure fails the event so at-least-once redelivery
	// reconverges — the pre-blue-green semantics, unchanged by dual-apply.
	colls := map[string]*fakeColl{"v": {updateErr: errFake, deleteErr: errFake}}
	mongo := newFakeMongoFunc(func(name string) *fakeColl { return colls[name] })
	s := &SyncEngine{mongo: mongo, resolver: NewViewResolver(nil)}
	if err := s.applyUpsert(context.Background(), "v", "id1", Document{"_id": "id1"}); err == nil {
		t.Error("active upsert failure must fail the event")
	}
	if err := s.applyDelete(context.Background(), "v", "id1", 0, 0); err == nil {
		t.Error("active delete failure must fail the event")
	}
}

func TestSyncEngine_DualApply_AbortErrorNeverFailsLive(t *testing.T) {
	// Shadow write fails AND the abort's registry write also fails: dualApply
	// logs and moves on — the live path (the active write) still succeeds.
	ctx := context.Background()
	resolver, eng := shadowResolver(t, "v__0")
	eng.q.(*fakeQuerier).execFn = func(string, []any) error { return errFake }
	colls := map[string]*fakeColl{"v": {}, "v__0": {updateErr: errFake, deleteErr: errFake}}
	mongo := newFakeMongoFunc(func(name string) *fakeColl { return colls[name] })
	s := &SyncEngine{eng: eng, mongo: mongo, resolver: resolver}
	if err := s.applyDelete(ctx, "v", "id1", 0, 0); err != nil {
		t.Fatalf("live path must not fail even when the abort itself errors: %v", err)
	}
	if len(colls["v"].deletes) != 1 {
		t.Errorf("active slot must still be deleted, got %d", len(colls["v"].deletes))
	}
}

func TestSyncEngine_DualApply_ShadowFailAbortsWithoutFailingLive(t *testing.T) {
	ctx := context.Background()
	resolver, eng := shadowResolver(t, "v__0")

	// Any Exec in this path is abortSlotRebuild (Refresh uses Query, not Exec).
	var aborted bool
	eng.q.(*fakeQuerier).execFn = func(string, []any) error { aborted = true; return nil }

	// Active slot writes fine; shadow slot always errors.
	colls := map[string]*fakeColl{"v": {}, "v__0": {updateErr: errFake}}
	mongo := newFakeMongoFunc(func(name string) *fakeColl { return colls[name] })
	s := &SyncEngine{eng: eng, mongo: mongo, resolver: resolver}

	// The live path must NOT fail even though the shadow write does.
	if err := s.applyUpsert(ctx, "v", "id1", Document{"_id": "id1"}); err != nil {
		t.Fatalf("applyUpsert must not fail the live path on a shadow error: %v", err)
	}
	if len(colls["v"].updates) != 1 {
		t.Errorf("active slot must still be written, got %d", len(colls["v"].updates))
	}
	if !aborted {
		t.Error("expected the rebuild to be aborted after the shadow retries were exhausted")
	}
}
