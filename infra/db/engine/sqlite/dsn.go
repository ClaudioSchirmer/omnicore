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
//  2. RESOLVE the file path. An absolute path is used verbatim. A relative path
//     resolves NEXT TO THE BINARY (filepath.Dir(os.Executable())), so the
//     single-binary MVP keeps its .db beside the executable wherever it is
//     launched from — and the parent directory is created (SQLite makes the file
//     on first open but not the directory). ":memory:" is left untouched.

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

	if !memory {
		resolved, err := resolveFilePath(path)
		if err != nil {
			return "", err
		}
		path = resolved
	}

	pragmas, others := partitionParams(params)
	pragmas = withForcedPragmas(pragmas, memory)

	return assembleDSN(path, pragmas, others), nil
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

// resolveFilePath makes a relative path absolute against the binary's directory
// (D12) and ensures its parent exists. An absolute path is used verbatim.
func resolveFilePath(path string) (string, error) {
	if !filepath.IsAbs(path) {
		if exe, err := os.Executable(); err == nil {
			path = filepath.Join(filepath.Dir(exe), path)
		}
		// If os.Executable() fails (rare), fall back to the path as given
		// (working-dir relative) rather than aborting the boot.
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("sqlite: creating database directory %q: %w", dir, err)
		}
	}
	return path, nil
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
