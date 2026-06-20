package migration

import (
	"os"
	"path/filepath"
	"testing"
)

// frameworkSource exposes the embedded framework migrations. The driver must
// open cleanly and report the first embedded version (0001_outbox).
func TestFrameworkSource_OpensEmbeddedDriver(t *testing.T) {
	drv, err := frameworkSource()
	if err != nil {
		t.Fatalf("frameworkSource: unexpected error %v", err)
	}
	first, err := drv.First()
	if err != nil {
		t.Fatalf("First: unexpected error %v", err)
	}
	if first != 1 {
		t.Fatalf("expected first embedded version 1, got %d", first)
	}
}

// serviceSource reads numbered migrations from a directory, converting a
// relative path to absolute before handing it to source/file.
func TestServiceSource_OpensDirectoryDriver(t *testing.T) {
	dir := t.TempDir()
	writeMigration(t, dir, "0002_create_users.up.sql", "CREATE TABLE users();")
	writeMigration(t, dir, "0002_create_users.down.sql", "DROP TABLE users;")

	drv, err := serviceSource(dir)
	if err != nil {
		t.Fatalf("serviceSource: unexpected error %v", err)
	}
	first, err := drv.First()
	if err != nil {
		t.Fatalf("First: unexpected error %v", err)
	}
	if first != 2 {
		t.Fatalf("expected first service version 2, got %d", first)
	}
}

// A non-existent directory must surface as a descriptive error from the
// underlying file source.
func TestServiceSource_MissingDirectoryReturnsError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if _, err := serviceSource(missing); err == nil {
		t.Fatal("expected error opening a non-existent migration directory")
	}
}

func writeMigration(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}
