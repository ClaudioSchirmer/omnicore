package query

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// Coverage for the unit-reachable rebuild body: the full ExecuteRebuild
// sequence (status writes, orphan-field cleanup, compose+upsert loop, orphan
// reconciliation), the operator rebuild paths (RebuildView/Since/All), the
// pure orphan helpers, and DerivedIndexName. Everything runs on the package's
// fakeEngine/fakeStore seams — no live backend.

// scriptStore decorates fakeStore with a scriptable ObservedFieldNames /
// UnsetFields (the base fake hardwires them to empty/no-op).
type scriptStore struct {
	*fakeStore
	observed    map[string]struct{}
	observedErr error
	unsetErr    error
	unsetCalls  [][]string
	snapshotErr error
}

func (s *scriptStore) ObservedFieldNames(ctx context.Context, collection string) (map[string]struct{}, error) {
	if s.observedErr != nil {
		return nil, s.observedErr
	}
	if s.observed != nil {
		return s.observed, nil
	}
	return map[string]struct{}{}, nil
}

func (s *scriptStore) UnsetFields(_ context.Context, _ string, fields []string) error {
	if s.unsetErr != nil {
		return s.unsetErr
	}
	s.unsetCalls = append(s.unsetCalls, fields)
	return nil
}

func (s *scriptStore) SnapshotDocumentIDs(ctx context.Context, collection string) (map[string]struct{}, error) {
	if s.snapshotErr != nil {
		return nil, s.snapshotErr
	}
	return s.fakeStore.SnapshotDocumentIDs(ctx, collection)
}

// rebuildQuerier scripts the three surfaces ExecuteRebuild touches: the
// registry Execs, the SELECT-id scan (ids), and the composer's per-id
// QueryMaps (rootDoc by id; a nil map simulates a hard-deleted root).
func rebuildQuerier(ids []string, rootDoc func(id string) map[string]any) *fakeQuerier {
	return &fakeQuerier{
		queryFn: func(sql string, _ []any) (core.Rows, error) {
			return &fakeRows{rows: len(ids), scan: func(idx int, dest []any) error {
				if p, ok := dest[0].(*string); ok {
					*p = ids[idx]
				}
				return nil
			}}, nil
		},
		queryMapsFn: func(_ string, args []any) ([]map[string]any, error) {
			if len(args) == 0 {
				return nil, nil
			}
			id, _ := args[0].(string)
			if doc := rootDoc(id); doc != nil {
				return []map[string]any{doc}, nil
			}
			return nil, nil
		},
	}
}

func aliveRoot(id string) map[string]any {
	return map[string]any{"id": id, "name": "n-" + id, "deleted_at": nil}
}

func newScriptEngine(ids []string, rootDoc func(string) map[string]any) *fakeEngine {
	return newFakeEngine(rebuildQuerier(ids, rootDoc))
}

func scriptSyncEngine(eng core.RelationalEngine, store ReadModelStore, views []*ViewDefinition) *SyncEngine {
	return NewSyncEngine(eng, store, nil, "grp", views, 1)
}

func TestExecuteRebuild_FullSequence(t *testing.T) {
	view := rebuildView()
	// Mongo carries id1 (still alive) and ghost (no relational source) plus an
	// orphan field left behind by an older projection shape.
	coll := &fakeColl{count: 2, docs: []any{
		map[string]any{"_id": "id1"},
		map[string]any{"_id": "ghost"},
	}}
	store := &scriptStore{
		fakeStore: newFakeMongo(coll),
		observed:  map[string]struct{}{"_id": {}, "name": {}, "legacy_field": {}},
	}
	eng := newScriptEngine([]string{"id1", "id2"}, aliveRoot)
	s := scriptSyncEngine(eng, store, []*ViewDefinition{view})

	plan := DriftPlan{View: view, CurrentVersion: 1, CurrentCombinedHash: "h"}
	if err := s.ExecuteRebuild(context.Background(), plan, RebuildConfig{Orphan: "delete", ServiceName: "svc"}); err != nil {
		t.Fatalf("ExecuteRebuild: %v", err)
	}
	// Orphan field cleanup ran on the stray key only.
	if len(store.unsetCalls) != 1 || len(store.unsetCalls[0]) != 1 || store.unsetCalls[0][0] != "legacy_field" {
		t.Errorf("expected legacy_field unset, got %v", store.unsetCalls)
	}
	// Both live ids were composed+upserted.
	if len(coll.updates) != 2 {
		t.Errorf("expected 2 upserts, got %d", len(coll.updates))
	}
	// The ghost doc was reconciled away (delete mode).
	if len(coll.deletes) != 1 || coll.deletes[0] != "ghost" {
		t.Errorf("expected the ghost orphan deleted, got %v", coll.deletes)
	}
}

func TestExecuteRebuild_TakeoverWarnsAndProceeds(t *testing.T) {
	view := rebuildView()
	pid, host := "41", "old-pod"
	for _, reg := range []*ViewRegistryRow{
		{Status: ViewRegistryStatusProcessing}, // nil PID/Host/StartedAt → "<unknown>" branches
		{Status: ViewRegistryStatusProcessing, PID: &pid, Host: &host},
	} {
		coll := &fakeColl{}
		s := scriptSyncEngine(newScriptEngine(nil, aliveRoot), &scriptStore{fakeStore: newFakeMongo(coll)}, []*ViewDefinition{view})
		plan := DriftPlan{View: view, Registry: reg}
		if err := s.ExecuteRebuild(context.Background(), plan, RebuildConfig{Orphan: "warn"}); err != nil {
			t.Fatalf("takeover rebuild: %v", err)
		}
	}
}

func TestExecuteRebuild_StepErrors(t *testing.T) {
	view := rebuildView()
	mkPlan := func() DriftPlan { return DriftPlan{View: view} }

	t.Run("beginRebuildExecError", func(t *testing.T) {
		q := rebuildQuerier(nil, aliveRoot)
		q.execFn = func(string, []any) error { return errFake }
		s := scriptSyncEngine(newFakeEngine(q), &scriptStore{fakeStore: newFakeMongo(&fakeColl{})}, []*ViewDefinition{view})
		if err := s.ExecuteRebuild(context.Background(), mkPlan(), RebuildConfig{Orphan: "delete"}); err == nil {
			t.Fatal("expected the registry write error")
		}
	})
	t.Run("hasDocumentsError", func(t *testing.T) {
		coll := &fakeColl{countErr: errFake}
		s := scriptSyncEngine(newScriptEngine(nil, aliveRoot), &scriptStore{fakeStore: newFakeMongo(coll)}, []*ViewDefinition{view})
		if err := s.ExecuteRebuild(context.Background(), mkPlan(), RebuildConfig{Orphan: "delete"}); err == nil {
			t.Fatal("expected the HasDocuments error")
		}
	})
	t.Run("observedFieldsError", func(t *testing.T) {
		store := &scriptStore{fakeStore: newFakeMongo(&fakeColl{count: 1}), observedErr: errFake}
		s := scriptSyncEngine(newScriptEngine(nil, aliveRoot), store, []*ViewDefinition{view})
		if err := s.ExecuteRebuild(context.Background(), mkPlan(), RebuildConfig{Orphan: "delete"}); err == nil {
			t.Fatal("expected the ObservedFieldNames error")
		}
	})
	t.Run("unsetFieldsError", func(t *testing.T) {
		store := &scriptStore{
			fakeStore: newFakeMongo(&fakeColl{count: 1}),
			observed:  map[string]struct{}{"stray": {}},
			unsetErr:  errFake,
		}
		s := scriptSyncEngine(newScriptEngine([]string{"id1"}, aliveRoot), store, []*ViewDefinition{view})
		if err := s.ExecuteRebuild(context.Background(), mkPlan(), RebuildConfig{Orphan: "delete"}); err == nil {
			t.Fatal("expected the UnsetFields error")
		}
	})
	t.Run("snapshotError", func(t *testing.T) {
		store := &scriptStore{fakeStore: newFakeMongo(&fakeColl{}), snapshotErr: errFake}
		s := scriptSyncEngine(newScriptEngine(nil, aliveRoot), store, []*ViewDefinition{view})
		if err := s.ExecuteRebuild(context.Background(), mkPlan(), RebuildConfig{Orphan: "delete"}); err == nil {
			t.Fatal("expected the snapshot error")
		}
	})
	t.Run("idScanQueryError", func(t *testing.T) {
		q := rebuildQuerier(nil, aliveRoot)
		q.queryFn = func(string, []any) (core.Rows, error) { return nil, errFake }
		s := scriptSyncEngine(newFakeEngine(q), &scriptStore{fakeStore: newFakeMongo(&fakeColl{})}, []*ViewDefinition{view})
		if err := s.ExecuteRebuild(context.Background(), mkPlan(), RebuildConfig{Orphan: "delete"}); err == nil {
			t.Fatal("expected the id-scan error")
		}
	})
	t.Run("idScanRowError", func(t *testing.T) {
		q := rebuildQuerier(nil, aliveRoot)
		q.queryFn = func(string, []any) (core.Rows, error) {
			return &fakeRows{rows: 1, scan: func(int, []any) error { return errFake }}, nil
		}
		s := scriptSyncEngine(newFakeEngine(q), &scriptStore{fakeStore: newFakeMongo(&fakeColl{})}, []*ViewDefinition{view})
		if err := s.ExecuteRebuild(context.Background(), mkPlan(), RebuildConfig{Orphan: "delete"}); err == nil {
			t.Fatal("expected the row scan error")
		}
	})
	t.Run("idScanErrAfterIteration", func(t *testing.T) {
		q := rebuildQuerier(nil, aliveRoot)
		q.queryFn = func(string, []any) (core.Rows, error) {
			return &fakeRows{rows: 0, nextErr: errFake}, nil
		}
		s := scriptSyncEngine(newFakeEngine(q), &scriptStore{fakeStore: newFakeMongo(&fakeColl{})}, []*ViewDefinition{view})
		if err := s.ExecuteRebuild(context.Background(), mkPlan(), RebuildConfig{Orphan: "delete"}); err == nil {
			t.Fatal("expected the rows.Err error")
		}
	})
	t.Run("composeError", func(t *testing.T) {
		q := rebuildQuerier([]string{"id1"}, aliveRoot)
		q.queryMapsFn = func(string, []any) ([]map[string]any, error) { return nil, errFake }
		s := scriptSyncEngine(newFakeEngine(q), &scriptStore{fakeStore: newFakeMongo(&fakeColl{})}, []*ViewDefinition{view})
		if err := s.ExecuteRebuild(context.Background(), mkPlan(), RebuildConfig{Orphan: "delete"}); err == nil {
			t.Fatal("expected the compose error")
		}
	})
	t.Run("upsertError", func(t *testing.T) {
		coll := &fakeColl{updateErr: errFake}
		s := scriptSyncEngine(newScriptEngine([]string{"id1"}, aliveRoot), &scriptStore{fakeStore: newFakeMongo(coll)}, []*ViewDefinition{view})
		if err := s.ExecuteRebuild(context.Background(), mkPlan(), RebuildConfig{Orphan: "delete"}); err == nil {
			t.Fatal("expected the upsert error")
		}
	})
	t.Run("orphanDeleteError", func(t *testing.T) {
		coll := &fakeColl{deleteErr: errFake, docs: []any{map[string]any{"_id": "ghost"}}}
		s := scriptSyncEngine(newScriptEngine(nil, aliveRoot), &scriptStore{fakeStore: newFakeMongo(coll)}, []*ViewDefinition{view})
		if err := s.ExecuteRebuild(context.Background(), mkPlan(), RebuildConfig{Orphan: "delete"}); err == nil {
			t.Fatal("expected the orphan delete error")
		}
	})
	t.Run("endRebuildExecError", func(t *testing.T) {
		calls := 0
		q := rebuildQuerier(nil, aliveRoot)
		q.execFn = func(string, []any) error {
			calls++
			if calls > 1 { // BeginRebuild passes, EndRebuild fails
				return errFake
			}
			return nil
		}
		s := scriptSyncEngine(newFakeEngine(q), &scriptStore{fakeStore: newFakeMongo(&fakeColl{})}, []*ViewDefinition{view})
		if err := s.ExecuteRebuild(context.Background(), mkPlan(), RebuildConfig{Orphan: "delete"}); err == nil {
			t.Fatal("expected the EndRebuild error")
		}
	})
}

// ─── computeOrphanFields / reconcileOrphans (direct) ─────────────────────────

func TestComputeOrphanFields(t *testing.T) {
	view := rebuildView()
	mkComposer := func(ids []string, rootDoc func(string) map[string]any) *Composer {
		return NewComposer(newScriptEngine(ids, rootDoc))
	}

	t.Run("straySurfaces", func(t *testing.T) {
		store := &scriptStore{
			fakeStore: newFakeMongo(&fakeColl{}),
			observed:  map[string]struct{}{"_id": {}, "name": {}, "stray": {}},
		}
		orphan, err := computeOrphanFields(context.Background(), store, mkComposer([]string{"s1"}, aliveRoot), view)
		if err != nil {
			t.Fatalf("computeOrphanFields: %v", err)
		}
		if len(orphan) != 1 || orphan[0] != "stray" {
			t.Errorf("orphans = %v, want [stray]", orphan)
		}
	})
	t.Run("nothingObserved", func(t *testing.T) {
		store := &scriptStore{fakeStore: newFakeMongo(&fakeColl{})}
		orphan, err := computeOrphanFields(context.Background(), store, mkComposer(nil, aliveRoot), view)
		if err != nil || orphan != nil {
			t.Fatalf("empty collection: got %v, %v", orphan, err)
		}
	})
	t.Run("emptyRelationalSkipsCleanup", func(t *testing.T) {
		store := &scriptStore{
			fakeStore: newFakeMongo(&fakeColl{}),
			observed:  map[string]struct{}{"anything": {}},
		}
		orphan, err := computeOrphanFields(context.Background(), store, mkComposer(nil, aliveRoot), view)
		if err != nil || orphan != nil {
			t.Fatalf("empty SoR: got %v, %v", orphan, err)
		}
	})
	t.Run("sampleQueryError", func(t *testing.T) {
		store := &scriptStore{
			fakeStore: newFakeMongo(&fakeColl{}),
			observed:  map[string]struct{}{"x": {}},
		}
		q := &fakeQuerier{queryFn: func(string, []any) (core.Rows, error) { return nil, errFake }}
		if _, err := computeOrphanFields(context.Background(), store, NewComposer(newFakeEngine(q)), view); err == nil {
			t.Fatal("expected the sample query error")
		}
	})
	t.Run("composeSampleError", func(t *testing.T) {
		store := &scriptStore{
			fakeStore: newFakeMongo(&fakeColl{}),
			observed:  map[string]struct{}{"x": {}},
		}
		q := rebuildQuerier([]string{"s1"}, aliveRoot)
		q.queryMapsFn = func(string, []any) ([]map[string]any, error) { return nil, errFake }
		if _, err := computeOrphanFields(context.Background(), store, NewComposer(newFakeEngine(q)), view); err == nil {
			t.Fatal("expected the compose error")
		}
	})
	t.Run("viewWithoutSchemaErrors", func(t *testing.T) {
		store := &scriptStore{
			fakeStore: newFakeMongo(&fakeColl{}),
			observed:  map[string]struct{}{"x": {}},
		}
		bare := View("bare").Version(1).Root("bare")
		if _, err := computeOrphanFields(context.Background(), store, mkComposer([]string{"s1"}, aliveRoot), bare); err == nil ||
			!strings.Contains(err.Error(), "no root .Schema") {
			t.Fatalf("expected the missing-schema error, got %v", err)
		}
	})
}

func TestReconcileOrphans_Direct(t *testing.T) {
	snapshot := map[string]struct{}{"a": {}, "b": {}}

	t.Run("emptySnapshotNoop", func(t *testing.T) {
		n, err := reconcileOrphans(context.Background(), newFakeMongo(&fakeColl{}), "v", nil, "delete")
		if err != nil || n != 0 {
			t.Fatalf("empty snapshot: %d, %v", n, err)
		}
	})
	t.Run("deleteMode", func(t *testing.T) {
		coll := &fakeColl{}
		n, err := reconcileOrphans(context.Background(), newFakeMongo(coll), "v", snapshot, "delete")
		if err != nil || n != 2 {
			t.Fatalf("delete mode: %d, %v", n, err)
		}
	})
	t.Run("warnMode", func(t *testing.T) {
		coll := &fakeColl{}
		n, err := reconcileOrphans(context.Background(), newFakeMongo(coll), "v", snapshot, "warn")
		if err != nil || n != 0 {
			t.Fatalf("warn mode: %d, %v", n, err)
		}
		if len(coll.deletes) != 0 {
			t.Errorf("warn mode must not delete, got %v", coll.deletes)
		}
	})
	t.Run("invalidMode", func(t *testing.T) {
		if _, err := reconcileOrphans(context.Background(), newFakeMongo(&fakeColl{}), "v", snapshot, "bogus"); err == nil {
			t.Fatal("expected the invalid-mode error")
		}
	})
	t.Run("deleteError", func(t *testing.T) {
		coll := &fakeColl{deleteErr: errFake}
		if _, err := reconcileOrphans(context.Background(), newFakeMongo(coll), "v", snapshot, "delete"); err == nil {
			t.Fatal("expected the delete error")
		}
	})
}

// ─── operator rebuild paths ──────────────────────────────────────────────────

func TestRebuildFromTable_PathsAndErrors(t *testing.T) {
	view := rebuildView()

	t.Run("fullRebuildUpserts", func(t *testing.T) {
		coll := &fakeColl{}
		s := scriptSyncEngine(newScriptEngine([]string{"id1", "id2"}, aliveRoot), newFakeMongo(coll), []*ViewDefinition{view})
		if err := s.RebuildView(context.Background(), view); err != nil {
			t.Fatalf("RebuildView: %v", err)
		}
		if len(coll.updates) != 2 {
			t.Errorf("expected 2 upserts, got %d", len(coll.updates))
		}
	})
	t.Run("rootGoneSkipsDoc", func(t *testing.T) {
		coll := &fakeColl{}
		s := scriptSyncEngine(newScriptEngine([]string{"id1"}, func(string) map[string]any { return nil }), newFakeMongo(coll), []*ViewDefinition{view})
		if err := s.RebuildView(context.Background(), view); err != nil {
			t.Fatalf("RebuildView: %v", err)
		}
		if len(coll.updates) != 0 {
			t.Errorf("a nil compose must skip the upsert, got %d", len(coll.updates))
		}
	})
	t.Run("sinceUsesUpdatedAt", func(t *testing.T) {
		coll := &fakeColl{}
		s := scriptSyncEngine(newScriptEngine([]string{"id1"}, aliveRoot), newFakeMongo(coll), []*ViewDefinition{view})
		if err := s.RebuildViewSince(context.Background(), view, time.Unix(1700000000, 0)); err != nil {
			t.Fatalf("RebuildViewSince: %v", err)
		}
		if len(coll.updates) != 1 {
			t.Errorf("expected 1 upsert, got %d", len(coll.updates))
		}
	})
	t.Run("sinceWithoutUpdatedAtErrors", func(t *testing.T) {
		bare := View("orders").Version(1).Root("orders").Schema(composerRootSchema()) // no UpdatedAt
		s := scriptSyncEngine(newScriptEngine(nil, aliveRoot), newFakeMongo(&fakeColl{}), []*ViewDefinition{bare})
		if err := s.RebuildViewSince(context.Background(), bare, time.Unix(1700000000, 0)); err == nil ||
			!strings.Contains(err.Error(), "no UpdatedAt") {
			t.Fatalf("expected the missing-UpdatedAt error, got %v", err)
		}
	})
	t.Run("viewWithoutSchemaErrors", func(t *testing.T) {
		bare := View("bare").Version(1).Root("bare")
		s := scriptSyncEngine(newScriptEngine(nil, aliveRoot), newFakeMongo(&fakeColl{}), []*ViewDefinition{bare})
		if err := s.RebuildView(context.Background(), bare); err == nil ||
			!strings.Contains(err.Error(), "no root .Schema") {
			t.Fatalf("expected the missing-schema error, got %v", err)
		}
	})
	t.Run("queryError", func(t *testing.T) {
		q := &fakeQuerier{queryFn: func(string, []any) (core.Rows, error) { return nil, errFake }}
		s := scriptSyncEngine(newFakeEngine(q), newFakeMongo(&fakeColl{}), []*ViewDefinition{view})
		if err := s.RebuildView(context.Background(), view); err == nil {
			t.Fatal("expected the scan query error")
		}
	})
	t.Run("scanError", func(t *testing.T) {
		q := &fakeQuerier{queryFn: func(string, []any) (core.Rows, error) {
			return &fakeRows{rows: 1, scan: func(int, []any) error { return errFake }}, nil
		}}
		s := scriptSyncEngine(newFakeEngine(q), newFakeMongo(&fakeColl{}), []*ViewDefinition{view})
		if err := s.RebuildView(context.Background(), view); err == nil {
			t.Fatal("expected the scan error")
		}
	})
	t.Run("rowsErr", func(t *testing.T) {
		q := &fakeQuerier{queryFn: func(string, []any) (core.Rows, error) {
			return &fakeRows{rows: 0, nextErr: errFake}, nil
		}}
		s := scriptSyncEngine(newFakeEngine(q), newFakeMongo(&fakeColl{}), []*ViewDefinition{view})
		if err := s.RebuildView(context.Background(), view); err == nil {
			t.Fatal("expected the rows.Err error")
		}
	})
	t.Run("composeError", func(t *testing.T) {
		q := rebuildQuerier([]string{"id1"}, aliveRoot)
		q.queryMapsFn = func(string, []any) ([]map[string]any, error) { return nil, errFake }
		s := scriptSyncEngine(newFakeEngine(q), newFakeMongo(&fakeColl{}), []*ViewDefinition{view})
		if err := s.RebuildView(context.Background(), view); err == nil {
			t.Fatal("expected the compose error")
		}
	})
	t.Run("upsertError", func(t *testing.T) {
		coll := &fakeColl{updateErr: errFake}
		s := scriptSyncEngine(newScriptEngine([]string{"id1"}, aliveRoot), newFakeMongo(coll), []*ViewDefinition{view})
		if err := s.RebuildView(context.Background(), view); err == nil {
			t.Fatal("expected the upsert error")
		}
	})
}

func TestRebuildAllViews(t *testing.T) {
	v := rebuildView()

	t.Run("walksIndexOnce", func(t *testing.T) {
		coll := &fakeColl{}
		// The same view is indexed under byPGTable AND byMongoColl — the seen
		// map must keep the rebuild single.
		s := scriptSyncEngine(newScriptEngine([]string{"id1"}, aliveRoot), newFakeMongo(coll), []*ViewDefinition{v})
		if err := s.RebuildAllViews(context.Background()); err != nil {
			t.Fatalf("RebuildAllViews: %v", err)
		}
		if len(coll.updates) != 1 {
			t.Errorf("the view must rebuild exactly once, got %d upserts", len(coll.updates))
		}
	})
	t.Run("propagatesError", func(t *testing.T) {
		q := &fakeQuerier{queryFn: func(string, []any) (core.Rows, error) { return nil, errFake }}
		s := scriptSyncEngine(newFakeEngine(q), newFakeMongo(&fakeColl{}), []*ViewDefinition{v})
		if err := s.RebuildAllViews(context.Background()); err == nil {
			t.Fatal("expected the rebuild error to propagate")
		}
	})
}

// ─── small pure surfaces ─────────────────────────────────────────────────────

func TestViewMaxLimit(t *testing.T) {
	v := View("orders").Version(1).Root("orders").MaxLimit(25)
	if got := v.MaxLimitValue(); got != 25 {
		t.Errorf("MaxLimitValue = %d, want 25", got)
	}
}

func TestValidIdentifier_PanicsOnUnsafe(t *testing.T) {
	if got := validIdentifier("safe_name"); got != "safe_name" {
		t.Errorf("validIdentifier = %q", got)
	}
	assertPanics(t, "unsafe identifier", func() { _ = validIdentifier("1; DROP TABLE x") })
}

// ─── DerivedIndexName ────────────────────────────────────────────────────────

func TestDerivedIndexName(t *testing.T) {
	explicit := &IndexSpec{name: "custom_idx", Keys: []IndexKey{{Field: "a", Order: IndexOrderAsc}}}
	if got := explicit.DerivedIndexName(); got != "custom_idx" {
		t.Errorf("explicit name = %q", got)
	}
	spec := &IndexSpec{Keys: []IndexKey{
		{Field: "a", Order: IndexOrderAsc},
		{Field: "b", Order: IndexOrderDesc},
		{Field: "c", Order: IndexOrderText},
		{Field: "d", Order: IndexOrderGeo2D},
		{Field: "e", Order: IndexOrderGeo2DSph},
		{Field: "f", Order: IndexOrderHashed},
	}}
	want := "a_1_b_-1_c_text_d_2d_e_2dsphere_f_hashed"
	if got := spec.DerivedIndexName(); got != want {
		t.Errorf("derived = %q, want %q", got, want)
	}
}
