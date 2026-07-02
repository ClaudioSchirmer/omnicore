//go:build postgres && mysql

package bootstrap

import (
	"context"

	"github.com/ClaudioSchirmer/omnicore/infra/audit"
	"github.com/ClaudioSchirmer/omnicore/infra/db/engine/postgres"
	"github.com/ClaudioSchirmer/omnicore/infra/migration"
)

// This file is the both-engines bootstrap binding, compiled only when BOTH the
// `postgres` and `mysql` build tags are set. The binary links both relational
// engines (each self-registers its dialect via init(), so core.NewEngine
// resolves whichever `relational.dialect` the YAML selects at boot) and the two
// dialect-bound boot steps — audit partition maintenance and the migration
// runner — dispatch on the runtime engine/dialect instead of the build tag.
//
// The single-engine builds keep their tag-gated twins (engine_postgres.go is
// `postgres && !mysql`, engine_mysql.go is `mysql && !postgres`) so a binary
// built with exactly one tag links exactly one engine + driver. This file is the
// only place that speaks both.

// ensureFuturePartitions provisions the next n monthly partitions of
// audit_events — a Postgres-only concern (the MySQL audit_events table is not
// range-partitioned). It runs only when the live engine is the PG adapter; on a
// MySQL-dialect boot the type assertion fails and it no-ops.
func ensureFuturePartitions(ctx context.Context, deps Deps, n int) error {
	pg, ok := deps.DB.(*postgres.Postgres)
	if !ok {
		return nil
	}
	return audit.EnsureFuturePartitions(ctx, pg.Pool(), n)
}

// newMigrator builds the migration runner for the configured dialect: the MySQL
// runner opens its own *sql.DB from the DSN; the Postgres runner borrows the live
// pgx pool. Dispatch keys on cfg.Relational.Dialect — already validated at load.
func newMigrator(deps Deps, cfg *Config) *migration.Manager {
	if cfg.Relational.Dialect == "mysql" {
		return migration.NewMySQL(cfg.Relational.DSN, cfg.Migrations.Dir)
	}
	return migration.New(postgres.AsPostgres(deps.DB).Pool(), cfg.Migrations.Dir)
}
