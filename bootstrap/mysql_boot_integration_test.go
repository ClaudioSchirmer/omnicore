//go:build integration && mysql

package bootstrap

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	// Blank import registers the "mysql" dialect in the engine registry — the
	// composition root resolves it by name, so bootstrap never imports it
	// directly outside this tag-guarded test.
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	_ "github.com/ClaudioSchirmer/omnicore/infra/db/engine/mysql"

	// The MySQL database/sql driver for the raw verification connection.
	_ "github.com/go-sql-driver/mysql"
)

// Integration test: a MySQL engine boots through the framework composition root.
// buildDeps is backend-neutral — the only Postgres-specific boot wiring
// (postgres.AsPostgres, audit partitions, the pgx migration runner) lives in
// bootstrap/engine_postgres.go behind the `postgres` build tag, so a MySQL build
// never compiles or links it. This proves buildDeps returns a live MySQL engine,
// and applyMigrations selects the MySQL runner (not the pgx pool).
//
//	go test -tags=integration,mysql ./bootstrap/ -run MySQLBoot -count=1
func mysqlBootDSN() string {
	if v := os.Getenv("MYSQL_DSN"); v != "" {
		return v
	}
	return "omnicore:omnicore@tcp(127.0.0.1:3307)/users_db?parseTime=true&multiStatements=true"
}

func mongoBootURI() string {
	if v := os.Getenv("OMNICORE_TEST_MONGO_URI"); v != "" {
		return v
	}
	return "mongodb://localhost:27018"
}

func mysqlBootConfig(t *testing.T, migrationsDir string) *Config {
	cfg := &Config{Service: "mysql-boot-probe"}
	cfg.Relational.Dialect = dialectMySQL
	cfg.Relational.DSN = mysqlBootDSN() // dialect + dsn both live under relational:
	cfg.Mongo.URI = mongoBootURI()
	cfg.Mongo.Database = "mysql_boot_probe"
	cfg.Migrations.Dir = migrationsDir
	cfg.Migrations.AutoRun = AutoRunTrue
	return cfg
}

func TestMySQLBoot_BuildDepsDoesNotPanic(t *testing.T) {
	// Pre-flight: skip cleanly when the MySQL container is not reachable, mirroring
	// the engine integration suite's policy.
	eng, err := core.NewEngine(dialectMySQL, context.Background(), mysqlBootDSN(), false)
	if err != nil {
		t.Skipf("MySQL not reachable (%v) — start devops/docker-compose.yml mysql service", err)
	}
	eng.Close()

	cfg := mysqlBootConfig(t, t.TempDir())

	deps, err := buildDeps(cfg)
	if err != nil {
		t.Fatalf("buildDeps on a MySQL engine failed: %v", err)
	}
	t.Cleanup(func() {
		deps.DB.Close()
		_ = deps.Mongo.Close(context.Background())
	})

	if deps.DB == nil {
		t.Fatal("Deps.DB is nil after a successful buildDeps")
	}
	// That the engine is MySQL (not Postgres) is now guaranteed by construction:
	// postgres.AsPostgres lives in the postgres-tagged engine package, which a
	// MySQL build neither compiles nor links — so buildDeps cannot reach for it.
	// The MySQL runner selection is proven end to end by the migration test below.
}

func TestMySQLBoot_ApplyMigrationsUsesMySQLRunner(t *testing.T) {
	eng, err := core.NewEngine(dialectMySQL, context.Background(), mysqlBootDSN(), false)
	if err != nil {
		t.Skipf("MySQL not reachable (%v)", err)
	}
	eng.Close()

	// A real (tiny) MySQL service migration so the framework + service Up path runs
	// end to end (golang-migrate's file source rejects an empty service dir).
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	// The service source has its own version space + tracking table
	// (omnicore_migrations), independent of the framework's version 1, so the
	// service's first migration is version 1 here.
	write("0001_boot_probe.up.sql", "CREATE TABLE IF NOT EXISTS bootstrap_probe (id INT PRIMARY KEY);")
	write("0001_boot_probe.down.sql", "DROP TABLE IF EXISTS bootstrap_probe;")

	cfg := mysqlBootConfig(t, dir)
	deps, err := buildDeps(cfg)
	if err != nil {
		t.Fatalf("buildDeps: %v", err)
	}
	t.Cleanup(func() {
		deps.DB.Close()
		_ = deps.Mongo.Close(context.Background())
	})

	// The proof: applyMigrations on a MySQL engine picks the MySQL runner
	// (newMigrator in engine_mysql.go → migration.NewMySQL) and runs to completion.
	// The Postgres runner (migration.New over the pgx pool) is not even linked in a
	// MySQL build, so a clean, nil return is the dialect-selected runner working.
	// (That the MySQL framework+service SQL actually applies is covered by the
	// migration package's own -tags=integration,mysql suite.)
	if err := applyMigrations(context.Background(), cfg, deps); err != nil {
		t.Fatalf("applyMigrations on MySQL failed: %v", err)
	}

	// Reset the probe table so a re-run is clean (the service tracking row in
	// omnicore_migrations persists across runs by design).
	if raw, err := sql.Open("mysql", mysqlBootDSN()); err == nil {
		_, _ = raw.ExecContext(context.Background(), "DROP TABLE IF EXISTS bootstrap_probe")
		_ = raw.Close()
	}
}
