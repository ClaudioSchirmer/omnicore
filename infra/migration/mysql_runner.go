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

// NewMySQL builds a MySQL Manager. The runner opens its own *sql.DB from the DSN
// (closed when each migrate instance closes) rather than sharing the engine's
// live pool. Behind the `mysql` build tag so a Postgres-only build links neither
// the MySQL migrate driver nor go-sql-driver. dir is the service migration
// directory.
func NewMySQL(dsn, dir string) *Manager {
	return &Manager{
		dialect: "mysql",
		dir:     dir,
		openDriver: func(trackingTable string) (database.Driver, string, error) {
			// The migration variant forces multiStatements=true (plus parseTime /
			// clientFoundRows) so the flattened framework migration — a
			// multi-statement script — runs regardless of the operator's DSN.
			// Stacked statements are scoped to this migration connection; the live
			// engine pool runs without them (EnsureDSNParams forces
			// multiStatements=false).
			mdsn, err := enginemysql.EnsureMigrationDSNParams(dsn)
			if err != nil {
				return nil, "", fmt.Errorf("migration[mysql]: %w", err)
			}
			db, err := sql.Open("mysql", mdsn)
			if err != nil {
				return nil, "", fmt.Errorf("migration[mysql]: open: %w", err)
			}
			drv, err := mysqldriver.WithInstance(db, &mysqldriver.Config{MigrationsTable: trackingTable})
			if err != nil {
				_ = db.Close()
				return nil, "", fmt.Errorf("migration[mysql]: driver: %w", err)
			}
			return drv, "mysql", nil
		},
	}
}
