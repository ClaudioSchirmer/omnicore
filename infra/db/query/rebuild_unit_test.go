package query

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// This file covers the unit-reachable branches of rebuild.go. The control plane
// now reaches the relational backend purely through the engine seam — the lock
// via AcquireRebuildLock, the registry writes via the lock's pinned-session
// Querier, the SELECT-id reads via the engine Querier — so a fakeEngine drives
// every path without a live database. The live compose+upsert body and Kafka
// Start/run remain integration-only.

func rebuildSyncEngine(eng core.RelationalEngine, coll *fakeColl, views []*ViewDefinition) *SyncEngine {
	return NewSyncEngine(eng, newFakeMongo(coll), identityResolver, nil, "grp", views, 1)
}

func rebuildView() *ViewDefinition {
	// Declare the managed timestamp columns: the incremental rebuild (since != "")
	// scans/orders on the schema's UpdatedAt column, so a root that supports it must
	// declare it (a root table without updated_at can only be rebuilt in full).
	return View("orders").Version(1).Schema(
		composerRootSchema().CreatedAt("created_at").UpdatedAt("updated_at"))
}

// emptyRowsEngine is a fakeEngine whose SELECT-id Query yields zero rows (so the
// compose+upsert loop is empty) and whose registry Exec succeeds.
func emptyRowsEngine() *fakeEngine {
	return newFakeEngine(&fakeQuerier{
		queryFn: func(string, []any) (core.Rows, error) { return &fakeRows{rows: 0}, nil },
	})
}

func TestExecuteRebuild_InvalidOrphan(t *testing.T) {
	s := rebuildSyncEngine(emptyRowsEngine(), &fakeColl{}, []*ViewDefinition{rebuildView()})
	plan := DriftPlan{View: rebuildView()}
	err := s.ExecuteRebuild(context.Background(), plan, RebuildConfig{Orphan: "bogus"})
	if err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("expected invalid-orphan error, got %v", err)
	}
}

func TestExecuteRebuild_AcquireError(t *testing.T) {
	// AcquireRebuildLock fails → ExecuteRebuild surfaces the wrapped error before
	// any rebuild work.
	eng := emptyRowsEngine()
	eng.acquireErr = errFake
	s := rebuildSyncEngine(eng, &fakeColl{}, []*ViewDefinition{rebuildView()})
	plan := DriftPlan{View: rebuildView()}
	err := s.ExecuteRebuild(context.Background(), plan, RebuildConfig{Orphan: "delete"})
	if err == nil || !strings.Contains(err.Error(), "acquire rebuild lock") {
		t.Fatalf("expected acquire rebuild lock error, got %v", err)
	}
}

func TestExecuteRebuild_LockHeldByOther(t *testing.T) {
	// The lock is held elsewhere (Acquired()==false) → ExecuteRebuild aborts with
	// a "held by" diagnostic naming the holder.
	eng := emptyRowsEngine()
	eng.lockHeld = true
	eng.lockHolder = "pid=42 application=svc"
	s := rebuildSyncEngine(eng, &fakeColl{}, []*ViewDefinition{rebuildView()})
	plan := DriftPlan{View: rebuildView()}
	err := s.ExecuteRebuild(context.Background(), plan, RebuildConfig{Orphan: "delete"})
	if err == nil || !strings.Contains(err.Error(), "held by") || !strings.Contains(err.Error(), "pid=42") {
		t.Fatalf("expected lock-held error naming the holder, got %v", err)
	}
}

func TestRegistryCombinedOrNone_Unit(t *testing.T) {
	if got := registryCombinedOrNone(nil); got != "<none>" {
		t.Errorf("nil registry = %q, want <none>", got)
	}
	row := &ViewRegistryRow{CombinedHash: "abc123"}
	if got := registryCombinedOrNone(row); got != "abc123" {
		t.Errorf("registry hash = %q, want abc123", got)
	}
}

func TestRebuildView_EmptyTable(t *testing.T) {
	s := rebuildSyncEngine(emptyRowsEngine(), &fakeColl{}, []*ViewDefinition{rebuildView()})
	if err := s.RebuildView(context.Background(), rebuildView()); err != nil {
		t.Fatalf("RebuildView: %v", err)
	}
}

func TestRebuildViewSince_EmptyTable(t *testing.T) {
	s := rebuildSyncEngine(emptyRowsEngine(), &fakeColl{}, []*ViewDefinition{rebuildView()})
	if err := s.RebuildViewSince(context.Background(), rebuildView(), time.Now()); err != nil {
		t.Fatalf("RebuildViewSince: %v", err)
	}
}

func TestRebuildFromTable_QueryError(t *testing.T) {
	eng := newFakeEngine(&fakeQuerier{
		queryFn: func(string, []any) (core.Rows, error) { return nil, errFake },
	})
	s := rebuildSyncEngine(eng, &fakeColl{}, []*ViewDefinition{rebuildView()})
	if err := s.RebuildView(context.Background(), rebuildView()); err == nil {
		t.Fatal("expected query error from rebuildFromTable")
	}
}

func TestInitRegistryOnly(t *testing.T) {
	// InitViewRegistry runs against the engine Querier's Exec (fake → success).
	s := rebuildSyncEngine(emptyRowsEngine(), &fakeColl{}, []*ViewDefinition{rebuildView()})
	plan := DriftPlan{
		View:                rebuildView(),
		CurrentVersion:      1,
		CurrentRebuildHash:  "rh",
		CurrentArtifactHash: "ah",
		CurrentCombinedHash: "ch",
	}
	if err := s.InitRegistryOnly(context.Background(), plan, "svc"); err != nil {
		t.Fatalf("InitRegistryOnly: %v", err)
	}
}

func TestRefreshRegistryArtifactOnly_Unit(t *testing.T) {
	// EndRebuild runs against the engine Querier's Exec (fake → success).
	s := rebuildSyncEngine(emptyRowsEngine(), &fakeColl{}, []*ViewDefinition{rebuildView()})
	plan := DriftPlan{
		View:                rebuildView(),
		CurrentVersion:      2,
		CurrentRebuildHash:  "rh",
		CurrentArtifactHash: "ah2",
		CurrentCombinedHash: "ch2",
	}
	if err := s.RefreshRegistryArtifactOnly(context.Background(), plan, "svc"); err != nil {
		t.Fatalf("RefreshRegistryArtifactOnly: %v", err)
	}
}

func TestInitRegistryOnly_ExecError(t *testing.T) {
	eng := newFakeEngine(&fakeQuerier{
		execFn: func(string, []any) error { return errFake },
	})
	s := rebuildSyncEngine(eng, &fakeColl{}, []*ViewDefinition{rebuildView()})
	plan := DriftPlan{View: rebuildView(), CurrentVersion: 1}
	if err := s.InitRegistryOnly(context.Background(), plan, "svc"); err == nil {
		t.Fatal("expected exec error from InitRegistryOnly")
	}
}

func TestRebuildAllViews_EmptyTables(t *testing.T) {
	// A view with a PG root + an external Mongo embed so both byPGTable and
	// byMongoColl index buckets are walked.
	external := JoinUpstream(core.NewExternalSchema("buyers").PK("id"), "Buyers", "buyers")
	v := View("orders").Version(1).Schema(composerRootSchema()).
		EmbedMany(external).On("order_id")
	s := rebuildSyncEngine(emptyRowsEngine(), &fakeColl{}, []*ViewDefinition{v})
	if err := s.RebuildAllViews(context.Background()); err != nil {
		t.Fatalf("RebuildAllViews: %v", err)
	}
}
