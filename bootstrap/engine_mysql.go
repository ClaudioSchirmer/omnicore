//go:build mysql && !postgres

package bootstrap

import (
	"context"

	_ "github.com/ClaudioSchirmer/omnicore/infra/db/engine/mysql"
	"github.com/ClaudioSchirmer/omnicore/infra/migration"
)

// This file is the MySQL engine's bootstrap binding, compiled only under the
// `mysql` build tag. The blank import runs the engine package's init(), which
// registers the "mysql" dialect in the engine registry so core.NewEngine
// resolves it — behind the build tag so a default build links neither the engine
// nor go-sql-driver.

// ensureFuturePartitions is a no-op on MySQL: the MySQL audit_events table is not
// range-partitioned (partition maintenance is a Postgres-only concern).
func ensureFuturePartitions(_ context.Context, _ Deps, _ int) error { return nil }

// newMigrator builds the MySQL migration runner — its own *sql.DB from the DSN,
// never the engine's live pool.
func newMigrator(_ Deps, cfg *Config) *migration.Manager {
	return migration.NewMySQL(cfg.Relational.DSN, cfg.Migrations.Dir)
}
