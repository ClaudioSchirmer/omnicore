//go:build sqlserver

package migration

import "embed"

//go:embed embedded/sqlserver/*.sql
var sqlserverFS embed.FS

func init() { registerFrameworkFS("sqlserver", sqlserverFS) }
