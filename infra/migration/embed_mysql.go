//go:build mysql

package migration

import "embed"

//go:embed embedded/mysql/*.sql
var mysqlFS embed.FS

func init() { registerFrameworkFS("mysql", mysqlFS) }
