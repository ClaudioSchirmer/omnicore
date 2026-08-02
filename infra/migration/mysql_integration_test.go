//go:build integration && mysql

package migration

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Phase: MySQL migration runner. Runs the flattened framework migration against
// a real MySQL container (devops/docker-compose.yml `mysql`, host :3307) through
// the Manager, then asserts the control-plane tables exist and that the COMMENTs
// landed.
//
//	go test -tags=integration,mysql ./infra/migration/ -count=1
//
// Each run targets a THROW-AWAY database it creates and drops (the same isolation
// the engine + boot integration suites use via OMNICORE_TEST_MYSQL_ADMIN_DSN). It
// must NOT touch the shared users_db: the example service migrates its own
// sequence into that database's omnicore_migrations service tracking table, so
// dropping that table here and re-running a one-migration probe dir wedges the
// next real boot ("Duplicate column" on 0002, left dirty) — a cross-process
// collision on the persistent bench volume.

func mysqlAdminDSN() string {
	if v := os.Getenv("OMNICORE_TEST_MYSQL_ADMIN_DSN"); v != "" {
		return v
	}
	return "root:root@tcp(127.0.0.1:3307)/?parseTime=true&multiStatements=true"
}

// newMigrationTestDB creates a throw-away MySQL database and returns its DSN,
// dropping it on cleanup. Skips the test when MySQL is unreachable. This is what
// keeps the migration runner off the shared users_db (and its example-owned
// service tracking).
func newMigrationTestDB(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	admin, err := sql.Open("mysql", mysqlAdminDSN())
	if err != nil {
		t.Skipf("MySQL not reachable: %v", err)
	}
	if err := admin.PingContext(ctx); err != nil {
		_ = admin.Close()
		t.Skipf("MySQL not reachable: %v", err)
	}
	dbName := "omnicore_mysql_mig_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+dbName); err != nil {
		_ = admin.Close()
		t.Fatalf("CREATE DATABASE %s: %v", dbName, err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = admin.ExecContext(c, "DROP DATABASE IF EXISTS "+dbName)
		_ = admin.Close()
	})
	return swapMySQLDB(mysqlAdminDSN(), dbName)
}

// swapMySQLDB inserts the database name into a `user:pass@tcp(host:port)/?p=v`
// DSN (between the last '/' and the '?').
func swapMySQLDB(adminDSN, name string) string {
	idx := strings.LastIndex(adminDSN, "/")
	q := strings.Index(adminDSN, "?")
	if idx == -1 || q == -1 || q < idx {
		return adminDSN
	}
	return adminDSN[:idx+1] + name + adminDSN[q:]
}

func TestMySQLManager_Up_AppliesFrameworkSchema(t *testing.T) {
	ctx := context.Background()
	dsn := newMigrationTestDB(t) // throw-away db; skips cleanly when MySQL is down

	raw, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open throw-away db: %v", err)
	}
	defer raw.Close()

	// The database is a fresh throw-away — no reset needed; the Manager creates
	// the framework schema + both tracking tables from empty.

	// Service dir with a no-op migration (golang-migrate's file source rejects an
	// empty dir on Up). Exercises the service stage on the MySQL runner too.
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/0001_noop.up.sql", []byte("SELECT 1;"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/0001_noop.down.sql", []byte("SELECT 1;"), 0o644); err != nil {
		t.Fatal(err)
	}

	mgr := NewMySQL(dsn, dir)
	if err := mgr.Up(ctx); err != nil {
		t.Fatalf("Up: %v", err)
	}

	// Every control-plane table exists.
	for _, tbl := range []string{
		"outbox", "omnicore_mongo_views", "omnicore_projection_failures", "audit_events",
		"integration_events", "omnicore_integration_failures", "omnicore_integration_processed",
	} {
		var n int
		if err := raw.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = ?`,
			tbl,
		).Scan(&n); err != nil {
			t.Fatalf("check table %s: %v", tbl, err)
		}
		if n != 1 {
			t.Fatalf("framework table %q not created by the MySQL migration", tbl)
		}
	}

	// Column COMMENTs landed (the maintainer-requested field descriptions).
	var comment string
	if err := raw.QueryRowContext(ctx,
		`SELECT COLUMN_COMMENT FROM information_schema.columns
		   WHERE table_schema = DATABASE() AND table_name = 'outbox' AND column_name = 'aggregate_id'`,
	).Scan(&comment); err != nil {
		t.Fatalf("read column comment: %v", err)
	}
	if comment == "" {
		t.Fatal("expected a COMMENT on outbox.aggregate_id (field descriptions missing)")
	}

	// Idempotent: a second Up is a no-op (ErrNoChange absorbed).
	if err := mgr.Up(ctx); err != nil {
		t.Fatalf("second Up: %v", err)
	}
}
