//go:build integration && postgres

package mongo

import (
	"context"
	"testing"
	"time"

	"github.com/ClaudioSchirmer/omnicore/infra/db/query"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// TestExecuteRebuild_FreshViewCreatesRegistryAndBackfills is the DriftFreshBackfill
// proof: a brand-new Mongo view over an aggregate that ALREADY holds data (rows
// written before the view existed, NO registry row) is backfilled — and the
// registry row is CREATED under the advisory lock (BeginRebuild/EndRebuild are
// UPDATEs, so without this the row would never exist and the backfill would loop
// every boot). The second ExecuteRebuild (the "another pod arrives after the
// first" case) re-reads the row under the lock, skips the insert, and re-backfills
// idempotently — no duplicate-insert error, converging to the same state.
func TestExecuteRebuild_FreshViewCreatesRegistryAndBackfills(t *testing.T) {
	pg, cleanupPG := newTestPG(t)
	defer cleanupPG()
	m, cleanupMongo := newTestMongo(t)
	defer cleanupMongo()

	ctx := context.Background()
	// loaderRootSchema declares a Revision column (the blue-green verify needs it
	// for parity); seed only the roots — the backfill is what we assert.
	createLoaderTables(t, pg)
	for _, name := range []string{"a", "b", "c"} {
		if _, err := pg.Pool().Exec(ctx,
			`INSERT INTO loader_roots (name, email) VALUES ($1, $2)`, name, name+"@x.com"); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}

	view := query.View("loader_roots").Schema(loaderRootSchema()).Version(1)
	// A pg-backed resolver reflects the blue-green flip (the docs land in the
	// now-active shadow slot, not the bare name).
	resolver := query.NewViewResolver(pg)
	engine := query.NewSyncEngine(pg, m, resolver, nil, "", []*query.ViewDefinition{view}, 1)

	activeCount := func() int64 {
		if err := resolver.Refresh(ctx); err != nil {
			t.Fatalf("resolver refresh: %v", err)
		}
		n, _ := m.Collection(resolver.Active("loader_roots").String()).CountDocuments(ctx, bson.M{})
		return n
	}

	// A fresh view: NO registry row — the DriftFreshBackfill plan the boot builds.
	plan := query.DriftPlan{
		View:                view,
		Registry:            nil,
		Decision:            query.DriftFreshBackfill,
		CurrentVersion:      view.VersionNumber(),
		CurrentRebuildHash:  view.RebuildHash(),
		CurrentArtifactHash: view.ArtifactHash(),
		CurrentCombinedHash: view.Hash(),
	}
	cfg := query.RebuildConfig{Orphan: "warn", ServiceName: "test", Workers: 1, BatchSize: 1000}

	if err := engine.ExecuteRebuild(ctx, plan, cfg); err != nil {
		t.Fatalf("ExecuteRebuild (fresh backfill): %v", err)
	}

	// the pre-existing rows are backfilled into the now-active slot
	if n := activeCount(); n != 3 {
		t.Errorf("expected 3 backfilled docs in the active slot, got %d", n)
	}
	// and the registry row was CREATED under the lock, at the spec hash
	reg, err := query.ReadViewRegistry(ctx, pg.Querier(), pg.Dialect(), "loader_roots")
	if err != nil {
		t.Fatalf("read registry: %v", err)
	}
	if reg == nil {
		t.Fatal("registry row must be created for the fresh view (else the backfill loops every boot)")
	}
	if reg.CombinedHash != view.Hash() {
		t.Errorf("registry hash = %s, want spec %s", reg.CombinedHash, view.Hash())
	}

	// A second run (the concurrent "pod B after pod A" case): the row now exists,
	// so the re-read-under-lock skips the insert and re-backfills without error.
	if err := engine.ExecuteRebuild(ctx, plan, cfg); err != nil {
		t.Fatalf("ExecuteRebuild (2nd, idempotent): %v", err)
	}
	if n := activeCount(); n != 3 {
		t.Errorf("2nd (idempotent) run should keep 3 docs, got %d", n)
	}
}

// --- RebuildAllViews / RebuildView (operator path) ------------------------

func TestRebuildView_RebuildsFromTable(t *testing.T) {
	pg, cleanupPG := newTestPG(t)
	defer cleanupPG()
	m, cleanupMongo := newTestMongo(t)
	defer cleanupMongo()

	createTable(t, pg, `CREATE TABLE rv_users (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		name TEXT NOT NULL,
		deleted_at TIMESTAMP,
		created_at TIMESTAMP NOT NULL DEFAULT NOW(),
		updated_at TIMESTAMP NOT NULL DEFAULT NOW()
	)`)
	pg.Pool().Exec(context.Background(),
		`INSERT INTO rv_users (name) VALUES ('a'), ('b'), ('c')`)

	view := query.View("rv_users").Schema(rootSchema("rv_users")).Version(1)
	engine := query.NewSyncEngine(pg, m, testResolver, nil, "", []*query.ViewDefinition{view}, 1)
	if err := engine.RebuildView(context.Background(), view); err != nil {
		t.Fatalf("RebuildView: %v", err)
	}

	count, _ := m.Collection("rv_users").CountDocuments(context.Background(), bson.M{})
	if count != 3 {
		t.Errorf("expected 3 mongo docs, got %d", count)
	}
}

func TestRebuildAllViews(t *testing.T) {
	pg, cleanupPG := newTestPG(t)
	defer cleanupPG()
	m, cleanupMongo := newTestMongo(t)
	defer cleanupMongo()

	createTable(t, pg, `CREATE TABLE rv_a (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		name TEXT NOT NULL,
		deleted_at TIMESTAMP,
		created_at TIMESTAMP NOT NULL DEFAULT NOW(),
		updated_at TIMESTAMP NOT NULL DEFAULT NOW()
	)`)
	createTable(t, pg, `CREATE TABLE rv_b (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		label TEXT NOT NULL,
		deleted_at TIMESTAMP,
		created_at TIMESTAMP NOT NULL DEFAULT NOW(),
		updated_at TIMESTAMP NOT NULL DEFAULT NOW()
	)`)
	pg.Pool().Exec(context.Background(), `INSERT INTO rv_a (name) VALUES ('x')`)
	pg.Pool().Exec(context.Background(), `INSERT INTO rv_b (label) VALUES ('y')`)

	va := query.View("rv_a").Schema(rootSchema("rv_a")).Version(1)
	vb := query.View("rv_b").Schema(rootSchema("rv_b")).Version(1)
	engine := query.NewSyncEngine(pg, m, testResolver, nil, "", []*query.ViewDefinition{va, vb}, 1)
	if err := engine.RebuildAllViews(context.Background()); err != nil {
		t.Fatalf("RebuildAllViews: %v", err)
	}
	for _, c := range []string{"rv_a", "rv_b"} {
		n, _ := m.Collection(c).CountDocuments(context.Background(), bson.M{})
		if n != 1 {
			t.Errorf("collection %q: expected 1 doc, got %d", c, n)
		}
	}
}

func TestRebuildViewSince_FiltersByUpdatedAt(t *testing.T) {
	pg, cleanupPG := newTestPG(t)
	defer cleanupPG()
	m, cleanupMongo := newTestMongo(t)
	defer cleanupMongo()

	createTable(t, pg, `CREATE TABLE rv_since (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		name TEXT NOT NULL,
		deleted_at TIMESTAMP,
		created_at TIMESTAMP NOT NULL DEFAULT NOW(),
		updated_at TIMESTAMP NOT NULL DEFAULT NOW()
	)`)
	// Old row.
	pg.Pool().Exec(context.Background(),
		`INSERT INTO rv_since (name, updated_at) VALUES ('old', NOW() - INTERVAL '1 day')`)
	pg.Pool().Exec(context.Background(),
		`INSERT INTO rv_since (name, updated_at) VALUES ('new', NOW())`)

	// RebuildViewSince scans/orders on the schema's declared UpdatedAt column, so
	// a view that supports incremental rebuild must declare it.
	view := query.View("rv_since").
		Schema(rootSchema("rv_since").UpdatedAt("updated_at")).Version(1)
	engine := query.NewSyncEngine(pg, m, testResolver, nil, "", []*query.ViewDefinition{view}, 1)
	since := time.Now().Add(-1 * time.Hour)
	if err := engine.RebuildViewSince(context.Background(), view, since); err != nil {
		t.Fatalf("RebuildViewSince: %v", err)
	}
	n, _ := m.Collection("rv_since").CountDocuments(context.Background(), bson.M{})
	if n != 1 {
		t.Errorf("expected 1 doc since 1h, got %d", n)
	}
}

// --- ExecuteRebuild end-to-end (PG + Mongo with lock + status) ----------

func TestExecuteRebuild_HappyPath(t *testing.T) {
	pg, cleanupPG := newTestPG(t)
	defer cleanupPG()
	m, cleanupMongo := newTestMongo(t)
	defer cleanupMongo()

	// revision is part of the fixture because production REQUIRES it (the
	// repository panics at boot on a schema without one) and the blue-green
	// verify checks REVISION PARITY between source and shadow. A fixture table
	// without it was modelling a root that cannot exist.
	createTable(t, pg, `CREATE TABLE er_users (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		name TEXT NOT NULL,
		revision BIGINT NOT NULL DEFAULT 0,
		deleted_at TIMESTAMP,
		created_at TIMESTAMP NOT NULL DEFAULT NOW(),
		updated_at TIMESTAMP NOT NULL DEFAULT NOW()
	)`)
	for _, n := range []string{"alice", "bob", "carol"} {
		pg.Pool().Exec(context.Background(),
			`INSERT INTO er_users (name) VALUES ($1)`, n)
	}

	view := query.View("er_users").Schema(rootSchema("er_users").Revision("revision")).Version(1)

	// Seed the registry row at the SAME hash so EndRebuild can find it.
	now := time.Now().UTC()
	query.InitViewRegistry(context.Background(), pg.Querier(), pg.Dialect(), query.InitViewRegistryInput{
		ViewName: view.Name(), Version: view.VersionNumber(),
		RebuildHash: view.RebuildHash(), ArtifactHash: view.ArtifactHash(),
		CombinedHash: view.Hash(), ServiceName: "svc", Now: now,
	})

	// Seed the slot this rebuild will RETIRE (before the first flip the bare
	// collection is the active one) with a marker document, so the survival
	// assertion below is a real observation and not a vacuous count of a
	// collection that was empty all along.
	if _, err := m.Collection(view.Name()).InsertOne(context.Background(), bson.M{"_id": "retired-marker"}); err != nil {
		t.Fatalf("seed retired slot marker: %v", err)
	}

	plan := query.DriftPlan{
		View:                view,
		CurrentVersion:      view.VersionNumber(),
		CurrentRebuildHash:  view.RebuildHash(),
		CurrentArtifactHash: view.ArtifactHash(),
		CurrentCombinedHash: view.Hash(),
	}
	// Blue-green: a pg-backed resolver reflects the flip. (The driver waits one
	// lease before backfill and one after flip; the default lease makes this
	// test slow but correct.)
	resolver := query.NewViewResolver(pg)
	engine := query.NewSyncEngine(pg, m, resolver, nil, "", []*query.ViewDefinition{view}, 1)
	if err := engine.ExecuteRebuild(context.Background(), plan, query.RebuildConfig{
		Orphan: "delete", ServiceName: "svc",
	}); err != nil {
		t.Fatalf("ExecuteRebuild: %v", err)
	}

	// The rebuild flipped to the shadow slot (er_users__0); the docs live in the
	// now-active slot and the RETIRED slot survives the flip.
	if err := resolver.Refresh(context.Background()); err != nil {
		t.Fatalf("resolver refresh: %v", err)
	}
	active := resolver.Active(view.Name()).String()
	if active != view.Name()+"__0" {
		t.Errorf("active slot after rebuild = %q, want er_users__0", active)
	}
	n, _ := m.Collection(active).CountDocuments(context.Background(), bson.M{})
	if n != 3 {
		t.Errorf("expected 3 mongo docs in the active slot, got %d", n)
	}
	// CONTRACT: the retired slot is NOT reclaimed at the flip — dropping it there
	// races in-flight operations that resolved the pointer while it was valid. The
	// marker seeded before the rebuild must still be there; the NEXT rebuild's
	// pre-provision drop is what reclaims it.
	bare, _ := m.Collection(view.Name()).CountDocuments(context.Background(), bson.M{})
	if bare != 1 {
		t.Errorf("expected the retired slot to survive the flip with its marker doc, got %d docs", bare)
	}
	row, _ := query.ReadViewRegistry(context.Background(), pg.Querier(), pg.Dialect(), view.Name())
	if row.Status != query.ViewRegistryStatusDone {
		t.Errorf("registry status after rebuild = %q, want done", row.Status)
	}
}

func TestExecuteRebuild_RejectsInvalidOrphanMode(t *testing.T) {
	pg, cleanupPG := newTestPG(t)
	defer cleanupPG()
	m, cleanupMongo := newTestMongo(t)
	defer cleanupMongo()

	view := query.View("er_x").Schema(rootSchema("er_x")).Version(1)
	plan := query.DriftPlan{View: view}
	engine := query.NewSyncEngine(pg, m, testResolver, nil, "", []*query.ViewDefinition{view}, 1)
	if err := engine.ExecuteRebuild(context.Background(), plan, query.RebuildConfig{Orphan: "banana"}); err == nil {
		t.Error("expected ExecuteRebuild to reject invalid Orphan mode")
	}
}

// --- InitRegistryOnly / RefreshRegistryArtifactOnly fast paths --------

func TestInitRegistryOnly_WritesRow(t *testing.T) {
	pg, cleanupPG := newTestPG(t)
	defer cleanupPG()
	m, cleanupMongo := newTestMongo(t)
	defer cleanupMongo()

	view := query.View("ir_users").Schema(rootSchema("ir_users")).Version(1)
	engine := query.NewSyncEngine(pg, m, testResolver, nil, "", []*query.ViewDefinition{view}, 1)
	plan := query.DriftPlan{
		View:                view,
		CurrentVersion:      view.VersionNumber(),
		CurrentRebuildHash:  view.RebuildHash(),
		CurrentArtifactHash: view.ArtifactHash(),
		CurrentCombinedHash: view.Hash(),
	}
	if err := engine.InitRegistryOnly(context.Background(), plan, "svc"); err != nil {
		t.Fatalf("InitRegistryOnly: %v", err)
	}
	if row, _ := query.ReadViewRegistry(context.Background(), pg.Querier(), pg.Dialect(), view.Name()); row == nil {
		t.Error("expected registry row after InitRegistryOnly")
	}
}

func TestRefreshRegistryArtifactOnly(t *testing.T) {
	pg, cleanupPG := newTestPG(t)
	defer cleanupPG()
	m, cleanupMongo := newTestMongo(t)
	defer cleanupMongo()

	view := query.View("rfa").Schema(rootSchema("rfa")).Version(1)
	// Seed.
	now := time.Now().UTC()
	query.InitViewRegistry(context.Background(), pg.Querier(), pg.Dialect(), query.InitViewRegistryInput{
		ViewName: view.Name(), Version: 1,
		RebuildHash: view.RebuildHash(), ArtifactHash: "old",
		CombinedHash: "old", ServiceName: "svc", Now: now,
	})

	engine := query.NewSyncEngine(pg, m, testResolver, nil, "", []*query.ViewDefinition{view}, 1)
	plan := query.DriftPlan{
		View: view, CurrentVersion: 1,
		CurrentRebuildHash:  view.RebuildHash(),
		CurrentArtifactHash: view.ArtifactHash(),
		CurrentCombinedHash: view.Hash(),
	}
	if err := engine.RefreshRegistryArtifactOnly(context.Background(), plan, "svc"); err != nil {
		t.Fatalf("RefreshRegistryArtifactOnly: %v", err)
	}
	row, _ := query.ReadViewRegistry(context.Background(), pg.Querier(), pg.Dialect(), view.Name())
	if row.CombinedHash == "old" {
		t.Error("CombinedHash should have been refreshed")
	}
}
