//go:build mysql

package main

// When omnicore-admin is built with -tags mysql, blank-import the MySQL engine
// so it self-registers in db's engine registry and the subcommands can run
// against a MySQL-backed service (database.dialect: mysql). The Postgres engine
// is registered transitively via the bootstrap import the subcommands already
// pull in; MySQL ships behind its build tag so a default build links neither the
// engine nor the go-sql-driver.
import _ "github.com/ClaudioSchirmer/omnicore/infra/db/engine/mysql"
