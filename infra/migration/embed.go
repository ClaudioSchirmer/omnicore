package migration

import "embed"

//go:embed embedded/*.sql
var frameworkMigrations embed.FS
