//go:build sqlite

package sqlite

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DSN handling for the SQLite engine (modernc.org/sqlite driver). Two jobs the
// factory delegates here (tasks/sql_mvp.md §A.9, D11 + D12):
//
//  1. FORCE the correctness pragmas — foreign_keys(ON) and case_sensitive_like(ON)
//     — into the DSN regardless of what the dev wrote. Without foreign_keys the
//     shared-base orphan-purge veto (IsForeignKeyViolation) is a no-op; without
//     case_sensitive_like a bare LIKE is ASCII-case-insensitive and OpLike breaks
//     its contract. These are framework invariants, not conveniences, so the dev
//     never has to remember them (the MySQL factory normalizes its DSN the same
//     way). journal_mode(WAL) + busy_timeout(5000) are defaulted only when the dev
//     did not set them — those are tuning knobs the dev may override.
//
//  2. RESOLVE the file path. A relative path resolves NEXT TO THE BINARY
//     (filepath.Dir(os.Executable())) so the single-binary MVP is portable — the
//     .db travels beside the executable (a USB stick, any mount point). The one
//     exception is an EPHEMERAL binary: under `go run` / `go test` the executable
//     is a throwaway temp file deleted on exit, so resolving beside it would lose
//     the data — there the working directory (the project) is the base instead.
//     An absolute path is used verbatim (fixed external location). Either way the
//     parent directory is created. ":memory:" resolves to a shared-cache NAMED
//     in-memory database (normalizeMemoryDSN) so the engine and the migration
//     runner share one database, not two private ones.

// forcedCorrectnessPragmas are injected on every connection, overriding any
// dev-supplied value — these are correctness invariants (D11).
var forcedCorrectnessPragmas = []string{"foreign_keys(ON)", "case_sensitive_like(ON)"}

// defaultTuningPragmas are applied only when the dev did not set that pragma —
// overridable performance knobs, and skipped entirely for an in-memory database
// (WAL/busy_timeout are meaningless there).
var defaultTuningPragmas = []string{"journal_mode(WAL)", "busy_timeout(5000)"}

// resolveDSN turns the raw relational.dsn into the DSN handed to sql.Open,
// applying path resolution (D12) and pragma forcing (D11). It never fails on a
// missing file (SQLite creates it); it fails only if the parent directory can
// not be created.
func resolveDSN(raw string) (string, error) {
	path, params := splitDSN(raw)
	memory := isMemoryPath(path, params)

	pragmas, others := partitionParams(params)
	if memory {
		path, others = normalizeMemoryDSN(path, others)
	} else {
		resolved, err := resolveFilePath(path)
		if err != nil {
			return "", err
		}
		path = resolved
	}

	pragmas = withForcedPragmas(pragmas, memory)

	return assembleDSN(path, pragmas, others), nil
}

// SharedMemoryName is the database name a bare ":memory:" DSN resolves to.
// migration.sqliteMigrateDSN mirrors this — the migration runner and the engine
// MUST land on the same in-memory database (see normalizeMemoryDSN).
const SharedMemoryName = "omnicore_mem"

// normalizeMemoryDSN turns an in-memory request into a SHARED-CACHE, NAMED
// in-memory database: "<name>" + mode=memory + cache=shared. A bare ":memory:"
// is private to a single connection, so the engine's pinned connection and the
// migration runner's SEPARATE *sql.DB pool would otherwise open two different
// empty databases — migrations would land in one the engine never reads, and the
// service would boot against an unmigrated store. A shared-cache named memory DB
// is one database across every connection and pool in the process, kept alive by
// the engine's perennial connection (MaxOpenConns=1, never recycled). A bare
// ":memory:" (or empty) resolves to SharedMemoryName; an explicitly named
// mode=memory DSN keeps its name and just gains cache=shared.
func normalizeMemoryDSN(path string, others []string) (string, []string) {
	if path == ":memory:" || path == "" {
		path = SharedMemoryName
	}
	others = ensureParam(others, "mode=memory")
	others = ensureParam(others, "cache=shared")
	return path, others
}

// ensureParam appends key=value only when its key is not already present.
func ensureParam(params []string, kv string) []string {
	key := kv
	if i := strings.IndexByte(kv, '='); i >= 0 {
		key = kv[:i+1]
	}
	for _, p := range params {
		if strings.HasPrefix(p, key) {
			return params
		}
	}
	return append(params, kv)
}

// splitDSN separates the file path from the query parameters, tolerating both
// the bare form ("app.db?x=y") and the URI form ("file:app.db?x=y" /
// "file::memory:?x=y"). Returns the path (with any "file:" scheme stripped) and
// the raw "&"-joined parameter list (without the leading "?").
func splitDSN(raw string) (path, params string) {
	s := strings.TrimPrefix(raw, "file:")
	if i := strings.IndexByte(s, '?'); i >= 0 {
		return s[:i], s[i+1:]
	}
	return s, ""
}

// isMemoryPath reports whether the DSN targets an in-memory database — the
// ":memory:" path or an explicit mode=memory parameter.
func isMemoryPath(path, params string) bool {
	return path == ":memory:" || strings.Contains(params, "mode=memory")
}

// resolveFilePath makes a relative path absolute against the resolution base
// (D12) and ensures its parent exists. An absolute path is used verbatim.
func resolveFilePath(path string) (string, error) {
	return resolveFilePathAgainst(resolutionBase(), path)
}

// resolveFilePathAgainst joins a relative path onto base (an absolute path is
// used verbatim) and MkdirAll's the parent. Split out from resolveFilePath so
// the base — which depends on os.Executable() and cannot be faked in a test —
// is chosen once by resolutionBase while this pure join+mkdir step stays
// directly testable with an explicit base.
func resolveFilePathAgainst(base, path string) (string, error) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(base, path)
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("sqlite: creating database directory %q: %w", dir, err)
		}
	}
	return path, nil
}

// resolutionBase is the directory a RELATIVE sqlite path resolves against: the
// binary's own directory — so the self-executable MVP keeps its .db beside the
// binary and travels as one unit (a USB stick, wherever it is mounted) — EXCEPT
// when the binary is ephemeral (`go run` / `go test` compile to a temp file the
// toolchain deletes on exit), where the working directory (the project) is used
// so the dev loop persists where the developer expects. If os.Executable()
// fails (rare), the working directory is the fallback.
func resolutionBase() string {
	exe, err := os.Executable()
	if err != nil {
		return workingDirOrDot()
	}
	exeDir := filepath.Dir(exe)
	if isEphemeralExeDir(exeDir) {
		return workingDirOrDot()
	}
	return exeDir
}

// workingDirOrDot returns the process working directory, or "." if it cannot be
// determined (filepath.Join tolerates "." — SQLite then opens relative to CWD).
func workingDirOrDot() string {
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "."
}

// isEphemeralExeDir reports whether an executable directory belongs to a
// throwaway build — the `go run` / `go test` case. Both compile the binary
// under the OS temp directory (a "go-build*" subtree), so a dir that lives
// inside os.TempDir() is ephemeral; the "go-build" name is a symlink-proof
// fallback when the temp comparison is inconclusive.
func isEphemeralExeDir(dir string) bool {
	if tmp, err := filepath.EvalSymlinks(os.TempDir()); err == nil {
		if d, err := filepath.EvalSymlinks(dir); err == nil {
			if rel, err := filepath.Rel(tmp, d); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return true
			}
		}
	}
	return strings.Contains(dir, "go-build")
}

// partitionParams splits the raw parameter list into the _pragma values (the
// pragma name+arg, e.g. "journal_mode(WAL)") and every other parameter kept
// verbatim (e.g. "mode=memory", "cache=shared").
func partitionParams(params string) (pragmas, others []string) {
	if params == "" {
		return nil, nil
	}
	for _, p := range strings.Split(params, "&") {
		if p == "" {
			continue
		}
		if v, ok := strings.CutPrefix(p, "_pragma="); ok {
			pragmas = append(pragmas, v)
			continue
		}
		others = append(others, p)
	}
	return pragmas, others
}

// withForcedPragmas drops any dev-supplied correctness pragma, appends the
// forced ON forms (D11), and defaults the tuning pragmas the dev omitted (unless
// in-memory). pragmaName extracts the name before "(" for the dedup check.
func withForcedPragmas(devPragmas []string, memory bool) []string {
	out := make([]string, 0, len(devPragmas)+len(forcedCorrectnessPragmas)+len(defaultTuningPragmas))
	forced := map[string]bool{"foreign_keys": true, "case_sensitive_like": true}
	present := map[string]bool{}
	for _, p := range devPragmas {
		name := pragmaName(p)
		if forced[name] {
			continue // dropped — the forced ON form is appended below
		}
		present[name] = true
		out = append(out, p)
	}
	out = append(out, forcedCorrectnessPragmas...)
	if !memory {
		for _, d := range defaultTuningPragmas {
			if !present[pragmaName(d)] {
				out = append(out, d)
			}
		}
	}
	return out
}

// pragmaName returns the pragma name preceding the "(" — "journal_mode" from
// "journal_mode(WAL)". A value with no "(" returns unchanged.
func pragmaName(p string) string {
	if i := strings.IndexByte(p, '('); i >= 0 {
		return p[:i]
	}
	return p
}

// assembleDSN rebuilds the modernc DSN: "file:<path>?_pragma=…&<others>". The
// parameters are joined by hand (not url.Values.Encode) so the pragma parens are
// NOT percent-escaped — modernc reads them literally.
func assembleDSN(path string, pragmas, others []string) string {
	var b strings.Builder
	b.WriteString("file:")
	b.WriteString(path)
	parts := make([]string, 0, len(pragmas)+len(others))
	for _, p := range pragmas {
		parts = append(parts, "_pragma="+p)
	}
	parts = append(parts, others...)
	if len(parts) > 0 {
		b.WriteByte('?')
		b.WriteString(strings.Join(parts, "&"))
	}
	return b.String()
}
