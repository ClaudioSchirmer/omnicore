//go:build postgres

package bootstrap

import (
	_ "github.com/ClaudioSchirmer/omnicore/infra/db/engine/postgres"
	"github.com/ClaudioSchirmer/omnicore/infra/migration"
)

// This file is the Postgres engine's bootstrap binding, compiled under the
// `postgres` build tag (alone or alongside other engine tags — dispatch is by
// the runtime relational.dialect through the engineBoots registry, never by tag
// exclusion). The blank import runs the engine package's init(), which registers
// the "postgres" dialect in the engine registry so core.NewEngine resolves it —
// behind the build tag so a build without it links neither the engine nor pgx.

func init() {
	registerEngineBoot(dialectPostgres, engineBoot{
		// The Postgres runner opens its own *sql.DB from the DSN, never the
		// engine's live pool — the same discipline as the other engines, so this
		// binding needs no concrete-engine recovery. No partition maintenance:
		// audit_events is a plain table on every backend (retention/partitioning
		// is a devops concern).
		newMigrator: func(_ Deps, cfg *Config) *migration.Manager {
			return migration.NewPostgres(cfg.Relational.DSN, cfg.Migrations.Dir)
		},
	})
}
