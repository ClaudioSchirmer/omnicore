//go:build integration && oracle

package migration

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Phase: Oracle migration runner. Runs the flattened framework migration
// against a real Oracle Free 23ai container (devops compose `oracle`, host
// :15211) through the Manager — over the framework's hand-rolled
// golang-migrate driver — then asserts the control-plane tables exist and the
// runner is idempotent.
//
//	go test -tags=integration,oracle ./infra/migration/ -count=1
//
// Each run targets a THROW-AWAY user/schema it creates and drops (Oracle's
// databases are heavyweight PDBs, so the isolation unit is a user/schema — the
// engine integration suite's convention, via OMNICORE_TEST_ORACLE_ADMIN_DSN). It
// must NOT touch the shared users_app schema: the example service owns that
// schema's omnicore_migrations tracking, and dropping/replaying it here wedges
// the next real boot — a cross-process collision on the persistent bench volume.

// The admin connection is SYS AS SYSDBA (go-ora's `dba privilege` option): the
// harness grants EXECUTE ON SYS.DBMS_LOCK to each throw-away user (the driver's
// Lock/Unlock), and only SYS can grant a SYS-owned object.
const defaultOracleAdminDSN = "oracle://sys:OmnicoreQA!2026@127.0.0.1:15211/FREEPDB1?dba+privilege=sysdba"

func oracleAdminDSN() string {
	if v := os.Getenv("OMNICORE_TEST_ORACLE_ADMIN_DSN"); v != "" {
		return v
	}
	return defaultOracleAdminDSN
}

// newMigrationTestOracleSchema creates a throw-away user/schema on the bench
// Oracle, returns its DSN and registers DROP USER … CASCADE as cleanup (retried
// through ORA-01940 while a straggler session still pins the user). Skips the
// test when Oracle is unreachable. The framework migration only creates TABLEs +
// INDEXes, so CREATE SESSION + CREATE TABLE + EXECUTE ON DBMS_LOCK (the driver's
// advisory lock) + SELECT_CATALOG_ROLE cover it.
func newMigrationTestOracleSchema(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	admin, err := sql.Open("oracle", oracleAdminDSN())
	if err != nil {
		t.Skipf("Oracle not reachable: %v", err)
	}
	if err := admin.PingContext(ctx); err != nil {
		_ = admin.Close()
		t.Skipf("Oracle not reachable: %v", err)
	}

	user := "omnicore_ora_mig_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	const pw = "OmnicoreQA_2026"
	for _, stmt := range []string{
		fmt.Sprintf("CREATE USER %s IDENTIFIED BY %s QUOTA UNLIMITED ON users", user, pw),
		"GRANT CREATE SESSION, CREATE TABLE TO " + user,
		"GRANT EXECUTE ON SYS.DBMS_LOCK TO " + user,
		"GRANT SELECT_CATALOG_ROLE TO " + user,
	} {
		if _, err := admin.ExecContext(ctx, stmt); err != nil {
			_ = admin.Close()
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	adminURL, err := url.Parse(oracleAdminDSN())
	if err != nil {
		_ = admin.Close()
		t.Fatalf("parse admin DSN: %v", err)
	}
	adminURL.User = url.UserPassword(user, pw)
	adminURL.RawQuery = "" // the admin's options (dba privilege) must not leak to the test user
	schemaDSN := adminURL.String()

	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		// A pooled session may take a beat to fully log off after Close — retry
		// the drop through ORA-01940 (user currently connected).
		var dropErr error
		for i := 0; i < 5; i++ {
			if _, dropErr = admin.ExecContext(c, "DROP USER "+user+" CASCADE"); dropErr == nil {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		if dropErr != nil {
			t.Logf("DROP USER %s CASCADE: %v (leftover schema on the bench)", user, dropErr)
		}
		_ = admin.Close()
	})
	return schemaDSN
}

func TestOracleManager_Up_AppliesFrameworkSchema(t *testing.T) {
	ctx := context.Background()
	dsn := newMigrationTestOracleSchema(t) // throw-away schema; skips cleanly when Oracle is down

	raw, err := sql.Open("oracle", dsn)
	if err != nil {
		t.Fatalf("open throw-away schema: %v", err)
	}
	defer raw.Close()

	// The schema is a fresh throw-away — no reset needed; the Manager creates the
	// framework schema + both tracking tables from empty.

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

	mgr := NewOracle(dsn, dir)
	if err := mgr.Up(ctx); err != nil {
		t.Fatalf("Up: %v", err)
	}

	// Every control-plane table exists (the catalog folds the unquoted
	// declared names to UPPERCASE — the D11 convention).
	for _, tbl := range []string{
		"OUTBOX", "OMNICORE_MONGO_VIEWS", "OMNICORE_PROJECTION_FAILURES", "AUDIT_EVENTS",
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
	// The framework sequence is 0001 + 0002 + 0003. Derived from the embedded set
	// so it cannot go stale as new framework migrations land.
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
	dsn := newMigrationTestOracleSchema(t) // throw-away schema; dropped on cleanup
	db, err := sql.Open("oracle", dsn)
	if err != nil {
		t.Fatalf("open throw-away schema: %v", err)
	}
	defer db.Close()

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
}
