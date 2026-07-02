package read

import (
	"fmt"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/domain"
)

// SharedBaseRoleRepository is the repository for an entity backed by a SharedBase
// (a Party-Role: the entity is a ROLE — aluno, professor — referencing a shared
// identity — pessoa). It embeds BaseAggregateRepository (the role may have its own
// children + always carries the base-children) and ADDS the
// persistence.SharedBaseInsertLoader[T] capability — the load-by-natural-key the
// SharedBase upsert insert needs.
//
// The capability is type-conditional ON PURPOSE: only this repository implements
// it, so its presence/absence drives the two-way marriage —
// SharedBaseInsertCommandHandler requires it, the plain InsertCommandHandler
// refuses it. A SharedBase-backed entity MUST use this repository; a non-SharedBase
// entity uses BaseAggregateRepository (which has no such capability). WithSchema
// asserts the schema actually declares a SharedBase.
type SharedBaseRoleRepository[T domain.Entity] struct {
	BaseAggregateRepository[T]
}

// NewSharedBaseRoleRepository composes the SharedBase role repository over the
// engine + entity factory (shared, like the aggregate repository).
func NewSharedBaseRoleRepository[T domain.Entity](eng RelationalEngine, newEntity func() T) SharedBaseRoleRepository[T] {
	return SharedBaseRoleRepository[T]{BaseAggregateRepository: NewBaseAggregateRepository[T](eng, newEntity)}
}

// WithSchema threads the schema (write + criteria + scan + loader) and asserts it
// declares a SharedBase — the boot guard of the marriage's read side.
func (r *SharedBaseRoleRepository[T]) WithSchema(schema *TableSchema) *SharedBaseRoleRepository[T] {
	if _, _, ok := schema.SharedBaseRef(); !ok {
		panic(fmt.Sprintf(
			"infra: SharedBaseRoleRepository requires a schema declaring .SharedBase(...); %q has none — "+
				"use BaseAggregateRepository for an entity without a shared base.", schema.Table()))
	}
	r.BaseAggregateRepository.WithSchema(schema)
	return r
}

// LoadForSharedBaseInsert satisfies persistence.SharedBaseInsertLoader[T]: load the
// existing shared identity (base fields + base-children as Constructor) by the
// natural key read from fresh. *configuration.AppContext satisfies context.Context,
// so it threads straight into the loader.
func (r *SharedBaseRoleRepository[T]) LoadForSharedBaseInsert(ctx *configuration.AppContext, fresh T) (T, bool, error) {
	return r.Loader.LoadSharedBaseIdentity(ctx, fresh)
}
