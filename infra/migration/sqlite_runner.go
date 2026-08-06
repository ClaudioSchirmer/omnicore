//go:build sqlite

package migration

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/golang-migrate/migrate/v4/database"
	sqlitedriver "github.com/golang-migrate/migrate/v4/database/sqlite"
	_ "modernc.org/sqlite"
)

// NewSQLite builds a SQLite Manager. Like the other runners it opens its own
// *sql.DB from the DSN rather than sharing the engine's live pool (the discipline
// that keeps the bootstrap wiring backend-neutral). The good news over Oracle —
// which needed a hand-written migrate driver — is that golang-migrate ships a
// PURE-GO database/sqlite driver (backed by modernc.org/sqlite, the same driver
// the engine uses), so this is the shortest runner: no custom driver. Behind the
// `sqlite` build tag so a build without it links neither modernc nor the sqlite
// migrate driver. dir is the service migration directory.
func NewSQLite(dsn, dir string) *Manager {
	return &Manager{
		dialect: "sqlite",
		dir:     dir,
		openDriver: func(trackingTable string) (database.Driver, string, error) {
			db, err := sql.Open("sqlite", sqliteMigrateDSN(dsn))
			if err != nil {
				return nil, "", fmt.Errorf("migration[sqlite]: open: %w", err)
			}
			drv, err := sqlitedriver.WithInstance(db, &sqlitedriver.Config{MigrationsTable: trackingTable})
			if err != nil {
				_ = db.Close()
				return nil, "", fmt.Errorf("migration[sqlite]: driver: %w", err)
			}
			return drv, "sqlite", nil
		},
	}
}

// sqliteSharedMemoryName mirrors infra/db/engine/sqlite.SharedMemoryName — the
// database name a bare ":memory:" resolves to. Duplicated (not imported) to keep
// the migration package free of an engine dependency; the two MUST stay equal.
const sqliteSharedMemoryName = "omnicore_mem"

// sqliteMigrateDSN resolves the raw relational.dsn to the database the migrate
// driver must open — the SAME database the engine opens. It mirrors the
// resolution in infra/db/engine/sqlite/dsn.go (a relative path resolves against
// the same base the engine uses — next to the binary, or the working directory
// for an ephemeral `go run`/`go test` binary; ":memory:" becomes a shared-cache
// named in-memory database) and
// MUST stay in agreement with it: migrations and the engine have to target the
// same database. Only the path is reproduced here (not the full pragma set) —
// the migrate connection needs only foreign_keys/busy_timeout, not the
// correctness pragmas the data path forces.
func sqliteMigrateDSN(raw string) string {
	path := strings.TrimPrefix(raw, "file:")
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	if path == ":memory:" || strings.Contains(raw, "mode=memory") {
		// In-memory: land on the SAME shared-cache named database the engine
		// resolves a bare ":memory:" to (sqlite.SharedMemoryName), so migrations
		// run in the very database the engine serves. A bare ":memory:" here would
		// be a SECOND, private in-memory database the engine never sees. Kept in
		// lockstep with infra/db/engine/sqlite.normalizeMemoryDSN by contract.
		name := path
		if name == ":memory:" || name == "" {
			name = sqliteSharedMemoryName
		}
		return "file:" + name + "?mode=memory&cache=shared&_pragma=foreign_keys(ON)"
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(sqliteResolutionBase(), path)
	}
	if dir := filepath.Dir(path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	return "file:" + path + "?_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)"
}

// sqliteResolutionBase mirrors infra/db/engine/sqlite.resolutionBase — the base
// a RELATIVE sqlite path resolves against: the binary's own directory (the
// self-executable MVP keeps its .db beside the binary), EXCEPT for an ephemeral
// `go run` / `go test` binary (compiled to a temp file the toolchain deletes),
// where the working directory is used so the dev loop persists in the project.
// Duplicated (not imported) for the same reason as sqliteSharedMemoryName; the
// two MUST stay equal — resolving the base differently here is exactly the bug
// that made `go run` migrate one file while the engine served another.
func sqliteResolutionBase() string {
	exe, err := os.Executable()
	if err != nil {
		return sqliteWorkingDirOrDot()
	}
	exeDir := filepath.Dir(exe)
	if sqliteIsEphemeralExeDir(exeDir) {
		return sqliteWorkingDirOrDot()
	}
	return exeDir
}

// sqliteWorkingDirOrDot mirrors infra/db/engine/sqlite.workingDirOrDot.
func sqliteWorkingDirOrDot() string {
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "."
}

// sqliteIsEphemeralExeDir mirrors infra/db/engine/sqlite.isEphemeralExeDir —
// a `go run`/`go test` binary compiles under the OS temp dir (a "go-build*"
// subtree), so a dir inside os.TempDir() is ephemeral; the "go-build" name is
// the symlink-proof fallback when the temp comparison is inconclusive.
func sqliteIsEphemeralExeDir(dir string) bool {
	if tmp, err := filepath.EvalSymlinks(os.TempDir()); err == nil {
		if d, err := filepath.EvalSymlinks(dir); err == nil {
			if rel, err := filepath.Rel(tmp, d); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return true
			}
		}
	}
	return strings.Contains(dir, "go-build")
}
