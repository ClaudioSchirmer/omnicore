package migration

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/golang-migrate/migrate/v4/source"
	"github.com/golang-migrate/migrate/v4/source/file"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

const (
	frameworkSourceName  = "framework-embedded"
	frameworkTrackingTbl = "omnicore_framework_migrations"
	serviceTrackingTbl   = "omnicore_migrations"
	embeddedSubdir       = "embedded"
)

// frameworkSource exposes the Postgres framework migrations — the default-dialect
// entry point kept for callers that do not (yet) thread a dialect; the Postgres
// runner is the one wired today.
func frameworkSource() (source.Driver, error) {
	return frameworkSourceFor("postgres")
}

// frameworkSourceFor exposes the embedded framework migrations for one dialect
// via embed.FS. Subpath "embedded/<dialect>" — where that dialect's flattened
// 0001_framework.{up,down}.sql lives.
func frameworkSourceFor(dialect string) (source.Driver, error) {
	sub, err := fs.Sub(frameworkMigrations, embeddedSubdir+"/"+dialect)
	if err != nil {
		return nil, fmt.Errorf("migration: framework subfs (%s): %w", dialect, err)
	}
	drv, err := iofs.New(sub, ".")
	if err != nil {
		return nil, fmt.Errorf("migration: framework iofs (%s): %w", dialect, err)
	}
	return drv, nil
}

// serviceSource reads the migration files from the service directory.
// Accepts a relative or absolute path; converts to absolute before passing
// to source/file (which requires file:// + absolute path).
func serviceSource(dir string) (source.Driver, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("migration: service dir abs: %w", err)
	}
	drv, err := (&file.File{}).Open("file://" + abs)
	if err != nil {
		return nil, fmt.Errorf("migration: service file source: %w", err)
	}
	return drv, nil
}

// frameworkMigrationNames lists the embedded framework migration base names for
// the postgres dialect (every dialect carries the same logical sequence, so one
// is representative). Used by tests to derive the expected framework version
// instead of hardcoding a number that silently goes stale when the control plane
// grows.
func frameworkMigrationNames() []string {
	entries, err := frameworkMigrations.ReadDir("embedded/postgres")
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".up.sql") {
			out = append(out, e.Name())
		}
	}
	return out
}
