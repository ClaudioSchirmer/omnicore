//go:build postgres && !mysql

package bootstrap

import (
	"context"

	"github.com/ClaudioSchirmer/omnicore/infra/audit"
	"github.com/ClaudioSchirmer/omnicore/infra/db/engine/postgres"
	"github.com/ClaudioSchirmer/omnicore/infra/migration"
)

// This file is the Postgres engine's bootstrap binding, compiled only under the
// `postgres` build tag. Importing infra/db/engine/postgres runs its init(),
// which registers the "postgres" dialect in the engine registry so
// core.NewEngine resolves it. It also carries the two boot steps that still
// speak pgx directly (audit partitions, the migration runner) — so a MySQL-only
// build links neither pgx nor this wiring.

// pgEngine recovers the concrete *postgres.Postgres from Deps for framework
// wiring that speaks pgx directly. It panics on a non-Postgres engine — safe
// here because this file only compiles under the postgres build tag, where the
// registered engine is always Postgres.
func pgEngine(deps Deps) *postgres.Postgres { return postgres.AsPostgres(deps.DB) }

// ensureFuturePartitions provisions the next n monthly partitions of
// audit_events over the live pgx pool. PG-only — the mysql/none builds supply a
// no-op of this name (the MySQL audit table is not range-partitioned).
func ensureFuturePartitions(ctx context.Context, deps Deps, n int) error {
	return audit.EnsureFuturePartitions(ctx, pgEngine(deps).Pool(), n)
}

// newMigrator builds the Postgres migration runner over the live pgx pool.
func newMigrator(deps Deps, cfg *Config) *migration.Manager {
	return migration.New(pgEngine(deps).Pool(), cfg.Migrations.Dir)
}
