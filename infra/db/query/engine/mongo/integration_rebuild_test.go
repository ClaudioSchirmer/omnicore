//go:build integration

package mongo

import (
	"context"
	"testing"
	"time"

	"github.com/ClaudioSchirmer/omnicore/infra/db/query"
	"go.mongodb.org/mongo-driver/v2/bson"
)

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

	view := query.View("rv_users").Root("rv_users").Schema(rootSchema("rv_users")).Version(1)
	engine := query.NewSyncEngine(pg, m, nil, "", []*query.ViewDefinition{view}, 1)
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

	va := query.View("rv_a").Root("rv_a").Schema(rootSchema("rv_a")).Version(1)
	vb := query.View("rv_b").Root("rv_b").Schema(rootSchema("rv_b")).Version(1)
	engine := query.NewSyncEngine(pg, m, nil, "", []*query.ViewDefinition{va, vb}, 1)
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
	view := query.View("rv_since").Root("rv_since").
		Schema(rootSchema("rv_since").UpdatedAt("updated_at")).Version(1)
	engine := query.NewSyncEngine(pg, m, nil, "", []*query.ViewDefinition{view}, 1)
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

	createTable(t, pg, `CREATE TABLE er_users (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		name TEXT NOT NULL,
		deleted_at TIMESTAMP,
		created_at TIMESTAMP NOT NULL DEFAULT NOW(),
		updated_at TIMESTAMP NOT NULL DEFAULT NOW()
	)`)
	for _, n := range []string{"alice", "bob", "carol"} {
		pg.Pool().Exec(context.Background(),
			`INSERT INTO er_users (name) VALUES ($1)`, n)
	}

	view := query.View("er_users").Root("er_users").Schema(rootSchema("er_users")).Version(1)

	// Seed the registry row at the SAME hash so EndRebuild can find it.
	now := time.Now().UTC()
	query.InitViewRegistry(context.Background(), pg.Querier(), pg.Dialect(), query.InitViewRegistryInput{
		ViewName: view.Name(), Version: view.VersionNumber(),
		RebuildHash: view.RebuildHash(), ArtifactHash: view.ArtifactHash(),
		CombinedHash: view.Hash(), ServiceName: "svc", Now: now,
	})

	plan := query.DriftPlan{
		View:                view,
		CurrentVersion:      view.VersionNumber(),
		CurrentRebuildHash:  view.RebuildHash(),
		CurrentArtifactHash: view.ArtifactHash(),
		CurrentCombinedHash: view.Hash(),
	}
	engine := query.NewSyncEngine(pg, m, nil, "", []*query.ViewDefinition{view}, 1)
	if err := engine.ExecuteRebuild(context.Background(), plan, query.RebuildConfig{
		Orphan: "delete", ServiceName: "svc",
	}); err != nil {
		t.Fatalf("ExecuteRebuild: %v", err)
	}

	n, _ := m.Collection("er_users").CountDocuments(context.Background(), bson.M{})
	if n != 3 {
		t.Errorf("expected 3 mongo docs after rebuild, got %d", n)
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

	view := query.View("er_x").Root("er_x").Schema(rootSchema("er_x")).Version(1)
	plan := query.DriftPlan{View: view}
	engine := query.NewSyncEngine(pg, m, nil, "", []*query.ViewDefinition{view}, 1)
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

	view := query.View("ir_users").Root("ir_users").Schema(rootSchema("ir_users")).Version(1)
	engine := query.NewSyncEngine(pg, m, nil, "", []*query.ViewDefinition{view}, 1)
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

	view := query.View("rfa").Root("rfa").Schema(rootSchema("rfa")).Version(1)
	// Seed.
	now := time.Now().UTC()
	query.InitViewRegistry(context.Background(), pg.Querier(), pg.Dialect(), query.InitViewRegistryInput{
		ViewName: view.Name(), Version: 1,
		RebuildHash: view.RebuildHash(), ArtifactHash: "old",
		CombinedHash: "old", ServiceName: "svc", Now: now,
	})

	engine := query.NewSyncEngine(pg, m, nil, "", []*query.ViewDefinition{view}, 1)
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
