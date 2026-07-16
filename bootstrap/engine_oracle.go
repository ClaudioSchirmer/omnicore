//go:build oracle

package bootstrap

import (
	_ "github.com/ClaudioSchirmer/omnicore/infra/db/engine/oracle"
	"github.com/ClaudioSchirmer/omnicore/infra/migration"
)

// This file is the Oracle engine's bootstrap binding, compiled under the
// `oracle` build tag (alone or alongside other engine tags — dispatch is by
// the runtime relational.dialect through the engineBoots registry, never by
// tag exclusion). The blank import runs the engine package's init(), which
// registers the "oracle" dialect in the engine registry so core.NewEngine
// resolves it — behind the build tag so a build without it links neither the
// engine nor go-ora.

func init() {
	registerEngineBoot(dialectOracle, engineBoot{
		// The Oracle runner opens its own *sql.DB from the DSN, never the
		// engine's live pool. No ensureFuturePartitions: the Oracle
		// audit_events table is not range-partitioned.
		newMigrator: func(_ Deps, cfg *Config) *migration.Manager {
			return migration.NewOracle(cfg.Relational.DSN, cfg.Migrations.Dir)
		},
	})
}
