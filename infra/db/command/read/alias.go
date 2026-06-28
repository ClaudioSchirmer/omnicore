package read

// Local aliases for the foundation + write symbols the relational read layer
// consumes unqualified (as they were when this code lived in package db). The
// read layer depends on core (schema + seam) and on write (BaseAggregateRepository
// embeds BaseRepository); it is never depended on by either — read → {write, core}
// is the safe edge direction. Mirrors the db/core_bridge.go pattern.

import (
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/db/command/write"
)

type (
	TableSchema      = core.TableSchema
	Querier          = core.Querier
	Dialect          = core.Dialect
	Row              = core.Row
	Rows             = core.Rows
	RelationalEngine = core.RelationalEngine

	BaseRepository[T any] = write.BaseRepository[T]
)

func TypeName[T any]() string { return core.TypeName[T]() }
