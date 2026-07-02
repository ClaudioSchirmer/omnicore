//go:build postgres

package migration

import (
	"github.com/golang-migrate/migrate/v4/database"
	pgxdriver "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
)

// New builds a Postgres Manager that runs migrations over the live pgx pool. dir
// is the service migration directory (relative or absolute), conventionally
// "./migrations". Behind the `postgres` build tag so a MySQL-only build links
// neither pgx nor the pgx5 migrate driver.
func New(pool *pgxpool.Pool, dir string) *Manager {
	return &Manager{
		dialect: "postgres",
		dir:     dir,
		openDriver: func(trackingTable string) (database.Driver, string, error) {
			// stdlib.OpenDBFromPool does NOT close the pool on Close (documented
			// in pgx/v5/stdlib), so the runner borrows the live engine pool
			// without owning its lifetime — closeMigrate's Close is a no-op on it.
			db := stdlib.OpenDBFromPool(pool)
			drv, err := pgxdriver.WithInstance(db, &pgxdriver.Config{MigrationsTable: trackingTable})
			if err != nil {
				_ = db.Close()
				return nil, "", err
			}
			return drv, "pgx5", nil
		},
	}
}
