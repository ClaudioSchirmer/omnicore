//go:build sqlite

package migration

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// Fix #4: a ":memory:" DSN must resolve to a SHARED-CACHE named in-memory
// database so the migration runner and the engine share ONE database. Pre-fix,
// each opened a bare ":memory:" — a private database per pool — so the framework
// control-plane migrations landed in a database the engine never saw, and the
// service booted against an unmigrated store.
//
// This proof needs no bench container (SQLite in RAM is the whole point): a
// "keeper" connection holds the shared database alive across pools exactly as
// the engine's perennial connection (MaxOpenConns=1) does at boot; migrations
// run on their OWN *sql.DB; a THIRD pool must then see the migrated tables.
func TestSQLiteMemory_MigrationsSharedAcrossPools(t *testing.T) {
	shared := sqliteMigrateDSN(":memory:")

	// The migrate DSN and the engine's DSN must name the same in-memory database.
	if wantName := sqliteSharedMemoryName; !contains(shared, wantName) ||
		!contains(shared, "mode=memory") || !contains(shared, "cache=shared") {
		t.Fatalf("migrate :memory: DSN must be shared-cache %q, got %q", wantName, shared)
	}

	keeper, err := sql.Open("sqlite", shared)
	if err != nil {
		t.Fatal(err)
	}
	keeper.SetMaxOpenConns(1)
	defer keeper.Close()
	if err := keeper.Ping(); err != nil { // establishes + pins the shared DB
		t.Fatal(err)
	}

	m := NewSQLite(":memory:", filepath.Join(t.TempDir(), "no-service-migrations"))
	if err := m.Up(context.Background()); err != nil {
		t.Fatalf("Up against :memory: failed: %v", err)
	}

	// A SEPARATE pool on the same shared DSN must SEE the migrated control plane.
	reader, err := sql.Open("sqlite", shared)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	for _, tbl := range []string{"outbox", "omnicore_mongo_views", "omnicore_projection_failures"} {
		var name string
		if err := reader.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, tbl,
		).Scan(&name); err != nil {
			t.Errorf("control-plane table %q not visible cross-pool against :memory:: %v", tbl, err)
		}
	}

	// Control: a private (bare) :memory: pool must NOT see the shared tables.
	priv, err := sql.Open("sqlite", "file::memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer priv.Close()
	var name string
	if err := priv.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name='outbox'`,
	).Scan(&name); err == nil {
		t.Error("a private :memory: pool must NOT see the shared control plane (control check failed)")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
