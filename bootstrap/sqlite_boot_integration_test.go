//go:build integration && sqlite

package bootstrap

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	_ "github.com/ClaudioSchirmer/omnicore/infra/db/engine/sqlite"

	_ "modernc.org/sqlite"
)

// Integration test: a SQLite service boots through the framework composition root
// in the INFRA-FREE posture — no mongo.uri, tagless transport (the build carries
// no -tags kafka|nats, so transport_none's no-op subscriber is linked). It proves
// the Etapa B contract end to end: buildDeps returns a live SQLite engine with
// deps.Mongo == nil, applyMigrations selects the SQLite runner and applies the
// control plane to the .db file, and no nil-Mongo deref panics along the way.
//
//	go test -tags=integration,sqlite ./bootstrap/ -run SQLiteBoot -count=1
func sqliteBootConfig(t *testing.T, dbPath, migrationsDir string) *Config {
	cfg := &Config{Service: "sqlite-boot-probe"}
	cfg.Relational.Dialect = dialectSQLite
	cfg.Relational.DSN = "file:" + dbPath
	cfg.Relational.Clock = "app"
	// No mongo.uri, no transport.* — the infra-free posture.
	cfg.Migrations.Dir = migrationsDir
	cfg.Migrations.AutoRun = AutoRunTrue
	cfg.applyDefaults()
	return cfg
}

func TestSQLiteBoot_InfraFree_BuildDeps(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "boot.db")

	cfg := sqliteBootConfig(t, dbPath, t.TempDir())
	deps, err := buildDeps(cfg)
	if err != nil {
		t.Fatalf("buildDeps on an infra-free SQLite service failed: %v", err)
	}
	t.Cleanup(func() { deps.DB.Close() })

	if deps.DB == nil {
		t.Fatal("Deps.DB is nil after a successful buildDeps")
	}
	// The heart of the infra-free posture: Mongo was never connected.
	if deps.Mongo != nil {
		t.Error("infra-free posture must leave deps.Mongo nil (no mongo.uri configured)")
	}
	// The transport is the no-op subscriber (tagless build), not nil.
	if deps.Transport == nil {
		t.Error("deps.Transport must be the no-op subscriber, not nil")
	}
}

func TestSQLiteBoot_InfraFree_AppliesControlPlane(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "boot.db")
	cfg := sqliteBootConfig(t, dbPath, t.TempDir())

	deps, err := buildDeps(cfg)
	if err != nil {
		t.Fatalf("buildDeps: %v", err)
	}
	t.Cleanup(func() { deps.DB.Close() })

	if err := applyMigrations(context.Background(), cfg, deps); err != nil {
		t.Fatalf("applyMigrations selected/ran the SQLite runner and failed: %v", err)
	}

	// Verify the control plane landed in the .db file.
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var name string
	if err := db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name='outbox'`,
	).Scan(&name); err != nil {
		t.Errorf("expected the control-plane outbox table after infra-free boot migrations: %v", err)
	}
}

// TestSQLiteBoot_EngineReachable is the pre-flight the other tests rely on: the
// SQLite engine needs no container, so this always resolves (unlike the
// bench-backed engines, whose boot tests skip when the container is down).
func TestSQLiteBoot_EngineReachable(t *testing.T) {
	eng, err := core.NewEngine(dialectSQLite, context.Background(),
		core.EngineConfig{DSN: "file:" + filepath.Join(t.TempDir(), "reach.db")})
	if err != nil {
		t.Fatalf("SQLite engine must be reachable with no infrastructure: %v", err)
	}
	eng.Close()
}
