//go:build sqlserver

package main

// When omnicore-admin is built with -tags sqlserver, blank-import the SQL
// Server engine so it self-registers in db's engine registry and the
// subcommands can run against a SQL Server-backed service
// (relational.dialect: sqlserver). It ships behind its build tag so a default
// build links neither the engine nor go-mssqldb.
import _ "github.com/ClaudioSchirmer/omnicore/infra/db/engine/sqlserver"
