//go:build integration && postgres

// Integration tests for the migration package. Run with:
//
//	go test -tags=integration ./infra/migration/...
//
// Requires Postgres reachable on OMNICORE_TEST_PG_DSN (default points at the
// example service's local docker-compose:
// postgres://omnicore:omnicore@localhost:5433/postgres?sslmode=disable). Each
// test creates its own throw-away database so parallel and repeat runs don't
// stomp on each other or on the example's QA suites.
package migration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const defaultAdminDSN = "postgres://omnicore:omnicore@localhost:5433/postgres?sslmode=disable"

// adminDSN returns the DSN used to create / drop test databases. Override via
// the OMNICORE_TEST_PG_DSN env var when the local bench listens elsewhere.
func adminDSN() string {
	if v := os.Getenv("OMNICORE_TEST_PG_DSN"); v != "" {
		return v
	}
	return defaultAdminDSN
}

// newTestDB creates a unique throw-away database, returns a connection pool to
// it plus a cleanup func that drops the database. Caller calls cleanup() in a
// defer.
func newTestDB(t *testing.T) (*pgxpool.Pool, func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dbName := fmt.Sprintf("omnicore_test_%s", strings.ReplaceAll(uuid.NewString(), "-", ""))

	adminPool, err := pgxpool.New(ctx, adminDSN())
	if err != nil {
		t.Skipf("skipping integration test: cannot reach Postgres at %s (%v) — set OMNICORE_TEST_PG_DSN or run `docker compose -f omnicore-example-users/devops/docker-compose.yml up -d`", adminDSN(), err)
	}
	defer adminPool.Close()

	if _, err := adminPool.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %q`, dbName)); err != nil {
		t.Fatalf("CREATE DATABASE %q failed: %v", dbName, err)
	}

	testDSN := withDatabase(adminDSN(), dbName)
	pool, err := pgxpool.New(ctx, testDSN)
	if err != nil {
		dropDatabase(adminPool, dbName)
		t.Fatalf("connect to %q failed: %v", dbName, err)
	}

	cleanup := func() {
		pool.Close()
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		adminPool, err := pgxpool.New(cleanupCtx, adminDSN())
		if err != nil {
			t.Logf("cleanup: cannot reach admin DB to drop %q: %v", dbName, err)
			return
		}
		defer adminPool.Close()
		dropDatabase(adminPool, dbName)
	}
	return pool, cleanup
}

func dropDatabase(adminPool *pgxpool.Pool, dbName string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Terminate any leftover connection from migrate.Migrate.Close() pgxdriver
	// path so DROP DATABASE doesn't hang on lingering backends.
	_, _ = adminPool.Exec(ctx, `
		SELECT pg_terminate_backend(pid)
		FROM pg_stat_activity
		WHERE datname = $1 AND pid <> pg_backend_pid()`, dbName)
	_, _ = adminPool.Exec(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %q`, dbName))
}

// withDatabase swaps the database name in a postgres DSN. The DSN may be in
// "postgres://" URL form (only form used in the test bench).
func withDatabase(dsn, db string) string {
	idx := strings.LastIndex(dsn, "/")
	q := strings.Index(dsn, "?")
	if idx == -1 || q == -1 || q < idx {
		return dsn
	}
	return dsn[:idx+1] + db + dsn[q:]
}

// --- service migration files generator -------------------------------------

func writeService(t *testing.T, version int, name, up, down string) string {
	t.Helper()
	dir := t.TempDir()
	writeFile := func(suffix, body string) {
		path := filepath.Join(dir, fmt.Sprintf("%04d_%s.%s.sql", version, name, suffix))
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	writeFile("up", up)
	writeFile("down", down)
	return dir
}

// --- tests -----------------------------------------------------------------

func TestUp_ApplyFrameworkAndService(t *testing.T) {
	pool, cleanup := newTestDB(t)
	defer cleanup()

	dir := writeService(t, 2, "create_users",
		`CREATE TABLE t_users (id UUID PRIMARY KEY, name TEXT NOT NULL);`,
		`DROP TABLE t_users;`)

	mgr := New(pool, dir)
	if err := mgr.Up(context.Background()); err != nil {
		t.Fatalf("Up failed: %v", err)
	}

	// Outbox table from the framework migration should exist.
	if !tableExists(t, pool, "outbox") {
		t.Error("expected 'outbox' table after framework migration")
	}
	if !tableExists(t, pool, "omnicore_mongo_views") {
		t.Error("expected 'omnicore_mongo_views' table after framework migration")
	}
	if !tableExists(t, pool, "t_users") {
		t.Error("expected 't_users' table after service migration")
	}
	if !tableExists(t, pool, "omnicore_framework_migrations") {
		t.Error("expected framework tracking table")
	}
	if !tableExists(t, pool, "omnicore_migrations") {
		t.Error("expected service tracking table")
	}
}

func TestUp_IsIdempotent(t *testing.T) {
	pool, cleanup := newTestDB(t)
	defer cleanup()

	dir := writeService(t, 2, "noop_one",
		`CREATE TABLE t_one (id INT);`,
		`DROP TABLE t_one;`)

	mgr := New(pool, dir)
	if err := mgr.Up(context.Background()); err != nil {
		t.Fatalf("first Up failed: %v", err)
	}
	if err := mgr.Up(context.Background()); err != nil {
		t.Fatalf("second Up should be a no-op, got %v", err)
	}
}

func TestStatus_NoMigrationsAppliedReturnsZero(t *testing.T) {
	pool, cleanup := newTestDB(t)
	defer cleanup()

	// No migrations on disk → an empty Status query should not error.
	dir := t.TempDir()
	mgr := New(pool, dir)
	v, dirty, err := mgr.Status(context.Background())
	if err != nil {
		t.Fatalf("Status() err = %v", err)
	}
	if v != 0 || dirty {
		t.Errorf("Status() = (%d, %v), want (0, false)", v, dirty)
	}
}

func TestStatus_AfterUpReportsLatest(t *testing.T) {
	pool, cleanup := newTestDB(t)
	defer cleanup()

	dir := writeService(t, 7, "with_v7",
		`CREATE TABLE t_seven (id INT);`,
		`DROP TABLE t_seven;`)

	mgr := New(pool, dir)
	if err := mgr.Up(context.Background()); err != nil {
		t.Fatalf("Up failed: %v", err)
	}
	v, dirty, err := mgr.Status(context.Background())
	if err != nil {
		t.Fatalf("Status() err = %v", err)
	}
	if v != 7 || dirty {
		t.Errorf("Status() = (%d, %v), want (7, false)", v, dirty)
	}
}

func TestDown_StepsAreValidated(t *testing.T) {
	pool, cleanup := newTestDB(t)
	defer cleanup()

	dir := writeService(t, 2, "skip", `SELECT 1;`, `SELECT 1;`)
	mgr := New(pool, dir)
	if err := mgr.Down(context.Background(), 0); err == nil {
		t.Error("Down(0) should fail")
	}
	if err := mgr.Down(context.Background(), -1); err == nil {
		t.Error("Down(-1) should fail")
	}
}

func TestDown_RevertsService(t *testing.T) {
	pool, cleanup := newTestDB(t)
	defer cleanup()

	dir := writeService(t, 2, "create_then_drop",
		`CREATE TABLE t_drop (id INT);`,
		`DROP TABLE t_drop;`)

	mgr := New(pool, dir)
	if err := mgr.Up(context.Background()); err != nil {
		t.Fatalf("Up failed: %v", err)
	}
	if !tableExists(t, pool, "t_drop") {
		t.Fatal("expected t_drop after Up")
	}
	if err := mgr.Down(context.Background(), 1); err != nil {
		t.Fatalf("Down failed: %v", err)
	}
	if tableExists(t, pool, "t_drop") {
		t.Error("expected t_drop to be gone after Down")
	}
	// Down does NOT touch the framework table; outbox must survive.
	if !tableExists(t, pool, "outbox") {
		t.Error("Down should not revert the framework outbox migration")
	}
}

func TestPending_ListsFutureFiles(t *testing.T) {
	pool, cleanup := newTestDB(t)
	defer cleanup()

	dir := t.TempDir()
	write := func(version int, name string) {
		path := filepath.Join(dir, fmt.Sprintf("%04d_%s.up.sql", version, name))
		if err := os.WriteFile(path, []byte("SELECT 1;"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		down := filepath.Join(dir, fmt.Sprintf("%04d_%s.down.sql", version, name))
		if err := os.WriteFile(down, []byte("SELECT 1;"), 0o644); err != nil {
			t.Fatalf("write down: %v", err)
		}
	}
	write(2, "a")
	write(3, "b")
	write(4, "c")

	mgr := New(pool, dir)
	pending, err := mgr.Pending(context.Background())
	if err != nil {
		t.Fatalf("Pending err = %v", err)
	}
	if got := pending; len(got) != 3 || got[0] != 2 || got[2] != 4 {
		t.Errorf("Pending = %v, want [2 3 4]", got)
	}

	// After applying 2+3 (not 4 — splice that file out before Up), Pending
	// should drop them.
	// Quick simulation: copy current files into a fresh dir without "0004_c",
	// run Up there, then back-fill 4 and Pending should report only [4].
	dir2 := t.TempDir()
	for _, base := range []string{"0002_a", "0003_b"} {
		for _, suffix := range []string{"up", "down"} {
			body, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("%s.%s.sql", base, suffix)))
			if err != nil {
				t.Fatalf("readback: %v", err)
			}
			path := filepath.Join(dir2, fmt.Sprintf("%s.%s.sql", base, suffix))
			if err := os.WriteFile(path, body, 0o644); err != nil {
				t.Fatalf("writeback: %v", err)
			}
		}
	}
	mgr2 := New(pool, dir2)
	if err := mgr2.Up(context.Background()); err != nil {
		t.Fatalf("Up after splice: %v", err)
	}

	// Re-issue mgr (the original dir has 0004 too) — must report [4].
	pending2, err := mgr.Pending(context.Background())
	if err != nil {
		t.Fatalf("Pending after Up: %v", err)
	}
	if len(pending2) != 1 || pending2[0] != 4 {
		t.Errorf("Pending after Up = %v, want [4]", pending2)
	}
}

func TestPending_MissingDirReturnsNil(t *testing.T) {
	pool, cleanup := newTestDB(t)
	defer cleanup()

	mgr := New(pool, filepath.Join(t.TempDir(), "does-not-exist"))
	// Status needs the dir; on missing dir golang-migrate will surface a
	// source-open error from openService. To trigger the os.IsNotExist branch
	// of Pending specifically, status must succeed first, which can't happen
	// when the dir doesn't exist. We instead exercise the secondary code path
	// where the dir exists but is empty (status -> 0, ReadDir -> empty slice
	// -> no entries -> empty result).
	emptyDir := t.TempDir()
	mgr = New(pool, emptyDir)
	pending, err := mgr.Pending(context.Background())
	if err != nil {
		t.Fatalf("Pending on empty dir err = %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("Pending on empty dir = %v, want []", pending)
	}
}

func TestPending_IgnoresInvalidFilenames(t *testing.T) {
	pool, cleanup := newTestDB(t)
	defer cleanup()

	dir := t.TempDir()
	// Valid migration files.
	mustWrite(t, dir, "0005_real.up.sql", "SELECT 1;")
	mustWrite(t, dir, "0005_real.down.sql", "SELECT 1;")
	// Noise that Pending should silently skip.
	mustWrite(t, dir, "garbage.up.sql", "SELECT 1;")
	mustWrite(t, dir, "README.md", "ignore me")
	mustWrite(t, dir, "0007_other.down.sql", "SELECT 1;") // .down without .up

	mgr := New(pool, dir)
	pending, err := mgr.Pending(context.Background())
	if err != nil {
		t.Fatalf("Pending err = %v", err)
	}
	if len(pending) != 1 || pending[0] != 5 {
		t.Errorf("Pending = %v, want [5]", pending)
	}
}

func TestForce_ResetsTrackingPointer(t *testing.T) {
	pool, cleanup := newTestDB(t)
	defer cleanup()

	dir := writeService(t, 2, "force",
		`CREATE TABLE t_force (id INT);`,
		`DROP TABLE t_force;`)

	mgr := New(pool, dir)
	if err := mgr.Up(context.Background()); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if err := mgr.Force(context.Background(), 99); err != nil {
		t.Fatalf("Force err = %v", err)
	}
	v, dirty, err := mgr.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if v != 99 || dirty {
		t.Errorf("Status after Force(99) = (%d, %v), want (99, false)", v, dirty)
	}
}

func TestValidateDownExists_MissingDirOK(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does-not-exist")
	mgr := New(nil, dir)
	if err := mgr.ValidateDownExists(); err != nil {
		t.Errorf("missing dir should be OK, got %v", err)
	}
}

func TestValidateDownExists_EmptyDirOK(t *testing.T) {
	dir := t.TempDir()
	mgr := New(nil, dir)
	if err := mgr.ValidateDownExists(); err != nil {
		t.Errorf("empty dir should be OK, got %v", err)
	}
}

func TestValidateDownExists_AllPaired(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, dir, "0002_init.up.sql", "SELECT 1;")
	mustWrite(t, dir, "0002_init.down.sql", "SELECT 1;")
	mustWrite(t, dir, "0003_more.up.sql", "SELECT 1;")
	mustWrite(t, dir, "0003_more.down.sql", "SELECT 1;")

	mgr := New(nil, dir)
	if err := mgr.ValidateDownExists(); err != nil {
		t.Errorf("paired files should pass, got %v", err)
	}
}

func TestValidateDownExists_MissingDownReportsNotification(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, dir, "0002_init.up.sql", "SELECT 1;")
	mustWrite(t, dir, "0002_init.down.sql", "SELECT 1;")
	mustWrite(t, dir, "0003_orphan.up.sql", "SELECT 1;")
	// No 0003_orphan.down.sql.

	mgr := New(nil, dir)
	err := mgr.ValidateDownExists()
	if err == nil {
		t.Fatal("expected ValidateDownExists to fail when down is missing")
	}
	// The offending file lives in the underlying cause (attached via
	// FieldErrorWithCause); the carrier's Error() only surfaces the context
	// count, so inspect the cause directly.
	if !errMentions(err, "0003_orphan.up.sql") {
		t.Errorf("expected the offending file in the error chain, got %v", err)
	}
}

// errMentions walks the error chain (errors.Unwrap) AND the NotificationCarrier
// shape looking for substr anywhere. Useful for *InfrastructureError, whose
// top-level Error() only carries the context count; the offending detail lives
// in NotificationMessage.Err on the wrapped notification.
func errMentions(err error, substr string) bool {
	for cur := err; cur != nil; cur = errors.Unwrap(cur) {
		if strings.Contains(cur.Error(), substr) {
			return true
		}
	}
	var carrier domain.NotificationCarrier
	if errors.As(err, &carrier) {
		for _, ctx := range carrier.NotificationContexts() {
			for _, msg := range ctx.Messages() {
				if msg.Err != nil && strings.Contains(msg.Err.Error(), substr) {
					return true
				}
				if strings.Contains(msg.FieldName, substr) || strings.Contains(msg.FieldValue, substr) {
					return true
				}
			}
		}
	}
	return false
}

func TestUp_FailureMarksDirty(t *testing.T) {
	pool, cleanup := newTestDB(t)
	defer cleanup()

	dir := writeService(t, 2, "broken",
		`THIS IS NOT VALID SQL;`,
		`SELECT 1;`)
	mgr := New(pool, dir)
	if err := mgr.Up(context.Background()); err == nil {
		t.Fatal("expected Up to fail on bad SQL")
	}
	_, dirty, err := mgr.Status(context.Background())
	if err != nil {
		t.Fatalf("Status err = %v", err)
	}
	if !dirty {
		t.Error("expected dirty=true after Up failure")
	}

	// Force recovery clears dirty.
	if err := mgr.Force(context.Background(), 2); err != nil {
		t.Fatalf("Force err = %v", err)
	}
	_, dirty, _ = mgr.Status(context.Background())
	if dirty {
		t.Error("expected dirty=false after Force")
	}
}

func TestUp_ServiceSourceErrorPropagates(t *testing.T) {
	pool, cleanup := newTestDB(t)
	defer cleanup()

	// Non-existent dir → service source open should fail with a fs-not-found
	// error wrapped by migration.runUp.
	mgr := New(pool, filepath.Join(t.TempDir(), "no-such-dir"))
	err := mgr.Up(context.Background())
	if err == nil {
		t.Fatal("expected Up to fail when service dir does not exist")
	}
	// Framework migration runs first, so the error is on the service stage.
	if !strings.Contains(err.Error(), "service") && !errors.Is(err, os.ErrNotExist) {
		t.Errorf("error should mention service stage or be NotExist, got %v", err)
	}
}

// --- helpers ---------------------------------------------------------------

func mustWrite(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func tableExists(t *testing.T, pool *pgxpool.Pool, name string) bool {
	t.Helper()
	var exists bool
	err := pool.QueryRow(context.Background(),
		`SELECT EXISTS (
		   SELECT 1 FROM information_schema.tables
		   WHERE table_schema = 'public' AND table_name = $1
		)`, name).Scan(&exists)
	if err != nil {
		t.Fatalf("tableExists(%q): %v", name, err)
	}
	return exists
}
