//go:build postgres

package bootstrap

import (
	"context"

	"github.com/ClaudioSchirmer/omnicore/infra/audit"
	"github.com/ClaudioSchirmer/omnicore/infra/db/engine/postgres"
	"github.com/ClaudioSchirmer/omnicore/infra/migration"
)

// This file is the Postgres engine's bootstrap binding, compiled under the
// `postgres` build tag (alone or alongside other engine tags — dispatch is by
// the runtime relational.dialect through the engineBoots registry, never by
// tag exclusion). Importing infra/db/engine/postgres runs its init(), which
// registers the "postgres" dialect in the engine registry so core.NewEngine
// resolves it. The two boot steps that still speak pgx directly — audit
// partition maintenance and the migration runner over the live pool — register
// here, so a build without this tag links neither pgx nor this wiring.

func init() {
	registerEngineBoot(dialectPostgres, engineBoot{
		newMigrator: func(deps Deps, cfg *Config) *migration.Manager {
			return migration.New(pgEngine(deps).Pool(), cfg.Migrations.Dir)
		},
		ensureFuturePartitions: func(ctx context.Context, deps Deps, n int) error {
			return audit.EnsureFuturePartitions(ctx, pgEngine(deps).Pool(), n)
		},
	})
}

// pgEngine recovers the concrete *postgres.Postgres from Deps for framework
// wiring that speaks pgx directly. It panics on a non-Postgres engine — safe
// here because the registry dispatches these steps only when the configured
// dialect is "postgres", where the registered engine is always Postgres.
func pgEngine(deps Deps) *postgres.Postgres { return postgres.AsPostgres(deps.DB) }
