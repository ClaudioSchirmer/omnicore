//go:build sqlite

package migration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The migration runner must resolve a RELATIVE sqlite path against the SAME
// base as the engine (infra/db/engine/sqlite.resolutionBase) — binary dir for a
// real binary, working directory for an ephemeral `go run`/`go test` binary.
// Pre-fix the runner joined against the executable's dir unconditionally, so
// under `go run` migrations landed beside the throwaway temp binary while the
// engine served the project-dir file: a green boot over an empty schema.

func TestSQLiteMigrateDSN_RelativeResolvesAgainstWorkingDirUnderGoTest(t *testing.T) {
	// The test binary IS the ephemeral case: it lives under os.TempDir(), so the
	// carve-out must pick the working directory, exactly like the engine.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	got := sqliteMigrateDSN("file:app.db")
	want := "file:" + filepath.Join(wd, "app.db")
	if !strings.HasPrefix(got, want+"?") {
		t.Fatalf("relative path must resolve against the working dir (engine parity): got %q, want prefix %q", got, want+"?")
	}
	if exe, err := os.Executable(); err == nil {
		if strings.HasPrefix(got, "file:"+filepath.Join(filepath.Dir(exe), "app.db")) {
			t.Fatalf("relative path resolved beside the ephemeral test binary — the pre-fix bug: %q", got)
		}
	}
}

func TestSQLiteMigrateDSN_AbsolutePathVerbatim(t *testing.T) {
	abs := filepath.Join(t.TempDir(), "nested", "app.db")
	got := sqliteMigrateDSN("file:" + abs)
	if !strings.HasPrefix(got, "file:"+abs+"?") {
		t.Fatalf("absolute path must be used verbatim: got %q", got)
	}
	// The parent dir is created as a side effect, same as the engine.
	if _, err := os.Stat(filepath.Dir(abs)); err != nil {
		t.Fatalf("parent dir must exist after resolution: %v", err)
	}
}

func TestSQLiteResolutionBase_EphemeralBinaryFallsBackToWorkingDir(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if got := sqliteResolutionBase(); got != wd {
		t.Fatalf("under go test the base must be the working dir, got %q want %q", got, wd)
	}
}

// Mirror of the engine's isEphemeralExeDir cases — the two implementations MUST
// stay in agreement (see the sqliteResolutionBase doc comment).
func TestSQLiteIsEphemeralExeDir(t *testing.T) {
	if !sqliteIsEphemeralExeDir(t.TempDir()) {
		t.Fatal("a dir under os.TempDir() must be ephemeral")
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if sqliteIsEphemeralExeDir(wd) {
		t.Fatal("the working directory must not be ephemeral")
	}
	if !sqliteIsEphemeralExeDir(filepath.Join("some", "go-build123", "b001", "exe")) {
		t.Fatal("a go-build path must be ephemeral (symlink-proof fallback)")
	}
}
