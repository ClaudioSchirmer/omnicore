package query

import (
	"context"
	"testing"
)

// Fix #3: the operator-triggered reconcile/rebuild methods and the shared-base
// fan-out paths must ALSO skip a relational view. The skip previously lived only
// in the ReconcileAllViews loop and the event-driven paths, leaving these
// fundamental/public methods able to (mis)materialize the Mongo collection a
// relational view forbids. noopRelReader panics if the SoR load is ever reached,
// and fakeColl records every upsert/delete — either is the failure.

func TestReconcileView_SkipsRelationalView(t *testing.T) {
	coll := &fakeColl{}
	rel := relationalView("t")
	s := NewSyncEngine(processEngineWithRow(), newFakeMongo(coll), identityResolver, nil, "", []*ViewDefinition{rel}, 1)

	report, err := s.ReconcileView(context.Background(), rel, ReconcileConfig{})
	if err != nil {
		t.Fatalf("ReconcileView: %v", err)
	}
	if report.Scanned != 0 || report.Missing != 0 || report.Repaired != 0 {
		t.Fatalf("relational view must not be scanned/repaired, got %+v", report)
	}
	if len(coll.updates) != 0 || len(coll.deletes) != 0 {
		t.Fatalf("ReconcileView must not materialize a relational view, got %d upserts %d deletes",
			len(coll.updates), len(coll.deletes))
	}
}

func TestRebuildView_And_RebuildAll_SkipRelationalView(t *testing.T) {
	coll := &fakeColl{}
	rel := relationalView("t")
	s := NewSyncEngine(processEngineWithRow(), newFakeMongo(coll), identityResolver, nil, "", []*ViewDefinition{rel}, 1)

	if err := s.RebuildView(context.Background(), rel); err != nil {
		t.Fatalf("RebuildView: %v", err)
	}
	if err := s.RebuildAllViews(context.Background()); err != nil {
		t.Fatalf("RebuildAllViews: %v", err)
	}
	if len(coll.updates) != 0 || len(coll.deletes) != 0 {
		t.Fatalf("rebuild must not materialize a relational view, got %d upserts %d deletes",
			len(coll.updates), len(coll.deletes))
	}
}

func TestFanOutSharedBase_SkipsRelationalView(t *testing.T) {
	coll := &fakeColl{}
	rel := relationalView("t")
	s := NewSyncEngine(processEngineWithRow(), newFakeMongo(coll), identityResolver, nil, "", []*ViewDefinition{rel}, 1)

	if err := s.fanOutSharedBase(context.Background(), "b1", []*ViewDefinition{rel}); err != nil {
		t.Fatalf("fanOutSharedBase: %v", err)
	}
	if err := s.fanOutSharedBasePayload(context.Background(), map[string]any{}, "b1", []*ViewDefinition{rel}); err != nil {
		t.Fatalf("fanOutSharedBasePayload: %v", err)
	}
	if len(coll.updates) != 0 || len(coll.deletes) != 0 {
		t.Fatalf("shared-base fan-out must not materialize a relational view, got %d upserts %d deletes",
			len(coll.updates), len(coll.deletes))
	}
}
