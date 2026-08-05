package migration

import (
	"os"
	"path/filepath"
	"strings"
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

// Fix #14: frameworkDialects returns the linked dialects DETERMINISTICALLY
// (sorted), so a multi-engine build picks the same "representative" dialect on
// every run instead of a random map-iteration winner.
func TestFrameworkDialects_DeterministicSorted(t *testing.T) {
	a := frameworkDialects()
	if len(a) == 0 {
		t.Fatal("expected at least one linked framework dialect")
	}
	b := frameworkDialects()
	if len(a) != len(b) {
		t.Fatalf("non-deterministic length: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("non-deterministic order at %d: %q vs %q", i, a[i], b[i])
		}
		if i > 0 && a[i-1] >= a[i] {
			t.Fatalf("dialects not strictly sorted: %q not before %q", a[i-1], a[i])
		}
	}
}

// serviceSource reads numbered migrations from a directory, converting a
// relative path to absolute before serving it via iofs over os.DirFS.
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

// The migration directory is a filesystem path, never URL material. A path
// containing URL-significant characters (space, %XX, non-ASCII — every Windows
// drive-letter path is also in this class) must resolve exactly as given; the
// old "file://"+path round-trip percent-decoded %20 into a space and read a
// Windows C:\ prefix as host:port, so it could not open this directory.
func TestServiceSource_URLSignificantCharactersInPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Área de Trabalho %20 test")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	writeMigration(t, dir, "0001_init.up.sql", "CREATE TABLE t();")
	writeMigration(t, dir, "0001_init.down.sql", "DROP TABLE t;")

	drv, err := serviceSource(dir)
	if err != nil {
		t.Fatalf("serviceSource: unexpected error %v", err)
	}
	first, err := drv.First()
	if err != nil {
		t.Fatalf("First: unexpected error %v", err)
	}
	if first != 1 {
		t.Fatalf("expected first service version 1, got %d", first)
	}
}

// A non-existent directory must surface as a descriptive error from the
// underlying source.
func TestServiceSource_MissingDirectoryReturnsError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if _, err := serviceSource(missing); err == nil {
		t.Fatal("expected error opening a non-existent migration directory")
	}
}

// A dialect not present in the tag-gated embed registry must fail with the
// "no embedded framework migrations" error — the registry guard now runs before
// fs.Sub (the embed FS is per-dialect and self-registered by embed_<dialect>.go,
// so an unlinked/unknown dialect is rejected up front rather than forming an
// invalid subpath).
func TestFrameworkSourceFor_UnregisteredDialectReturnsError(t *testing.T) {
	_, err := frameworkSourceFor("nonesuch")
	if err == nil {
		t.Fatal("expected error for an unregistered dialect")
	}
	if !strings.Contains(err.Error(), "no embedded framework migrations") {
		t.Fatalf("expected the registry-miss error, got: %v", err)
	}
}

func writeMigration(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}
