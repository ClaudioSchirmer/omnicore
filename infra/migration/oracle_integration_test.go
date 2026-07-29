//go:build integration && oracle

package migration

import (
	"context"
	"database/sql"
	"os"
	"testing"
)

// Phase: Oracle migration runner. Runs the flattened framework migration
// against a real Oracle Free 23ai container (devops compose `oracle`, host
// :15211) through the Manager — over the framework's hand-rolled
// golang-migrate driver — then asserts the control-plane tables exist and the
// runner is idempotent.
//
//	go test -tags=integration,oracle ./infra/migration/ -count=1

func oracleDSN() string {
	if v := os.Getenv("ORACLE_DSN"); v != "" {
		return v
	}
	return "oracle://users_app:OmnicoreQA!2026@127.0.0.1:15211/FREEPDB1"
}

func TestOracleManager_Up_AppliesFrameworkSchema(t *testing.T) {
	ctx := context.Background()
	raw, err := sql.Open("oracle", oracleDSN())
	if err != nil {
		t.Skipf("Oracle not reachable: %v", err)
	}
	defer raw.Close()
	if err := raw.PingContext(ctx); err != nil {
		t.Skipf("Oracle not reachable: %v", err)
	}

	// Fresh slate: drop every framework table + both tracking tables (IF
	// EXISTS is native on the 23ai floor).
	for _, tbl := range []string{
		"omnicore_integration_processed", "omnicore_integration_failures", "integration_events",
		"audit_events", "omnicore_upstream_failures", "omnicore_projection_failures",
		"omnicore_mongo_views", "outbox",
		"omnicore_framework_migrations", "omnicore_migrations",
	} {
		if _, err := raw.ExecContext(ctx, "DROP TABLE IF EXISTS "+tbl+" CASCADE CONSTRAINTS"); err != nil {
			t.Fatalf("drop %s: %v", tbl, err)
		}
	}

	// Service dir with a no-op migration (golang-migrate's file source rejects
	// an empty dir on Up). Exercises the service stage — and the statement
	// splitter — on the Oracle runner too.
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/0001_noop.up.sql", []byte("SELECT 1 FROM dual;"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/0001_noop.down.sql", []byte("SELECT 1 FROM dual;"), 0o644); err != nil {
		t.Fatal(err)
	}

	mgr := NewOracle(oracleDSN(), dir)
	if err := mgr.Up(ctx); err != nil {
		t.Fatalf("Up: %v", err)
	}

	// Every control-plane table exists (the catalog folds the unquoted
	// declared names to UPPERCASE — the D11 convention).
	for _, tbl := range []string{
		"OUTBOX", "OMNICORE_MONGO_VIEWS", "OMNICORE_UPSTREAM_FAILURES", "AUDIT_EVENTS",
		"INTEGRATION_EVENTS", "OMNICORE_INTEGRATION_FAILURES", "OMNICORE_INTEGRATION_PROCESSED",
	} {
		var n int
		if err := raw.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM user_tables WHERE table_name = :1", tbl,
		).Scan(&n); err != nil {
			t.Fatalf("check table %s: %v", tbl, err)
		}
		if n != 1 {
			t.Fatalf("framework table %q not created by the Oracle migration", tbl)
		}
	}

	// The tracking tables carry the applied versions.
	var fwVersion int64
	if err := raw.QueryRowContext(ctx,
		"SELECT version FROM omnicore_framework_migrations FETCH FIRST 1 ROWS ONLY").Scan(&fwVersion); err != nil {
		t.Fatalf("read framework tracking row: %v", err)
	}
	// The framework sequence is 0001 + 0002 + 0003. This assertion was pinned at
	// 1 and went stale when 0002 landed — unnoticed, because this test SKIPS
	// unless ORACLE_DSN matches the bench, so nobody ever saw it fail. Derived
	// from the embedded set now, so it cannot go stale again.
	wantVersion := int64(len(frameworkMigrationNames()))
	if fwVersion != wantVersion {
		t.Fatalf("framework tracking version = %d, want %d", fwVersion, wantVersion)
	}

	// Idempotent: a second Up is a no-op (ErrNoChange absorbed).
	if err := mgr.Up(ctx); err != nil {
		t.Fatalf("second Up: %v", err)
	}
}

// TestOracleDriver_VersionLifecycle drives the hand-rolled driver directly
// through the golang-migrate contract points the Manager relies on:
// NilVersion on a fresh table, SetVersion round-trip (dirty included), and
// the Lock/Unlock pair over DBMS_LOCK.
func TestOracleDriver_VersionLifecycle(t *testing.T) {
	db, err := sql.Open("oracle", oracleDSN())
	if err != nil {
		t.Skipf("Oracle not reachable: %v", err)
	}
	defer db.Close()
	if err := db.PingContext(context.Background()); err != nil {
		t.Skipf("Oracle not reachable: %v", err)
	}
	_, _ = db.ExecContext(context.Background(), "DROP TABLE IF EXISTS omnicore_drv_test CASCADE CONSTRAINTS")

	drv, err := newOracleDriver(db, "omnicore_drv_test")
	if err != nil {
		t.Fatalf("newOracleDriver: %v", err)
	}

	if v, dirty, err := drv.Version(); err != nil || v != -1 || dirty {
		t.Fatalf("fresh Version = (%d,%v,%v), want (-1,false,nil)", v, dirty, err)
	}
	if err := drv.SetVersion(3, true); err != nil {
		t.Fatalf("SetVersion: %v", err)
	}
	if v, dirty, err := drv.Version(); err != nil || v != 3 || !dirty {
		t.Fatalf("Version = (%d,%v,%v), want (3,true,nil)", v, dirty, err)
	}
	if err := drv.Lock(); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	if err := drv.Unlock(); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	_, _ = db.ExecContext(context.Background(), "DROP TABLE IF EXISTS omnicore_drv_test CASCADE CONSTRAINTS")
}
