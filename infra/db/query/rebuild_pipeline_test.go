package query

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// countingStore is a concurrency-safe ReadModelStore recorder: it tallies how
// many times each _id was written by the backfill's batched port call, under a
// mutex, so a workers>1 backfill can be asserted for data integrity under the
// race detector (the plain fakeColl is not safe for concurrent bulk writes).
// The backfill drives BulkApplyProjection (the guarded pipeline batch), so
// that is the method counted. Every other port method is promoted from the
// embedded fakeStore (unused by backfillInto, which only writes).
type countingStore struct {
	*fakeStore
	mu      sync.Mutex
	byID    map[string]int
	fail    string // if set, BulkApplyProjection errors on any batch containing this id
	capture func(items []IdentifiedStages)
}

func newCountingStore() *countingStore {
	return &countingStore{fakeStore: newFakeMongo(&fakeColl{}), byID: map[string]int{}}
}

func (c *countingStore) BulkApplyProjection(_ context.Context, _ PhysicalCollection, items []IdentifiedStages) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, it := range items {
		if c.fail != "" && it.ID == c.fail {
			return errFake
		}
	}
	for _, it := range items {
		c.byID[it.ID]++
	}
	if c.capture != nil {
		c.capture(items)
	}
	return nil
}

// TestBackfillInto_ConcurrentWorkersWriteEveryRootOnce is the data-integrity
// guarantee under real parallelism: 4 workers, batchSize 3, 50 roots (17
// batches). Every root must be composed and bulk-upserted EXACTLY once — no lost
// batch, no double write, no race (run under -race).
func TestBackfillInto_ConcurrentWorkersWriteEveryRootOnce(t *testing.T) {
	view := rebuildView()
	ids := make([]string, 50)
	for i := range ids {
		ids[i] = fmt.Sprintf("id%d", i)
	}
	store := newCountingStore()
	s := scriptSyncEngine(newScriptEngine(ids, aliveRoot), store, []*ViewDefinition{view})

	if err := s.backfillInto(context.Background(), view, pc("shadow"), "", 4, 3, nil); err != nil {
		t.Fatalf("backfillInto: %v", err)
	}
	if len(store.byID) != len(ids) {
		t.Fatalf("expected %d distinct roots written, got %d", len(ids), len(store.byID))
	}
	for _, id := range ids {
		if store.byID[id] != 1 {
			t.Errorf("root %s written %d times, want exactly 1", id, store.byID[id])
		}
	}
}

// TestBackfillInto_EveryWriteIsRevisionGuarded pins the backfill-clobber
// guarantee (§10 of tasks/sync_fixes.md): the backfill batch writes through the
// GUARDED pipeline, never a plain full-document overwrite. A live write racing
// the backfill dual-applies to the shadow with a fresher revision; an unguarded
// batch landing afterwards would silently regress the slot about to flip. The
// observable at this seam: every IdentifiedStages carries guard stages that
// reference the revision watermark (a plain $set of composed data would carry
// no watermark at all — "watermarks travel via their guard stages, never as
// data").
func TestBackfillInto_EveryWriteIsRevisionGuarded(t *testing.T) {
	view := rebuildView()
	ids := []string{"id1", "id2", "id3"}
	store := newCountingStore()
	var mu sync.Mutex
	var captured []IdentifiedStages
	store.capture = func(items []IdentifiedStages) {
		mu.Lock()
		captured = append(captured, items...)
		mu.Unlock()
	}
	s := scriptSyncEngine(newScriptEngine(ids, aliveRoot), store, []*ViewDefinition{view})

	if err := s.backfillInto(context.Background(), view, pc("shadow"), "", 2, 2, nil); err != nil {
		t.Fatalf("backfillInto: %v", err)
	}
	if len(captured) != len(ids) {
		t.Fatalf("expected %d writes captured, got %d", len(ids), len(captured))
	}
	for _, it := range captured {
		if len(it.Stages) == 0 {
			t.Fatalf("root %s written with no stages — a raw overwrite", it.ID)
		}
		if !strings.Contains(fmt.Sprint(it.Stages), docRevisionField) {
			t.Errorf("root %s written WITHOUT a revision guard — a stale backfill batch could clobber a fresher dual-applied write; stages: %v",
				it.ID, it.Stages)
		}
	}
}

// TestBackfillInto_ConcurrentAbortTerminates proves that when a worker errors
// mid-stream with several workers in flight, the pipeline returns the error and
// terminates cleanly (all workers drain and exit — the test would hang on a
// leak/deadlock). Under -race this also proves no concurrent misuse on abort.
func TestBackfillInto_ConcurrentAbortTerminates(t *testing.T) {
	view := rebuildView()
	ids := make([]string, 40)
	for i := range ids {
		ids[i] = fmt.Sprintf("id%d", i)
	}
	store := newCountingStore()
	store.fail = "id25" // whichever batch carries id25 fails, cancelling the run

	s := scriptSyncEngine(newScriptEngine(ids, aliveRoot), store, []*ViewDefinition{view})
	if err := s.backfillInto(context.Background(), view, pc("shadow"), "", 4, 3, nil); err == nil {
		t.Fatal("expected the worker error to propagate out of the pipeline")
	}
	// The active slot is never written here (this is the shadow target), and a
	// partial shadow is acceptable because ExecuteRebuild never flips on error —
	// the point of this test is that the run TERMINATED (no hang) and surfaced the
	// error. A clean partial is fine: fewer than all ids may have been written.
	if len(store.byID) >= len(ids) {
		t.Errorf("abort should have stopped before writing every root, wrote %d/%d", len(store.byID), len(ids))
	}
}

// TestBackfillInto_MultipleBatchesAllComposed drives the producer/consumer
// pipeline across more than one batch: with batchSize=2 the five source ids cut
// into three batches ([id1,id2],[id3,id4],[id5]) and every composed root must be
// bulk-upserted exactly once. workers=1 keeps the fake store single-threaded —
// the concurrency plumbing itself (goroutine pool, bounded channel, close+Wait,
// worker error propagation) is exercised by the default worker count in the
// RebuildView / ExecuteRebuild coverage tests.
func TestBackfillInto_MultipleBatchesAllComposed(t *testing.T) {
	view := rebuildView()
	ids := []string{"id1", "id2", "id3", "id4", "id5"}
	coll := &fakeColl{}
	s := scriptSyncEngine(newScriptEngine(ids, aliveRoot), newFakeMongo(coll), []*ViewDefinition{view})

	if err := s.backfillInto(context.Background(), view, pc("shadow"), "", 1, 2, nil); err != nil {
		t.Fatalf("backfillInto: %v", err)
	}
	if len(coll.updates) != len(ids) {
		t.Errorf("expected %d upserts across batches, got %d", len(ids), len(coll.updates))
	}
}

// TestBackfillInto_WorkerErrorCancelsProducer covers the cancel path: many
// single-id batches with a store that fails every bulk write. The first worker
// error sets firstErr and cancels the context, so the producer stops sending
// mid-scan (send() hits ctx.Done()) and the error propagates out of Wait().
func TestBackfillInto_WorkerErrorCancelsProducer(t *testing.T) {
	view := rebuildView()
	ids := []string{"id1", "id2", "id3", "id4", "id5", "id6", "id7", "id8"}
	coll := &fakeColl{updateErr: errFake}
	s := scriptSyncEngine(newScriptEngine(ids, aliveRoot), newFakeMongo(coll), []*ViewDefinition{view})

	if err := s.backfillInto(context.Background(), view, pc("shadow"), "", 1, 1, nil); err == nil {
		t.Fatal("expected the bulk-write error to propagate out of the pipeline")
	}
}

// TestBackfillInto_ZeroTuningUsesDefaults proves the 0 → framework-default
// resolution: a zero workers/batchSize still composes and writes every root
// (single batch under the default 1000).
func TestBackfillInto_ZeroTuningUsesDefaults(t *testing.T) {
	view := rebuildView()
	coll := &fakeColl{}
	s := scriptSyncEngine(newScriptEngine([]string{"id1", "id2"}, aliveRoot), newFakeMongo(coll), []*ViewDefinition{view})

	if err := s.backfillInto(context.Background(), view, pc("shadow"), "", 0, 0, nil); err != nil {
		t.Fatalf("backfillInto: %v", err)
	}
	if len(coll.updates) != 2 {
		t.Errorf("expected 2 upserts, got %d", len(coll.updates))
	}
}
