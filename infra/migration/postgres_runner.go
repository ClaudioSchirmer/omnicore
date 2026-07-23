//go:build postgres

package migration

import (
	"database/sql"
	"fmt"

	"github.com/golang-migrate/migrate/v4/database"
	pgxdriver "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// NewPostgres builds a Postgres Manager. The runner opens its own *sql.DB from
// the DSN (closed when each migrate instance closes) rather than sharing the
// engine's live pool — the same discipline as NewMySQL / NewOracle / NewSQLServer,
// which is what lets the bootstrap wiring stay backend-neutral (no engine-recovery
// escape hatch). Behind the `postgres` build tag so a MySQL-only build links
// neither pgx nor the pgx5 migrate driver. dir is the service migration directory
// (relative or absolute), conventionally "./migrations".
func NewPostgres(dsn, dir string) *Manager {
	return &Manager{
		dialect: "postgres",
		dir:     dir,
		openDriver: func(trackingTable string) (database.Driver, string, error) {
			db, err := sql.Open("pgx", dsn)
			if err != nil {
				return nil, "", fmt.Errorf("migration[postgres]: open: %w", err)
			}
			drv, err := pgxdriver.WithInstance(db, &pgxdriver.Config{MigrationsTable: trackingTable})
			if err != nil {
				_ = db.Close()
				return nil, "", fmt.Errorf("migration[postgres]: driver: %w", err)
			}
			return drv, "pgx5", nil
		},
	}
}
