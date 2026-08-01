//go:build sqlite

package bootstrap

import (
	_ "github.com/ClaudioSchirmer/omnicore/infra/db/engine/sqlite"
	"github.com/ClaudioSchirmer/omnicore/infra/migration"
)

// This file is the SQLite engine's bootstrap binding, compiled under the
// `sqlite` build tag (alone or alongside other engine tags — dispatch is by the
// runtime relational.dialect through the engineBoots registry, never by tag
// exclusion). The blank import runs the engine package's init(), which registers
// the "sqlite" dialect in the engine registry so core.NewEngine resolves it —
// behind the build tag so a build without it links neither the engine nor
// modernc.org/sqlite.
func init() {
	registerEngineBoot(dialectSQLite, engineBoot{
		// The SQLite runner opens its own *sql.DB from the DSN (resolved to the
		// same file the engine opens), never the engine's live pool.
		newMigrator: func(_ Deps, cfg *Config) *migration.Manager {
			return migration.NewSQLite(cfg.Relational.DSN, cfg.Migrations.Dir)
		},
	})
}
