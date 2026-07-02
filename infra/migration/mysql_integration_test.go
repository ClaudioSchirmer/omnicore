//go:build integration && mysql

package migration

import (
	"context"
	"database/sql"
	"os"
	"testing"
)

// Phase: MySQL migration runner. Runs the flattened framework migration against
// a real MySQL container (devops/docker-compose.yml `mysql`, host :3307) through
// the Manager, then asserts the control-plane tables exist and that the COMMENTs
// landed.
//
//	go test -tags=integration,mysql ./infra/migration/ -count=1

func mysqlDSN() string {
	if v := os.Getenv("MYSQL_DSN"); v != "" {
		return v
	}
	return "omnicore:omnicore@tcp(127.0.0.1:3307)/users_db?parseTime=true&multiStatements=true"
}

func TestMySQLManager_Up_AppliesFrameworkSchema(t *testing.T) {
	ctx := context.Background()
	raw, err := sql.Open("mysql", mysqlDSN())
	if err != nil {
		t.Skipf("MySQL not reachable: %v", err)
	}
	defer raw.Close()
	if err := raw.PingContext(ctx); err != nil {
		t.Skipf("MySQL not reachable: %v", err)
	}

	// Fresh slate: drop every framework table + both tracking tables.
	for _, tbl := range []string{
		"omnicore_integration_processed", "omnicore_integration_failures", "integration_events",
		"audit_events", "omnicore_upstream_failures", "omnicore_mongo_views", "outbox",
		"omnicore_framework_migrations", "omnicore_migrations",
	} {
		if _, err := raw.ExecContext(ctx, "DROP TABLE IF EXISTS "+tbl); err != nil {
			t.Fatalf("drop %s: %v", tbl, err)
		}
	}

	// Service dir with a no-op migration (golang-migrate's file source rejects an
	// empty dir on Up). Exercises the service stage on the MySQL runner too.
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/0001_noop.up.sql", []byte("SELECT 1;"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/0001_noop.down.sql", []byte("SELECT 1;"), 0o644); err != nil {
		t.Fatal(err)
	}

	mgr := NewMySQL(mysqlDSN(), dir)
	if err := mgr.Up(ctx); err != nil {
		t.Fatalf("Up: %v", err)
	}

	// Every control-plane table exists.
	for _, tbl := range []string{
		"outbox", "omnicore_mongo_views", "omnicore_upstream_failures", "audit_events",
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
