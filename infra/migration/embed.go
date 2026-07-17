package migration

import "embed"

// Framework migrations live under embedded/<dialect>/ — one flattened migration
// per dialect (postgres, mysql, sqlserver, oracle) describing the same logical
// control-plane schema.
//
//go:embed embedded/postgres/*.sql embedded/mysql/*.sql embedded/sqlserver/*.sql embedded/oracle/*.sql
var frameworkMigrations embed.FS
