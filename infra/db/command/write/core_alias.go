package write

// Local aliases for the core foundation symbols the write layer consumes, so
// the moved write files reference them unqualified (as they did when this code
// lived in package db). Mirrors the db/core_bridge.go pattern; an internal
// convenience, not new surface.

import (
	"github.com/ClaudioSchirmer/omnicore/application/persistence"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

type (
	TableSchema      = core.TableSchema
	Dialect          = core.Dialect
	Tx               = core.Tx
	WriteTx          = core.WriteTx
	WriteBeginner    = core.WriteBeginner
	WriteHook        = core.WriteHook
	RelationalEngine = core.RelationalEngine
)

var (
	FieldErrorWithCause = core.FieldErrorWithCause
	SortedKeys          = core.SortedKeys
)

func AdaptWriteOptions[T any](opts []persistence.WriteOption[T]) core.WriteHook {
	return core.AdaptWriteOptions[T](opts)
}

func TypeName[T any]() string { return core.TypeName[T]() }
