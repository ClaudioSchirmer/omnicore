//go:build sqlite

package migration

import "embed"

//go:embed embedded/sqlite/*.sql
var sqliteFS embed.FS

func init() { registerFrameworkFS("sqlite", sqliteFS) }
