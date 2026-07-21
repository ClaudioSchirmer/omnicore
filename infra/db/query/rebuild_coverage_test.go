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

// scriptStore decorates fakeStore with a scriptable SnapshotDocumentIDs so a
// test can force the shadow snapshot (used by verify) to fail.
type scriptStore struct {
	*fakeStore
	snapshotErr error
}

func (s *scriptStore) SnapshotDocumentIDs(ctx context.Context, collection PhysicalCollection) (map[string]struct{}, error) {
	if s.snapshotErr != nil {
		return nil, s.snapshotErr
	}
	return s.fakeStore.SnapshotDocumentIDs(ctx, collection)
}

// rebuildQuerier scripts the three surfaces ExecuteRebuild touches: the
// registry Execs, the SELECT-id scan (ids), and the composer's batched
// QueryMaps. ComposeBatch fetches the whole batch of roots in one IN (...)
// lookup, so the fake returns a row for EVERY arg id that has a live root
// (rootDoc non-nil); a nil map simulates a hard-deleted root that the IN
// lookup simply does not return.
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
			var out []map[string]any
			for _, a := range args {
				id, _ := a.(string)
				if doc := rootDoc(id); doc != nil {
					out = append(out, doc)
				}
			}
			return out, nil
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
	identityResolver.lease = 0 // no fence/settle wait in unit tests
	return NewSyncEngine(eng, store, identityResolver, nil, "grp", views, 1)
}

func TestExecuteRebuild_BlueGreenSequence(t *testing.T) {
	view := rebuildView()
	// One fakeColl backs every physical name; the source has two live ids the
	// backfill composes into the shadow and verify then confirms.
	slot := &fakeColl{docs: []any{shapedDoc("id1"), shapedDoc("id2")}}
	store := &scriptStore{fakeStore: newFakeMongoFunc(func(string) *fakeColl { return slot })}
	eng := newScriptEngine([]string{"id1", "id2"}, aliveRoot)
	s := scriptSyncEngine(eng, store, []*ViewDefinition{view})

	plan := DriftPlan{View: view, CurrentVersion: 1, CurrentCombinedHash: "h"}
	if err := s.ExecuteRebuild(context.Background(), plan, RebuildConfig{Orphan: "delete", ServiceName: "svc"}); err != nil {
		t.Fatalf("ExecuteRebuild: %v", err)
	}
	// The inactive slot off the bare active (view__0) was provisioned and backfilled.
	if len(store.provisioned) != 1 || store.provisioned[0] != view.Name()+"__0" {
		t.Errorf("expected the shadow slot provisioned, got %v", store.provisioned)
	}
	if len(slot.updates) < 2 {
		t.Errorf("expected the shadow backfilled (>=2 upserts), got %d", len(slot.updates))
	}
	// The shadow slot is dropped BEFORE provisioning (clean by construction —
	// an unreclaimed retiree must never leak documents into the new build),
	// then, after the flip, the retired bare collection is reclaimed.
	if len(store.dropped) != 2 || store.dropped[0] != view.Name()+"__0" || store.dropped[1] != view.Name() {
		t.Errorf("expected [shadow pre-drop, retired reclaim], got %v", store.dropped)
	}
}

func TestExecuteRebuild_TakeoverResetsStaleShadow(t *testing.T) {
	view := rebuildView()
	staleShadow := view.Name() + "__1"
	store := &scriptStore{fakeStore: newFakeMongo(&fakeColl{})}
	s := scriptSyncEngine(newScriptEngine(nil, aliveRoot), store, []*ViewDefinition{view})
	plan := DriftPlan{View: view, Registry: &ViewRegistryRow{
		Status: ViewRegistryStatusProcessing, ShadowCollection: &staleShadow,
	}}
	if err := s.ExecuteRebuild(context.Background(), plan, RebuildConfig{Orphan: "delete"}); err != nil {
		t.Fatalf("takeover rebuild: %v", err)
	}
	found := false
	for _, d := range store.dropped {
		if d == staleShadow {
			found = true
		}
	}
	if !found {
		t.Errorf("expected the crashed driver's stale shadow %q dropped on takeover, got %v", staleShadow, store.dropped)
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
			t.Fatal("expected the BeginRebuild error")
		}
	})
	t.Run("provisionError", func(t *testing.T) {
		store := &scriptStore{fakeStore: newFakeMongo(&fakeColl{})}
		store.provisionErr = errFake
		s := scriptSyncEngine(newScriptEngine(nil, aliveRoot), store, []*ViewDefinition{view})
		if err := s.ExecuteRebuild(context.Background(), mkPlan(), RebuildConfig{Orphan: "delete"}); err == nil {
			t.Fatal("expected the ProvisionSlot error")
		}
	})
	t.Run("backfillUpsertError", func(t *testing.T) {
		slot := &fakeColl{updateErr: errFake}
		store := &scriptStore{fakeStore: newFakeMongo(slot)}
		s := scriptSyncEngine(newScriptEngine([]string{"id1"}, aliveRoot), store, []*ViewDefinition{view})
		if err := s.ExecuteRebuild(context.Background(), mkPlan(), RebuildConfig{Orphan: "delete"}); err == nil {
			t.Fatal("expected the backfill upsert error")
		}
		// An aborted backfill discards the half-built shadow (clears the dual-apply
		// flag + drops the collection) instead of leaving it live; it never flips,
		// so the bare active is never reclaimed. dropped == [pre-drop, discard]
		// (both on the shadow) proves it.
		if len(store.dropped) != 2 || store.dropped[0] != view.Name()+"__0" || store.dropped[1] != view.Name()+"__0" {
			t.Errorf("expected the shadow pre-dropped and discarded on backfill abort, dropped=%v", store.dropped)
		}
	})
	t.Run("verifyError", func(t *testing.T) {
		// A shadow snapshot failure fails verify; the shadow is discarded.
		store := &scriptStore{fakeStore: newFakeMongo(&fakeColl{}), snapshotErr: errFake}
		s := scriptSyncEngine(newScriptEngine([]string{"id1"}, aliveRoot), store, []*ViewDefinition{view})
		if err := s.ExecuteRebuild(context.Background(), mkPlan(), RebuildConfig{Orphan: "delete"}); err == nil {
			t.Fatal("expected the verify error")
		}
	})
	t.Run("endRebuildExecError", func(t *testing.T) {
		// Begin(1), beginSlot(2), flip(3) pass; EndRebuild(4) fails.
		calls := 0
		q := rebuildQuerier([]string{"id1"}, aliveRoot)
		q.execFn = func(string, []any) error {
			calls++
			if calls >= 4 {
				return errFake
			}
			return nil
		}
		slot := &fakeColl{docs: []any{shapedDoc("id1")}}
		s := scriptSyncEngine(newFakeEngine(q), &scriptStore{fakeStore: newFakeMongo(slot)}, []*ViewDefinition{view})
		if err := s.ExecuteRebuild(context.Background(), mkPlan(), RebuildConfig{Orphan: "delete"}); err == nil {
			t.Fatal("expected the EndRebuild error")
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
