package db

// Bridge re-exports for the infra/db → infra/db/core extraction. The
// implementation moved to the core foundation package (ports + schema + value
// types); these aliases keep the historical db.* surface stable so internal
// callers and the example consumer compile unchanged while the split proceeds
// phase by phase. The whole file is removed in the final phase, when consumers
// import core directly.

import (
	"github.com/ClaudioSchirmer/omnicore/application/persistence"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/db/read/relational"
	"github.com/ClaudioSchirmer/omnicore/infra/db/write"
)

// --- exception.go ---

type InfrastructureError = core.InfrastructureError

var (
	NewInfrastructureError     = core.NewInfrastructureError
	NewInfrastructureErrorWith = core.NewInfrastructureErrorWith
	SingleNotificationError    = core.SingleNotificationError
	FieldErrorWithCause        = core.FieldErrorWithCause
	LimitExceededError         = core.LimitExceededError
	InvalidCursorError         = core.InvalidCursorError
)

// --- fields.go ---

var SortedKeys = core.SortedKeys

// --- identifier.go ---

var SafeIdentifier = core.SafeIdentifier

// --- infer.go (generic — needs a forwarding wrapper, not a var alias) ---

func TypeName[T any]() string { return core.TypeName[T]() }

// --- read.go (the read/dialect seam) ---

type (
	Rows          = core.Rows
	Row           = core.Row
	Querier       = core.Querier
	Dialect       = core.Dialect
	UpsertSet     = core.UpsertSet
	UpsertSetMode = core.UpsertSetMode
)

const (
	UpsertSetNew  = core.UpsertSetNew
	UpsertSetExpr = core.UpsertSetExpr
)

// --- tx.go ---

type (
	Tx            = core.Tx
	WriteTx       = core.WriteTx
	WriteBeginner = core.WriteBeginner
)

var UnwrapTx = core.UnwrapTx

// --- rebuild_lock.go ---

type RebuildLock = core.RebuildLock

// --- table_schema.go (the schema spine) ---

type TableSchema = core.TableSchema

var NewExternalSchema = core.NewExternalSchema

// NewTableSchema is generic, so it forwards through a wrapper (a var alias can't
// carry the type parameter).
func NewTableSchema[T any](table string) *core.TableSchema { return core.NewTableSchema[T](table) }

// --- hooks.go ---

type WriteHook = core.WriteHook

// AdaptWriteOptions is generic — forwards through a wrapper.
func AdaptWriteOptions[T any](opts []persistence.WriteOption[T]) core.WriteHook {
	return core.AdaptWriteOptions[T](opts)
}

// --- engine.go (the relational engine port + dialect registry) ---

type (
	RelationalEngine = core.RelationalEngine
	EngineFactory    = core.EngineFactory
)

var (
	RegisterEngine = core.RegisterEngine
	NewEngine      = core.NewEngine
)

// --- write layer (executor/persister/repos/audit-builder/outbox) ---

type (
	BaseRepository[T any] = write.BaseRepository[T]
	ConstraintBinding     = write.ConstraintBinding
	BaseEngine            = write.BaseEngine
	HookContext           = write.HookContext
	AuditBundle           = write.AuditBundle
)

// --- read layer (relational: aggregate loader + aggregate repo + audit reader) ---

type (
	AggregateLoader[T domain.Entity]          = relational.AggregateLoader[T]
	BaseAggregateRepository[T domain.Entity]  = relational.BaseAggregateRepository[T]
	RootScanner[T domain.Entity]              = relational.RootScanner[T]
	ChildScanner                              = relational.ChildScanner
)

var NewAuditReader = relational.NewAuditReader

// Generic constructors forward through wrappers.
func NewAggregateLoader[T domain.Entity](eng RelationalEngine, newEntity func() T) *relational.AggregateLoader[T] {
	return relational.NewAggregateLoader[T](eng, newEntity)
}
func NewBaseAggregateRepository[T domain.Entity](eng RelationalEngine, newEntity func() T) relational.BaseAggregateRepository[T] {
	return relational.NewBaseAggregateRepository[T](eng, newEntity)
}
