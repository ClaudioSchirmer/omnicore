package query

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// reconcileSource scripts the relational side of a parity sweep: one page of
// (pk, revision) pairs, then an empty page to end the walk. Keyset paging means
// the second call carries a cursor, which is how the fake knows to stop.
func reconcileSource(rows [][2]any) func(sql string, args []any) (core.Rows, error) {
	return func(sql string, args []any) (core.Rows, error) {
		if !strings.Contains(sql, "revision") {
			return &fakeRows{}, nil
		}
		if len(args) > 0 { // a cursor is bound: the walk already delivered its page
			return &fakeRows{}, nil
		}
		return &fakeRows{
			rows: len(rows),
			scan: func(i int, dest []any) error {
				*(dest[0].(*string)) = rows[i][0].(string)
				*(dest[1].(*int64)) = int64(rows[i][1].(int))
				return nil
			},
		}, nil
	}
}

// reconcileEngine wires a sync engine whose source yields the given rows and
// whose composer can recompose them.
func reconcileEngine(t *testing.T, rows [][2]any, slot *fakeColl) (*SyncEngine, *ViewDefinition) {
	t.Helper()
	view := convRoleView()
	eng := newFakeEngine(&fakeQuerier{
		queryFn:     reconcileSource(rows),
		queryMapsFn: convComposerRows,
	})
	store := newFakeMongo(slot)
	identityResolver.lease = 0
	return NewSyncEngine(eng, store, identityResolver, nil, "grp", []*ViewDefinition{view}, 1), view
}

// TestReconcile_ConvergedViewReportsNoDivergence: the sweep must be silent when
// the projection is in step. A backstop that cries wolf on healthy state gets
// muted, and then it is not a backstop.
func TestReconcile_ConvergedViewReportsNoDivergence(t *testing.T) {
	slot := &fakeColl{docs: []any{
		map[string]any{"_id": "a1", DocRevisionField: int64(5)},
		map[string]any{"_id": "a2", DocRevisionField: int64(3)},
	}}
	s, view := reconcileEngine(t, [][2]any{{"a1", 5}, {"a2", 3}}, slot)

	rep, err := s.ReconcileView(context.Background(), view, ReconcileConfig{RowsPerSecond: -1})
	if err != nil {
		t.Fatalf("ReconcileView: %v", err)
	}
	if rep.Scanned != 2 {
		t.Errorf("scanned = %d, want 2", rep.Scanned)
	}
	if rep.Diverged() != 0 {
		t.Errorf("a converged view must report no divergence, got missing=%d stale=%d", rep.Missing, rep.Stale)
	}
}

// TestReconcile_DetectsMissingDocument is the insert-once case that motivated
// the whole program: an aggregate whose only event was lost has no later write
// to reconverge it, so the sweep is the only thing that can ever bring it back.
func TestReconcile_DetectsMissingDocument(t *testing.T) {
	slot := &fakeColl{docs: []any{
		map[string]any{"_id": "a1", DocRevisionField: int64(5)},
	}}
	s, view := reconcileEngine(t, [][2]any{{"a1", 5}, {"a2", 3}}, slot)

	rep, err := s.ReconcileView(context.Background(), view, ReconcileConfig{RowsPerSecond: -1})
	if err != nil {
		t.Fatalf("ReconcileView: %v", err)
	}
	if rep.Missing != 1 {
		t.Errorf("missing = %d, want 1 (a2 has no document)", rep.Missing)
	}
	if rep.Stale != 0 {
		t.Errorf("stale = %d, want 0", rep.Stale)
	}
	if rep.Repaired != 1 {
		t.Errorf("the sweep must repair what it finds, repaired = %d", rep.Repaired)
	}
	if len(slot.updates) == 0 {
		t.Error("expected a recompose write into the active slot")
	}
}

// TestReconcile_DetectsStaleDocument: a document present but behind the source
// revision is I1 divergence just as much as an absent one — this is what a
// dropped UPDATE looks like, and presence-only checks are blind to it.
func TestReconcile_DetectsStaleDocument(t *testing.T) {
	slot := &fakeColl{docs: []any{
		map[string]any{"_id": "a1", DocRevisionField: int64(2)}, // source says 5
	}}
	s, view := reconcileEngine(t, [][2]any{{"a1", 5}}, slot)

	rep, err := s.ReconcileView(context.Background(), view, ReconcileConfig{RowsPerSecond: -1})
	if err != nil {
		t.Fatalf("ReconcileView: %v", err)
	}
	if rep.Stale != 1 {
		t.Errorf("stale = %d, want 1 (stored revision 2 < source 5)", rep.Stale)
	}
	if rep.Missing != 0 {
		t.Errorf("missing = %d, want 0 — the document exists", rep.Missing)
	}
}

// TestReconcile_DocumentAheadOfSourceIsNotDivergence: the source page is a
// snapshot, so an event that landed after it read leaves the document AHEAD.
// Treating that as divergence would make every busy aggregate a false positive
// and the sweep would repair in a loop.
func TestReconcile_DocumentAheadOfSourceIsNotDivergence(t *testing.T) {
	slot := &fakeColl{docs: []any{
		map[string]any{"_id": "a1", DocRevisionField: int64(9)},
	}}
	s, view := reconcileEngine(t, [][2]any{{"a1", 5}}, slot)

	rep, err := s.ReconcileView(context.Background(), view, ReconcileConfig{RowsPerSecond: -1})
	if err != nil {
		t.Fatalf("ReconcileView: %v", err)
	}
	if rep.Diverged() != 0 {
		t.Errorf("a document ahead of the source snapshot is healthy, got missing=%d stale=%d", rep.Missing, rep.Stale)
	}
}

// TestReconcileAllViews_SweepsUnderTheLock: the deployment-wide entry point
// sweeps each distinct view once under its advisory lock and stamps the
// liveness clock — the signal §7 item 14 alarms on.
func TestReconcileAllViews_SweepsUnderTheLock(t *testing.T) {
	slot := &fakeColl{docs: []any{
		map[string]any{"_id": "a1", DocRevisionField: int64(5)},
	}}
	s, _ := reconcileEngine(t, [][2]any{{"a1", 5}}, slot)

	reports, err := s.ReconcileAllViews(context.Background(), ReconcileConfig{RowsPerSecond: -1})
	if err != nil {
		t.Fatalf("ReconcileAllViews: %v", err)
	}
	if len(reports) != 1 {
		t.Fatalf("reports = %d, want 1 (one distinct view)", len(reports))
	}
	if reports[0].Diverged() != 0 {
		t.Errorf("converged view must sweep clean, got missing=%d stale=%d", reports[0].Missing, reports[0].Stale)
	}
	if s.ProjectionHealth().LastReconcile.IsZero() {
		t.Error("a completed pass must stamp the reconcile liveness clock")
	}
}

// TestReconcileAllViews_LockHeldElsewhereSkipsQuietly: a lock owned by another
// pod means that pod is already sweeping — skipping is correct, erroring or
// double-sweeping is not.
func TestReconcileAllViews_LockHeldElsewhereSkipsQuietly(t *testing.T) {
	slot := &fakeColl{docs: []any{
		map[string]any{"_id": "a1", DocRevisionField: int64(5)},
	}}
	view := convRoleView()
	eng := newFakeEngine(&fakeQuerier{
		queryFn:     reconcileSource([][2]any{{"a1", 5}}),
		queryMapsFn: convComposerRows,
	})
	eng.lockHeld = true // another pod owns every advisory lock
	identityResolver.lease = 0
	s := NewSyncEngine(eng, newFakeMongo(slot), identityResolver, nil, "grp", []*ViewDefinition{view}, 1)

	reports, err := s.ReconcileAllViews(context.Background(), ReconcileConfig{RowsPerSecond: -1})
	if err != nil {
		t.Fatalf("a held lock is not an error: %v", err)
	}
	if len(reports) != 0 {
		t.Fatalf("a held lock must skip the view, got %d reports", len(reports))
	}
}

// TestRunReconcileLoop_PacesAndStops: the scheduled form runs passes on the
// end-to-start cadence, survives a failing pass (a backstop that dies on its
// first bad pass protects nothing), and exits with the context.
func TestRunReconcileLoop_PacesAndStops(t *testing.T) {
	slot := &fakeColl{docs: []any{
		map[string]any{"_id": "a1", DocRevisionField: int64(5)},
	}}
	s, _ := reconcileEngine(t, [][2]any{{"a1", 5}}, slot)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.RunReconcileLoop(ctx, 5*time.Millisecond, ReconcileConfig{RowsPerSecond: -1}); close(done) }()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !s.ProjectionHealth().LastReconcile.IsZero() {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	first := s.ProjectionHealth().LastReconcile
	if first.IsZero() {
		t.Fatal("the loop never completed a pass")
	}
	for time.Now().Before(deadline) {
		if s.ProjectionHealth().LastReconcile.After(first) {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !s.ProjectionHealth().LastReconcile.After(first) {
		t.Fatal("the loop never ran a SECOND pass — the cadence is broken")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the loop did not exit on context cancellation")
	}
}

// TestReconcile_RequiresARevisionColumn: parity is undefined without one, and
// degrading to a presence-only check would silently narrow what the sweep
// actually proves. It must refuse rather than under-report.
func TestReconcile_RequiresARevisionColumn(t *testing.T) {
	schema := core.NewTableSchema[*builderTestEntity]("norev").PK("id").Field("Email", "email")
	view := View("norev").Schema(schema).Version(1)
	eng := newFakeEngine(&fakeQuerier{})
	s := NewSyncEngine(eng, newFakeMongo(&fakeColl{}), identityResolver, nil, "grp", []*ViewDefinition{view}, 1)

	if _, err := s.ReconcileView(context.Background(), view, ReconcileConfig{RowsPerSecond: -1}); err == nil {
		t.Fatal("a root without Revision must fail loudly, not degrade to a presence check")
	}
}

// TestRateLimiter_Semantics: the sweep's ONLY cost knob. Negative = unthrottled
// (no sleep), zero = the framework default rate, positive paces rows/rate, and
// a cancelled context cuts the sleep short — the operator's Ctrl-C must not
// wait out a long batch pause.
func TestRateLimiter_Semantics(t *testing.T) {
	if got := newRateLimiter(0).perSecond; got != reconcileDefaultRowsPerSecond {
		t.Fatalf("zero must resolve to the framework default, got %d", got)
	}

	un := newRateLimiter(-1)
	start := time.Now()
	un.wait(context.Background(), 1_000_000)
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("a negative rate must not sleep at all")
	}

	paced := newRateLimiter(10) // 5 rows at 10/s → 500ms
	start = time.Now()
	paced.wait(context.Background(), 5)
	if e := time.Since(start); e < 300*time.Millisecond {
		t.Fatalf("the pace must be rows/rate, slept only %v", e)
	}

	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	start = time.Now()
	paced.wait(cctx, 1_000) // would be 100s — the dead context must cut it
	if time.Since(start) > time.Second {
		t.Fatal("a cancelled context must cut the pause short")
	}

	start = time.Now()
	paced.wait(context.Background(), 0) // nothing scanned → nothing to pace
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("zero rows must not sleep")
	}
}
