//go:build oracle

package migration

import "embed"

//go:embed embedded/oracle/*.sql
var oracleFS embed.FS

func init() { registerFrameworkFS("oracle", oracleFS) }
