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

// sqliteMigrateDSN resolves the raw relational.dsn to the file the migrate
// driver must open — the SAME file the engine opens. It mirrors the path
// resolution in infra/db/engine/sqlite/dsn.go (a relative path resolves next to
// the binary; ":memory:" is left untouched) and MUST stay in agreement with it:
// migrations and the engine have to target the same database. Only the path is
// reproduced here (not the full pragma set) — the migrate connection needs only
// foreign_keys/busy_timeout, not the correctness pragmas the data path forces.
func sqliteMigrateDSN(raw string) string {
	path := strings.TrimPrefix(raw, "file:")
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	if path == ":memory:" || strings.Contains(raw, "mode=memory") {
		return raw
	}
	if !filepath.IsAbs(path) {
		if exe, err := os.Executable(); err == nil {
			path = filepath.Join(filepath.Dir(exe), path)
		}
	}
	if dir := filepath.Dir(path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	return "file:" + path + "?_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)"
}
