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
	UpsertSet        = core.UpsertSet
	UpsertSetMode    = core.UpsertSetMode
	OrphanPolicy     = core.OrphanPolicy
	RoleRef          = core.RoleRef
	Redactor         = core.Redactor
	ClockMode        = core.ClockMode
)

const (
	UpsertSetNew           = core.UpsertSetNew
	DeleteWhenUnreferenced = core.DeleteWhenUnreferenced
	KeepOrphan             = core.KeepOrphan
	ClockApp               = core.ClockApp
	ClockDB                = core.ClockDB
)

var (
	FieldErrorWithCause     = core.FieldErrorWithCause
	SingleNotificationError = core.SingleNotificationError
	SortedKeys              = core.SortedKeys
)

func AdaptWriteOptions[T any](opts []persistence.WriteOption[T]) core.WriteHook {
	return core.AdaptWriteOptions[T](opts)
}

func TypeName[T any]() string { return core.TypeName[T]() }
