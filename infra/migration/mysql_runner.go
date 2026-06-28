//go:build mysql

package migration

import (
	"database/sql"
	"fmt"

	_ "github.com/go-sql-driver/mysql"
	"github.com/golang-migrate/migrate/v4/database"
	mysqldriver "github.com/golang-migrate/migrate/v4/database/mysql"

	enginemysql "github.com/ClaudioSchirmer/omnicore/infra/db/engine/mysql"
)

// openMySQLMigrateDriver opens a dedicated *sql.DB from the DSN and wraps it in
// golang-migrate's MySQL database driver. The returned driver OWNS that *sql.DB
// — migrate.Close() (via closeMigrate) closes it, so the runner never touches
// the engine's live pool. Behind the `mysql` build tag so a Postgres-only build
// links neither this driver nor go-sql-driver.
func openMySQLMigrateDriver(dsn, trackingTable string) (database.Driver, error) {
	// The migration variant forces multiStatements=true (plus parseTime /
	// clientFoundRows) so the flattened framework migration — a multi-statement
	// script — runs regardless of the operator's DSN. Stacked statements are
	// scoped to this migration connection; the live engine pool runs without them
	// (EnsureDSNParams forces multiStatements=false).
	dsn, err := enginemysql.EnsureMigrationDSNParams(dsn)
	if err != nil {
		return nil, fmt.Errorf("migration[mysql]: %w", err)
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("migration[mysql]: open: %w", err)
	}
	drv, err := mysqldriver.WithInstance(db, &mysqldriver.Config{MigrationsTable: trackingTable})
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migration[mysql]: driver: %w", err)
	}
	return drv, nil
}
