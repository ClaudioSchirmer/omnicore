//go:build sqlserver

package bootstrap

import (
	_ "github.com/ClaudioSchirmer/omnicore/infra/db/engine/sqlserver"
	"github.com/ClaudioSchirmer/omnicore/infra/migration"
)

// This file is the SQL Server engine's bootstrap binding, compiled under the
// `sqlserver` build tag (alone or alongside other engine tags — dispatch is by
// the runtime relational.dialect through the engineBoots registry, never by
// tag exclusion). The blank import runs the engine package's init(), which
// registers the "sqlserver" dialect in the engine registry so core.NewEngine
// resolves it — behind the build tag so a build without it links neither the
// engine nor go-mssqldb.

func init() {
	registerEngineBoot(dialectSQLServer, engineBoot{
		// The SQL Server runner opens its own *sql.DB from the DSN, never the
		// engine's live pool. No ensureFuturePartitions: the SQL Server
		// audit_events table is not range-partitioned.
		newMigrator: func(_ Deps, cfg *Config) *migration.Manager {
			return migration.NewSQLServer(cfg.Relational.DSN, cfg.Migrations.Dir)
		},
	})
}
