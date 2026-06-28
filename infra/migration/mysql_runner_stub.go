//go:build !mysql

package migration

import (
	"errors"

	"github.com/golang-migrate/migrate/v4/database"
)

// openMySQLMigrateDriver is the no-MySQL build stub: a Manager configured for the
// mysql dialect fails loudly here instead of silently linking the MySQL driver
// into a Postgres-only build. Build with -tags mysql to enable it.
func openMySQLMigrateDriver(_, _ string) (database.Driver, error) {
	return nil, errors.New("migration: MySQL migrations require building with -tags mysql")
}
