//go:build integration && sqlite

package migration

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// TestSQLiteControlPlane_Applies proves the embedded SQLite control-plane DDL
// (0001_framework / 0002_view_slots / 0003_projection_failures) is valid SQLite
// and applies cleanly against a real database file — the check no fake-driver
// unit test can make. It needs no bench container: a throwaway .db in the test's
// temp dir is the whole "infrastructure", which is itself the point of the
// SQLite engine.
func TestSQLiteControlPlane_Applies(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "cp.db")
	dsn := "file:" + dbPath
	// A missing service dir → only the framework stage runs (Manager tolerates it).
	m := NewSQLite(dsn, filepath.Join(t.TempDir(), "no-service-migrations"))

	if err := m.Up(context.Background()); err != nil {
		t.Fatalf("Up: applying the SQLite control plane failed: %v", err)
	}
	// Idempotent: a second Up with nothing pending is a no-op.
	if err := m.Up(context.Background()); err != nil {
		t.Fatalf("second Up (idempotent): %v", err)
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	wantTables := []string{
		"outbox",
		"omnicore_mongo_views",
		"audit_events",
		"integration_events",
		"omnicore_integration_failures",
		"omnicore_integration_processed",
		"omnicore_projection_failures",
	}
	for _, tbl := range wantTables {
		var name string
		err := db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, tbl,
		).Scan(&name)
		if err != nil {
			t.Errorf("expected control-plane table %q to exist: %v", tbl, err)
		}
	}

	// 0002 added the two blue-green slot columns onto omnicore_mongo_views.
	var slotCols int
	rows, err := db.Query(`PRAGMA table_info(omnicore_mongo_views)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid        int
			name, typ  string
			notNull    int
			dflt       sql.NullString
			pk         int
		)
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		if name == "active_collection" || name == "shadow_collection" {
			slotCols++
		}
	}
	if slotCols != 2 {
		t.Errorf("expected 0002 to add active_collection + shadow_collection, found %d of 2", slotCols)
	}
}
