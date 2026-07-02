package persistence

import "github.com/ClaudioSchirmer/omnicore/application/configuration"

// SharedBaseInsertLoader is the optional repository capability behind the
// SharedBase upsert insert: load the existing shared identity — its base fields
// plus its native children (base-children) as Constructor items — by the entity's
// natural key. A repository implements it ONLY when its schema declares a
// SharedBase, so its presence/absence is the read-side half of the two-way
// marriage: handlers.SharedBaseInsertCommandHandler requires it, and the plain
// InsertCommandHandler refuses a repo that has it. Implemented by
// db.BaseAggregateRepository when the schema declares .SharedBase(...).
type SharedBaseInsertLoader[T any] interface {
	// LoadForSharedBaseInsert reads the natural key from fresh, looks up the shared
	// identity, and returns it hydrated (base fields + base-children as Constructor)
	// with existed=true when it already exists; otherwise returns fresh unchanged
	// with existed=false (cold insert, no shared identity yet). The returned entity
	// is what the command's ApplyTo mutates and the persister inserts.
	LoadForSharedBaseInsert(ctx *configuration.AppContext, fresh T) (entity T, existed bool, err error)
}
