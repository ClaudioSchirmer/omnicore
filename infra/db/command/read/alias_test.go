package read

// Test-only aliases for core symbols the relational read white-box test fixtures
// (fake engine/querier/dialect, schema constructors) reference unqualified.

import "github.com/ClaudioSchirmer/omnicore/infra/db/core"

type (
	WriteHook           = core.WriteHook
	RebuildLock         = core.RebuildLock
	UpsertSet           = core.UpsertSet
	InfrastructureError = core.InfrastructureError
)

var (
	SafeIdentifier    = core.SafeIdentifier
	NewExternalSchema = core.NewExternalSchema
)

func NewTableSchema[T any](table string) *core.TableSchema { return core.NewTableSchema[T](table) }

func NewSiblingSchema[T any](table string) *core.TableSchema { return core.NewSiblingSchema[T](table) }
