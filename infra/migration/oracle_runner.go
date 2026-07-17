//go:build oracle

package migration

import (
	"database/sql"
	"fmt"

	"github.com/golang-migrate/migrate/v4/database"
	_ "github.com/sijms/go-ora/v2"
)

// NewOracle builds an Oracle Manager. The runner opens its own *sql.DB from
// the DSN (closed when each migrate instance closes) rather than sharing the
// engine's live pool — the other runners' discipline. Behind the `oracle`
// build tag so a build without it links neither the driver below nor go-ora.
// Unlike the other dialects the golang-migrate database driver is the
// framework's own (oracle_driver.go): golang-migrate ships no in-tree Oracle
// driver. No DSN massaging is needed — migrations never read LOBs, and the
// runner feeds statements one by one (see splitOracleStatements). dir is the
// service migration directory.
func NewOracle(dsn, dir string) *Manager {
	return &Manager{
		dialect: "oracle",
		dir:     dir,
		openDriver: func(trackingTable string) (database.Driver, string, error) {
			db, err := sql.Open("oracle", dsn)
			if err != nil {
				return nil, "", fmt.Errorf("migration[oracle]: open: %w", err)
			}
			drv, err := newOracleDriver(db, trackingTable)
			if err != nil {
				_ = db.Close()
				return nil, "", fmt.Errorf("migration[oracle]: driver: %w", err)
			}
			return drv, "oracle", nil
		},
	}
}
