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

	if err := s.applyProjection(ctx, "v", "id1", guardedStagesForTest(), true); err != nil {
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
	if err := s.applyProjection(context.Background(), "v", "id1", guardedStagesForTest(), true); err != nil {
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
	if err := s.applyProjection(context.Background(), "v", "id1", guardedStagesForTest(), true); err == nil {
		t.Error("active projection failure must fail the event")
	}
	if err := s.applyDelete(context.Background(), "v", "id1", 0, 0); err == nil {
		t.Error("active delete failure must fail the event")
	}
}

// CONTRACT CHANGE. A shadow-write failure used to be swallowed: the live path
// succeeded, the event was confirmed, and the rebuild was abandoned
// cluster-wide on 150 ms of evidence from ONE event. Both halves were wrong.
//
//   - Swallowing it was only survivable because nothing was retried. Now the
//     event FAILS and the whole message is replayed, with the shadow write among
//     the obligations retried — the cheap, local recovery.
//   - Abandoning hours of backfill for every pod is the expensive, cluster-wide
//     recovery, and it now needs sustained evidence (shadowAbortThreshold
//     consecutive failing events).
func TestSyncEngine_DualApply_ShadowFailFailsTheEventForRetry(t *testing.T) {
	ctx := context.Background()
	resolver, eng := shadowResolver(t, "v__0")
	shadowHealth.succeeded("v") // isolate from other tests' streaks

	var aborted bool
	eng.q.(*fakeQuerier).execFn = func(string, []any) error { aborted = true; return nil }

	// Active slot writes fine; shadow slot always errors.
	colls := map[string]*fakeColl{"v": {}, "v__0": {updateErr: errFake}}
	mongo := newFakeMongoFunc(func(name string) *fakeColl { return colls[name] })
	s := &SyncEngine{eng: eng, mongo: mongo, resolver: resolver}

	if err := s.applyProjection(ctx, "v", "id1", guardedStagesForTest(), true); err == nil {
		t.Fatal("a shadow-write failure must fail the event so the message is retried")
	}
	// The active slot is still written — the failure is reported, not rolled back;
	// the retry re-applies both writes idempotently.
	if len(colls["v"].updates) != 1 {
		t.Errorf("active slot must still be written, got %d", len(colls["v"].updates))
	}
	if aborted {
		t.Error("one failing event must NOT abandon the rebuild — the abort needs sustained evidence")
	}
}

// TestSyncEngine_DualApply_AbortsOnSustainedFailure: once the streak reaches the
// threshold the shadow is genuinely unreachable and the rebuild is abandoned, so
// no pod keeps dual-applying to a slot that will never flip.
func TestSyncEngine_DualApply_AbortsOnSustainedFailure(t *testing.T) {
	ctx := context.Background()
	resolver, eng := shadowResolver(t, "v__0")
	shadowHealth.succeeded("v")

	var aborted bool
	eng.q.(*fakeQuerier).execFn = func(string, []any) error { aborted = true; return nil }
	colls := map[string]*fakeColl{"v": {}, "v__0": {updateErr: errFake}}
	mongo := newFakeMongoFunc(func(name string) *fakeColl { return colls[name] })
	s := &SyncEngine{eng: eng, mongo: mongo, resolver: resolver}

	for i := 0; i < shadowAbortThreshold; i++ {
		if err := s.applyProjection(ctx, "v", "id1", guardedStagesForTest(), true); err == nil {
			t.Fatalf("event %d: shadow failure must fail the event", i+1)
		}
	}
	if !aborted {
		t.Errorf("expected the rebuild to be abandoned after %d consecutive failing events", shadowAbortThreshold)
	}
}

// TestSyncEngine_DualApply_SuccessClearsTheStreak: one reachable shadow write
// proves the slot is alive, so the evidence for abandoning it is gone. Without
// this reset an intermittent shadow would accumulate failures forever and
// eventually abandon a healthy rebuild.
func TestSyncEngine_DualApply_SuccessClearsTheStreak(t *testing.T) {
	ctx := context.Background()
	resolver, eng := shadowResolver(t, "v__0")
	shadowHealth.succeeded("v")

	var aborted bool
	eng.q.(*fakeQuerier).execFn = func(string, []any) error { aborted = true; return nil }
	shadow := &fakeColl{updateErr: errFake}
	colls := map[string]*fakeColl{"v": {}, "v__0": shadow}
	mongo := newFakeMongoFunc(func(name string) *fakeColl { return colls[name] })
	s := &SyncEngine{eng: eng, mongo: mongo, resolver: resolver}

	// Fail just short of the threshold, then succeed once.
	for i := 0; i < shadowAbortThreshold-1; i++ {
		_ = s.applyProjection(ctx, "v", "id1", guardedStagesForTest(), true)
	}
	shadow.updateErr = nil
	if err := s.applyProjection(ctx, "v", "id1", guardedStagesForTest(), true); err != nil {
		t.Fatalf("a healthy shadow write must succeed: %v", err)
	}
	// The streak is cleared, so the next failure starts from one again.
	shadow.updateErr = errFake
	_ = s.applyProjection(ctx, "v", "id1", guardedStagesForTest(), true)
	if aborted {
		t.Error("a successful shadow write must clear the streak — the rebuild was abandoned anyway")
	}
}

// guardedStagesForTest is the minimal revision-guarded pipeline the live write
// path applies. These tests exercise dual-apply, not composition, so the stage
// content only has to be well-formed.
func guardedStagesForTest() []Document {
	return []Document{{"$set": Document{"x": lit(1)}}}
}
