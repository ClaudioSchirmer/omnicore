package query

import "github.com/ClaudioSchirmer/omnicore/infra/db/core"

// PhysicalCollection is the physical Mongo collection a logical view currently
// resolves to. It is deliberately NOT a bare string: the ReadModelStore port
// accepts only this type, so a raw view name can never be handed to the store as
// a collection by accident — every physical name must come from a ViewResolver,
// which maps a view to its active slot (or, for a rebuild driver, its shadow
// slot). The unexported field means only this package constructs one; the zero
// value is invalid and never produced by the resolver.
type PhysicalCollection struct{ name string }

// String returns the raw Mongo collection name for the driver-facing seam.
func (p PhysicalCollection) String() string { return p.name }

// ViewResolver maps a logical view name to the PhysicalCollection currently
// serving it. The mapping is served from an in-memory pointer view: the
// relational registry is consulted only at boot and on a slow per-lease refresh,
// never per operation, because the active-slot pointer changes only on a rebuild
// flip (a rare event). One instance is shared process-wide so every read-model
// component observes a single, consistent pointer.
//
// Phase 1 is identity: no slot pointer is set yet (omnicore_mongo_views
// .active_collection is NULL for every row), so Active returns the bare view
// name and Shadow is unused. The engine handle is retained for the
// registry-backed phase (it exposes Querier/Dialect); Phase 1 never touches it.
type ViewResolver struct {
	eng core.RelationalEngine
}

// NewViewResolver builds the process-wide resolver. One instance is shared by
// every read-model component so they observe a single, consistent pointer view.
func NewViewResolver(eng core.RelationalEngine) *ViewResolver {
	return &ViewResolver{eng: eng}
}

// Active returns the collection currently serving reads for the given logical
// name. For a managed view mid/post-flip this is its active slot; for any other
// name — an externally-materialized upstream/embed collection, or (in Phase 1)
// every view — it is the name itself. A nil resolver resolves to identity, so a
// component wired without one behaves exactly as the pre-blue-green code.
func (r *ViewResolver) Active(name string) PhysicalCollection {
	// Phase 1: identity. Phase 2 consults the cached registry pointer here,
	// falling back to the bare name for any name that is not a managed view.
	return PhysicalCollection{name: name}
}

// Shadow returns the inactive slot a rebuild driver builds for the given view.
// Unused until Phase 2 introduces the slot lifecycle; defined now so the write
// seam and the driver share one construction path for physical names.
func (r *ViewResolver) Shadow(name string) PhysicalCollection {
	return PhysicalCollection{name: name}
}
