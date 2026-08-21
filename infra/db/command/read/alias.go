package read

// Local aliases for the foundation + write symbols the relational read layer
// consumes unqualified (as they were when this code lived in package db). The
// read layer depends on core (schema + seam) and on write (BaseAggregateRepository
// embeds write.BaseRepository); it is never depended on by either — read → {write, core}
// is the safe edge direction. Mirrors the db/core_bridge.go pattern.

import (
	"github.com/ClaudioSchirmer/omnicore/infra/db/command/write"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

type (
	TableSchema      = core.TableSchema
	Querier          = core.Querier
	Dialect          = core.Dialect
	Row              = core.Row
	Rows             = core.Rows
	RelationalEngine = core.RelationalEngine

	// Kept for source compatibility with services that spell the write base
	// unqualified through this package. The framework itself no longer embeds
	// through it: see the note on BaseAggregateRepository for why promotion
	// across a GENERIC type alias must be avoided in embeds.
	BaseRepository[T any] = write.BaseRepository[T]
)

func TypeName[T any]() string { return core.TypeName[T]() }
