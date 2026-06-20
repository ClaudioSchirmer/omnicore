package infra

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// This file covers the unit-reachable, non-Kafka / non-pinned-connection
// branches of rebuild.go. The boot-time ExecuteRebuild path pins a real
// *pgxpool.Conn via Postgres.acquire — against the fake pool that assertion
// fails, so we cover the cfg.Orphan validation and the acquire() error branch.
// The operator-triggered RebuildView / RebuildViewSince / RebuildAllViews /
// rebuildFromTable paths run end-to-end against the fake pool + fake Mongo when
// the root table yields zero rows (empty compose+upsert loop). The live
// compose+upsert body and Kafka Start/run remain integration-only.

func rebuildSyncEngine(pool pgxPool, coll mongoColl, views []*ViewDefinition) *SyncEngine {
	return NewSyncEngine(newFakePostgres(pool), newFakeMongo(coll), nil, "grp", views, 1)
}

func rebuildView() *ViewDefinition {
	return View("orders").Version(1).Root("orders").Schema(composerRootSchema())
}

func TestExecuteRebuild_InvalidOrphan(t *testing.T) {
	s := rebuildSyncEngine(newFakePool(), &fakeColl{}, []*ViewDefinition{rebuildView()})
	plan := DriftPlan{View: rebuildView()}
	err := s.ExecuteRebuild(context.Background(), plan, RebuildConfig{Orphan: "bogus"})
	if err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("expected invalid-orphan error, got %v", err)
	}
}

func TestExecuteRebuild_AcquireError(t *testing.T) {
	// The fake pool is not a *pgxpool.Pool, so Postgres.acquire returns an error
	// before any lock/rebuild work — the now-unit-reachable acquire branch.
	s := rebuildSyncEngine(newFakePool(), &fakeColl{}, []*ViewDefinition{rebuildView()})
	plan := DriftPlan{View: rebuildView()}
	err := s.ExecuteRebuild(context.Background(), plan, RebuildConfig{Orphan: "delete"})
	if err == nil || !strings.Contains(err.Error(), "acquire pg connection") {
		t.Fatalf("expected acquire error, got %v", err)
	}
}

func TestRegistryCombinedOrNone(t *testing.T) {
	if got := registryCombinedOrNone(nil); got != "<none>" {
		t.Errorf("nil registry = %q, want <none>", got)
	}
	row := &ViewRegistryRow{CombinedHash: "abc123"}
	if got := registryCombinedOrNone(row); got != "abc123" {
		t.Errorf("registry hash = %q, want abc123", got)
	}
}

func TestRebuildView_EmptyTable(t *testing.T) {
	pool := newFakePool()
	pool.queryHandler = func(sql string, args []any) (pgx.Rows, error) {
		// "SELECT id FROM orders ..." → no rows, so the compose+upsert loop is empty.
		return &fakeRows{rows: 0}, nil
	}
	s := rebuildSyncEngine(pool, &fakeColl{}, []*ViewDefinition{rebuildView()})
	if err := s.RebuildView(context.Background(), rebuildView()); err != nil {
		t.Fatalf("RebuildView: %v", err)
	}
}

func TestRebuildViewSince_EmptyTable(t *testing.T) {
	pool := newFakePool()
	pool.queryHandler = func(sql string, args []any) (pgx.Rows, error) {
		return &fakeRows{rows: 0}, nil
	}
	s := rebuildSyncEngine(pool, &fakeColl{}, []*ViewDefinition{rebuildView()})
	if err := s.RebuildViewSince(context.Background(), rebuildView(), time.Now()); err != nil {
		t.Fatalf("RebuildViewSince: %v", err)
	}
}

func TestRebuildFromTable_QueryError(t *testing.T) {
	pool := newFakePool()
	pool.queryHandler = func(sql string, args []any) (pgx.Rows, error) {
		return nil, errFake
	}
	s := rebuildSyncEngine(pool, &fakeColl{}, []*ViewDefinition{rebuildView()})
	if err := s.RebuildView(context.Background(), rebuildView()); err == nil {
		t.Fatal("expected query error from rebuildFromTable")
	}
}

func TestInitRegistryOnly(t *testing.T) {
	// InitViewRegistry runs against s.pg.pool.Exec (the fake returns OK 1).
	s := rebuildSyncEngine(newFakePool(), &fakeColl{}, []*ViewDefinition{rebuildView()})
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

func TestRefreshRegistryArtifactOnly(t *testing.T) {
	// EndRebuild runs against s.pg.pool.Exec; the fake CommandTag reports 1 row.
	s := rebuildSyncEngine(newFakePool(), &fakeColl{}, []*ViewDefinition{rebuildView()})
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
	pool := newFakePool()
	pool.execHandler = func(sql string, args []any) (pgconn.CommandTag, error) {
		return pgconn.CommandTag{}, errFake
	}
	s := rebuildSyncEngine(pool, &fakeColl{}, []*ViewDefinition{rebuildView()})
	plan := DriftPlan{View: rebuildView(), CurrentVersion: 1}
	if err := s.InitRegistryOnly(context.Background(), plan, "svc"); err == nil {
		t.Fatal("expected exec error from InitRegistryOnly")
	}
}

func TestSampleExpectedFieldNames(t *testing.T) {
	pool := newFakePool()
	pool.queryHandler = func(sql string, args []any) (pgx.Rows, error) {
		if strings.Contains(sql, "SELECT id FROM") {
			// Sample-id query: one root id.
			return &fakeRows{rows: 1, scan: func(_ int, dest []any) error {
				if p, ok := dest[0].(*string); ok {
					*p = "o1"
				}
				return nil
			}}, nil
		}
		// Compose's root fetch (SELECT * FROM orders ...).
		return &composerRows{cols: []string{"id", "name"}, data: [][]any{{"o1", "first"}}}, nil
	}
	c := NewComposer(newFakePostgres(pool))
	got, err := sampleExpectedFieldNames(context.Background(), c, rebuildView())
	if err != nil {
		t.Fatalf("sampleExpectedFieldNames: %v", err)
	}
	if _, ok := got["id"]; !ok {
		t.Errorf("expected 'id' in field set, got %v", got)
	}
	if _, ok := got["name"]; !ok {
		t.Errorf("expected 'name' in field set, got %v", got)
	}
}

func TestSampleExpectedFieldNames_EmptyAndError(t *testing.T) {
	// No rows → empty expected set (the "skip cleanup" branch).
	emptyPool := newFakePool()
	emptyPool.queryHandler = func(sql string, args []any) (pgx.Rows, error) {
		return &fakeRows{rows: 0}, nil
	}
	got, err := sampleExpectedFieldNames(context.Background(), NewComposer(newFakePostgres(emptyPool)), rebuildView())
	if err != nil || len(got) != 0 {
		t.Fatalf("empty sample = %v, err=%v, want empty", got, err)
	}
	// Sample-id query error surfaces.
	errPool := newFakePool()
	errPool.queryHandler = func(sql string, args []any) (pgx.Rows, error) { return nil, errFake }
	if _, err := sampleExpectedFieldNames(context.Background(), NewComposer(newFakePostgres(errPool)), rebuildView()); err == nil {
		t.Fatal("expected sample-id query error")
	}
}

func TestRebuildAllViews_EmptyTables(t *testing.T) {
	pool := newFakePool()
	pool.queryHandler = func(sql string, args []any) (pgx.Rows, error) {
		return &fakeRows{rows: 0}, nil
	}
	// A view with a PG root + an external Mongo embed so both byPGTable and
	// byMongoColl index buckets are walked.
	external := FromSchema(NewExternalSchema("buyers").PK("ID", "id").FK("order_id")).As("Buyers")
	v := View("orders").Version(1).Root("orders").Schema(composerRootSchema()).
		EmbedMany("buyers", external)
	s := rebuildSyncEngine(pool, &fakeColl{}, []*ViewDefinition{v})
	if err := s.RebuildAllViews(context.Background()); err != nil {
		t.Fatalf("RebuildAllViews: %v", err)
	}
}
