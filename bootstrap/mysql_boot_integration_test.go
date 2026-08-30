//go:build integration && mysql

package bootstrap

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	// Blank import registers the "mysql" dialect in the engine registry — the
	// composition root resolves it by name, so bootstrap never imports it
	// directly outside this tag-guarded test.
	_ "github.com/ClaudioSchirmer/omnicore/infra/db/engine/mysql"

	// The MySQL database/sql driver for the raw admin connection.
	_ "github.com/go-sql-driver/mysql"
)

// Integration test: a MySQL engine boots through the framework composition root.
// buildDeps is backend-neutral — the only Postgres-specific boot wiring (the pgx
// migration runner) lives in bootstrap/engine_postgres.go behind the `postgres`
// build tag, so a MySQL build never compiles or links it. This proves buildDeps
// returns a live MySQL engine, and applyMigrations selects the MySQL runner.
//
// Each test runs against a THROW-AWAY database it creates and drops (the same
// isolation the engine integration suite uses via OMNICORE_TEST_MYSQL_ADMIN_DSN).
// It must NOT touch the shared users_db: the example service migrates its own
// sequence into that database's omnicore_migrations service tracking table, so a
// boot probe pointed there would find a service version the probe's one-migration
// dir cannot satisfy ("no migration found for version N") once the example has
// run — a cross-process collision on a persistent bench volume.
//
//	go test -tags=integration,mysql ./bootstrap/ -run MySQLBoot -count=1
func mysqlBootAdminDSN() string {
	if v := os.Getenv("OMNICORE_TEST_MYSQL_ADMIN_DSN"); v != "" {
		return v
	}
	return "root:root@tcp(127.0.0.1:3307)/?parseTime=true&multiStatements=true"
}

// newBootTestDB creates a throw-away MySQL database and returns its DSN, dropping
// it on cleanup. Skips the test when MySQL is unreachable. This is what keeps the
// boot probe off the shared users_db (and its example-owned service tracking).
func newBootTestDB(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	admin, err := sql.Open("mysql", mysqlBootAdminDSN())
	if err != nil {
		t.Skipf("MySQL not reachable (%v) — start devops/docker-compose.yml mysql service", err)
	}
	if err := admin.PingContext(ctx); err != nil {
		_ = admin.Close()
		t.Skipf("MySQL not reachable (%v) — start devops/docker-compose.yml mysql service", err)
	}
	dbName := "omnicore_mysql_boot_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	return swapBootMySQLDB(mysqlBootAdminDSN(), dbName)
}

// swapBootMySQLDB inserts the database name into a `user:pass@tcp(host:port)/?p=v`
// DSN (between the last '/' and the '?').
func swapBootMySQLDB(adminDSN, name string) string {
	idx := strings.LastIndex(adminDSN, "/")
	q := strings.Index(adminDSN, "?")
	if idx == -1 || q == -1 || q < idx {
		return adminDSN
	}
	return adminDSN[:idx+1] + name + adminDSN[q:]
}

func mongoBootURI() string {
	if v := os.Getenv("OMNICORE_TEST_MONGO_URI"); v != "" {
		return v
	}
	return "mongodb://localhost:27018"
}

func mysqlBootConfig(t *testing.T, dsn, migrationsDir string) *Config {
	cfg := &Config{Service: "mysql-boot-probe"}
	cfg.Relational.Dialect = dialectMySQL
	cfg.Relational.DSN = dsn // dialect + dsn both live under relational:
	cfg.Relational.Clock = "app"
	cfg.Mongo.URI = mongoBootURI()
	cfg.Mongo.Database = "mysql_boot_probe"
	cfg.Migrations.Dir = migrationsDir
	cfg.Migrations.AutoRun = AutoRunTrue
	// Mirror the production load path: LoadConfig always runs applyDefaults before
	// buildDeps, so the relational pool pointers are set. Hand-built configs must
	// do the same or buildDeps dereferences a nil *cfg.Relational.Pool.MaxOpenConns.
	cfg.applyDefaults()
	return cfg
}

func TestMySQLBoot_BuildDepsDoesNotPanic(t *testing.T) {
	dsn := newBootTestDB(t) // throw-away db; skips cleanly when MySQL is down
	cfg := mysqlBootConfig(t, dsn, t.TempDir())

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
	// the postgres-tagged engine package is neither compiled nor linked in a MySQL
	// build, so buildDeps cannot reach for it. The MySQL runner selection is proven
	// end to end by the migration test below.
}

func TestMySQLBoot_ApplyMigrationsUsesMySQLRunner(t *testing.T) {
	dsn := newBootTestDB(t)

	// A real (tiny) MySQL service migration so the framework + service Up path runs
	// end to end (golang-migrate's file source rejects an empty service dir). The
	// throw-away database's omnicore_migrations starts empty, so version 1 applies
	// cleanly — no cross-process version already recorded.
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	write("0001_boot_probe.up.sql", "CREATE TABLE IF NOT EXISTS bootstrap_probe (id INT PRIMARY KEY);")
	write("0001_boot_probe.down.sql", "DROP TABLE IF EXISTS bootstrap_probe;")

	cfg := mysqlBootConfig(t, dsn, dir)
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
	// The Postgres runner (migration.NewPostgres) is not even linked in a MySQL
	// build, so a clean, nil return is the dialect-selected runner working.
	// (That the MySQL framework+service SQL actually applies is covered by the
	// migration package's own -tags=integration,mysql suite.) No manual cleanup of
	// the probe table is needed — the whole throw-away database is dropped.
	if err := applyMigrations(context.Background(), cfg, deps); err != nil {
		t.Fatalf("applyMigrations on MySQL failed: %v", err)
	}
}
