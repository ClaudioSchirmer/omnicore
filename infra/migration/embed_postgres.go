//go:build postgres

package migration

import "embed"

//go:embed embedded/postgres/*.sql
var postgresFS embed.FS

func init() { registerFrameworkFS("postgres", postgresFS) }
