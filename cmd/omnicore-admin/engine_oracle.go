//go:build oracle

package main

// When omnicore-admin is built with -tags oracle, blank-import the Oracle
// engine so it self-registers in db's engine registry and the subcommands can
// run against an Oracle-backed service (relational.dialect: oracle). It ships
// behind its build tag so a default build links neither the engine nor go-ora.
import _ "github.com/ClaudioSchirmer/omnicore/infra/db/engine/oracle"
