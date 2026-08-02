//go:build sqlite

package sqlite

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveDSN_Memory_ForcesCorrectnessSkipsTuning(t *testing.T) {
	got, err := resolveDSN(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	// Fix #4: :memory: resolves to a SHARED-CACHE NAMED in-memory database so the
	// engine and the migration runner share one database (a bare ":memory:" is
	// private per connection/pool). Correctness pragmas forced; NO WAL/busy_timeout
	// (meaningless in RAM).
	if !strings.HasPrefix(got, "file:"+SharedMemoryName+"?") {
		t.Errorf("memory DSN should resolve to the shared named db, got %q", got)
	}
	mustContain(t, got, "mode=memory")
	mustContain(t, got, "cache=shared")
	mustContain(t, got, "_pragma=foreign_keys(ON)")
	mustContain(t, got, "_pragma=case_sensitive_like(ON)")
	if strings.Contains(got, "journal_mode") || strings.Contains(got, "busy_timeout") {
		t.Errorf("memory DSN must not default WAL/busy_timeout, got %q", got)
	}
}

func TestResolveDSN_AbsolutePath_ForcesAllAndMkdir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "data")
	abs := filepath.Join(dir, "app.db")
	got, err := resolveDSN("file:" + abs)
	if err != nil {
		t.Fatal(err)
	}
	// Absolute path used verbatim; parent directory created.
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("expected the parent directory to be created: %v", err)
	}
	mustContain(t, got, "file:"+abs+"?")
	mustContain(t, got, "_pragma=foreign_keys(ON)")
	mustContain(t, got, "_pragma=case_sensitive_like(ON)")
	mustContain(t, got, "_pragma=journal_mode(WAL)")
	mustContain(t, got, "_pragma=busy_timeout(5000)")
}

func TestResolveDSN_DevPragmasPreservedAndForcedOverridden(t *testing.T) {
	abs := filepath.Join(t.TempDir(), "app.db")
	// Dev sets a custom busy_timeout AND tries to disable foreign_keys.
	got, err := resolveDSN("file:" + abs + "?_pragma=busy_timeout(9999)&_pragma=foreign_keys(OFF)")
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, got, "_pragma=busy_timeout(9999)") // dev tuning preserved
	if strings.Contains(got, "busy_timeout(5000)") {
		t.Errorf("dev busy_timeout must not be re-defaulted, got %q", got)
	}
	mustContain(t, got, "_pragma=foreign_keys(ON)") // forced ON
	if strings.Contains(got, "foreign_keys(OFF)") {
		t.Errorf("dev foreign_keys(OFF) must be overridden to ON, got %q", got)
	}
}

func TestResolveFilePathAgainst_RelativeJoinsBaseAndMkdir(t *testing.T) {
	base := t.TempDir()
	got, err := resolveFilePathAgainst(base, filepath.Join("nested", "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(base, "nested", "app.db")
	if got != want {
		t.Errorf("relative should join onto base: got %q, want %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(base, "nested")); err != nil {
		t.Errorf("expected the parent directory to be created: %v", err)
	}
}

func TestResolveFilePathAgainst_AbsoluteIgnoresBase(t *testing.T) {
	abs := filepath.Join(t.TempDir(), "app.db")
	got, err := resolveFilePathAgainst(filepath.Join(t.TempDir(), "other-base"), abs)
	if err != nil {
		t.Fatal(err)
	}
	if got != abs {
		t.Errorf("absolute path must be used verbatim regardless of base: got %q, want %q", got, abs)
	}
}

func TestIsEphemeralExeDir(t *testing.T) {
	// A directory inside the OS temp tree — the go run / go test case.
	if !isEphemeralExeDir(t.TempDir()) {
		t.Error("a directory under os.TempDir() must be classified ephemeral")
	}
	// The working directory (the package dir under the repo) is not ephemeral.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if isEphemeralExeDir(wd) {
		t.Errorf("the working directory %q must not be classified ephemeral", wd)
	}
	// The "go-build" name is the symlink-proof fallback signal.
	if !isEphemeralExeDir(filepath.Join("some", "go-build123", "b001", "exe")) {
		t.Error("a go-build* path must be classified ephemeral via the name fallback")
	}
}

func TestResolveDSN_RelativePath_ResolvesToAbsolute(t *testing.T) {
	// The test binary itself is ephemeral (go test compiles to a temp dir), so a
	// relative DSN must resolve against the working directory, NOT be left bare —
	// proving the .db lands somewhere persistent, never beside the throwaway exe.
	got, err := resolveDSN("file:app.db")
	if err != nil {
		t.Fatal(err)
	}
	path, _ := splitDSN(got)
	if !filepath.IsAbs(path) {
		t.Errorf("a relative sqlite DSN must resolve to an absolute path, got %q", path)
	}
	if !strings.HasSuffix(path, "app.db") {
		t.Errorf("resolved path should keep the file name, got %q", path)
	}
}

func TestSplitDSN(t *testing.T) {
	cases := []struct{ in, path, params string }{
		{"file:app.db?_pragma=x(1)", "app.db", "_pragma=x(1)"},
		{"app.db", "app.db", ""},
		{"file::memory:?mode=memory", ":memory:", "mode=memory"},
		{"/abs/app.db", "/abs/app.db", ""},
	}
	for _, c := range cases {
		p, q := splitDSN(c.in)
		if p != c.path || q != c.params {
			t.Errorf("splitDSN(%q) = (%q,%q), want (%q,%q)", c.in, p, q, c.path, c.params)
		}
	}
}

func TestIsMemoryPath(t *testing.T) {
	if !isMemoryPath(":memory:", "") {
		t.Error(":memory: should be memory")
	}
	if !isMemoryPath("x", "mode=memory&cache=shared") {
		t.Error("mode=memory should be memory")
	}
	if isMemoryPath("app.db", "_pragma=foreign_keys(ON)") {
		t.Error("a file path is not memory")
	}
}

func TestWithForcedPragmas(t *testing.T) {
	// Dev provides a journal_mode + a foreign_keys(OFF) they should not win.
	got := withForcedPragmas([]string{"journal_mode(MEMORY)", "foreign_keys(OFF)"}, false)
	joined := strings.Join(got, "|")
	mustContain(t, joined, "journal_mode(MEMORY)")    // dev journal_mode preserved (present → not re-defaulted)
	mustContain(t, joined, "foreign_keys(ON)")        // forced
	mustContain(t, joined, "case_sensitive_like(ON)") // forced
	mustContain(t, joined, "busy_timeout(5000)")      // defaulted (dev omitted it)
	if strings.Contains(joined, "foreign_keys(OFF)") {
		t.Errorf("forced foreign_keys must override dev OFF, got %q", joined)
	}
	if strings.Contains(joined, "journal_mode(WAL)") {
		t.Errorf("dev journal_mode present → WAL must not be defaulted, got %q", joined)
	}

	// Memory: no tuning defaults at all.
	mem := strings.Join(withForcedPragmas(nil, true), "|")
	if strings.Contains(mem, "journal_mode") || strings.Contains(mem, "busy_timeout") {
		t.Errorf("memory must get no tuning defaults, got %q", mem)
	}
	mustContain(t, mem, "foreign_keys(ON)")
}

func TestPragmaName(t *testing.T) {
	if got := pragmaName("journal_mode(WAL)"); got != "journal_mode" {
		t.Errorf("pragmaName = %q", got)
	}
	if got := pragmaName("bare"); got != "bare" {
		t.Errorf("pragmaName bare = %q", got)
	}
}

func mustContain(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Errorf("expected %q to contain %q", haystack, needle)
	}
}
