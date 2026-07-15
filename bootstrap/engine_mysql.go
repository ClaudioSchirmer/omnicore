//go:build mysql

package bootstrap

import (
	_ "github.com/ClaudioSchirmer/omnicore/infra/db/engine/mysql"
	"github.com/ClaudioSchirmer/omnicore/infra/migration"
)

// This file is the MySQL engine's bootstrap binding, compiled under the `mysql`
// build tag (alone or alongside other engine tags — dispatch is by the runtime
// relational.dialect through the engineBoots registry, never by tag exclusion).
// The blank import runs the engine package's init(), which registers the
// "mysql" dialect in the engine registry so core.NewEngine resolves it — behind
// the build tag so a build without it links neither the engine nor
// go-sql-driver.

func init() {
	registerEngineBoot(dialectMySQL, engineBoot{
		// The MySQL runner opens its own *sql.DB from the DSN, never the
		// engine's live pool. No ensureFuturePartitions: the MySQL audit_events
		// table is not range-partitioned.
		newMigrator: func(_ Deps, cfg *Config) *migration.Manager {
			return migration.NewMySQL(cfg.Relational.DSN, cfg.Migrations.Dir)
		},
	})
}
