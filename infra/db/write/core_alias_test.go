package write

// Test-only aliases for core symbols the write-layer white-box test fixtures
// (fake engine/querier/dialect) reference unqualified.

import "github.com/ClaudioSchirmer/omnicore/infra/db/core"

type (
	Row                 = core.Row
	Rows                = core.Rows
	Querier             = core.Querier
	UpsertSet           = core.UpsertSet
	RebuildLock         = core.RebuildLock
	InfrastructureError = core.InfrastructureError
)

var (
	SafeIdentifier    = core.SafeIdentifier
	NewExternalSchema = core.NewExternalSchema
)

func NewTableSchema[T any](table string) *core.TableSchema { return core.NewTableSchema[T](table) }
