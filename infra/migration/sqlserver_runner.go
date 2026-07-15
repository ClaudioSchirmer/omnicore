//go:build sqlserver

package migration

import (
	"database/sql"
	"fmt"

	"github.com/golang-migrate/migrate/v4/database"
	sqlserverdriver "github.com/golang-migrate/migrate/v4/database/sqlserver"
	_ "github.com/microsoft/go-mssqldb"
)

// NewSQLServer builds a SQL Server Manager. The runner opens its own *sql.DB
// from the DSN (closed when each migrate instance closes) rather than sharing
// the engine's live pool — the MySQL runner's discipline. Behind the
// `sqlserver` build tag so a build without it links neither the sqlserver
// migrate driver nor go-mssqldb. Unlike MySQL, no DSN massaging is needed:
// go-mssqldb executes multi-statement batches natively and scans time.Time
// without a flag. dir is the service migration directory.
func NewSQLServer(dsn, dir string) *Manager {
	return &Manager{
		dialect: "sqlserver",
		dir:     dir,
		openDriver: func(trackingTable string) (database.Driver, string, error) {
			db, err := sql.Open("sqlserver", dsn)
			if err != nil {
				return nil, "", fmt.Errorf("migration[sqlserver]: open: %w", err)
			}
			drv, err := sqlserverdriver.WithInstance(db, &sqlserverdriver.Config{MigrationsTable: trackingTable})
			if err != nil {
				_ = db.Close()
				return nil, "", fmt.Errorf("migration[sqlserver]: driver: %w", err)
			}
			return drv, "sqlserver", nil
		},
	}
}
