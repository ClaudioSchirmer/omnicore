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

// frameworkSource exposes the embedded framework migrations of whatever dialect
// the build linked — the dialect-agnostic entry point for callers that do not
// thread a specific dialect. Every dialect carries the same logical sequence, so
// any linked one is representative; it reads the first entry in the tag-gated
// registry rather than assuming "postgres" (which a non-postgres build never
// links). Empty registry (no engine tag) is a clear error.
func frameworkSource() (source.Driver, error) {
	for dialect := range frameworkFS {
		return frameworkSourceFor(dialect)
	}
	return nil, fmt.Errorf("migration: no embedded framework migrations linked (build with an engine build tag)")
}

// frameworkSourceFor exposes the embedded framework migrations for one dialect
// via embed.FS. Subpath "embedded/<dialect>" — where that dialect's flattened
// 0001_framework.{up,down}.sql lives.
func frameworkSourceFor(dialect string) (source.Driver, error) {
	fsys, ok := frameworkFS[dialect]
	if !ok {
		return nil, fmt.Errorf("migration: no embedded framework migrations for dialect %q (build with the engine's build tag?)", dialect)
	}
	sub, err := fs.Sub(fsys, embeddedSubdir+"/"+dialect)
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

// frameworkMigrationNames lists the embedded framework migration base names.
// Every dialect carries the same logical sequence, so ANY linked dialect is
// representative — it reads the first entry in the tag-gated registry rather than
// a hardcoded dialect, so it works in a build that linked only (say) sqlite.
// Used by tests to derive the expected framework version instead of hardcoding a
// number that silently goes stale when the control plane grows.
func frameworkMigrationNames() []string {
	for dialect, fsys := range frameworkFS {
		entries, err := fsys.ReadDir(embeddedSubdir + "/" + dialect)
		if err != nil {
			continue
		}
		var out []string
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".up.sql") {
				out = append(out, e.Name())
			}
		}
		return out
	}
	return nil
}
