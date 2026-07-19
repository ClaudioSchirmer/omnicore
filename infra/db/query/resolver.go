package query

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// defaultResolverLease bounds how stale a pod's pointer cache may be before an
// apply forces a re-read. It is the bounded-staleness lease of the G3 fence: a
// rebuild driver waits one lease (plus margin) after enabling dual-apply so that
// every consuming pod has re-read the flag before the backfill starts.
const defaultResolverLease = 15 * time.Second

// slotSuffix0 / slotSuffix1 are the two physical slots a view alternates between
// across rebuilds. A view has a logical name and at most these two collections;
// the registry's active_collection pointer names which one currently serves
// reads. A NULL pointer means the bare <view> collection is active (the
// pre-blue-green state), so the physical set a view can resolve to is
// {<view>, <view>__0, <view>__1} — three states only until the first flip.
const (
	slotSuffix0 = "__0"
	slotSuffix1 = "__1"
)

// PhysicalCollectionNames returns every physical collection a view named viewName
// can occupy: the bare <view> (pre-first-flip / no blue-green) plus its two slots
// <view>__0 and <view>__1. Any consumer that must recognize where a view may
// physically live — most importantly the DB-per-service foreign-collection guard,
// which would otherwise flag a view's own active/shadow slot as an orphan and
// abort a non-dev boot — whitelists all three.
func PhysicalCollectionNames(viewName string) []string {
	return []string{viewName, viewName + slotSuffix0, viewName + slotSuffix1}
}

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

// viewPointer is the cached slot state of one view. active is the collection
// currently serving reads ("" means NULL ⇒ the bare view name); shadow is the
// slot a rebuild is building ("" means no rebuild in flight).
type viewPointer struct {
	active string
	shadow string
}

// ViewResolver maps a logical view name to the PhysicalCollection currently
// serving it. The mapping is served from an in-memory pointer cache: the
// relational registry is consulted only by Refresh (at boot and, later, on a
// slow per-lease cadence), never per operation, because the active-slot pointer
// changes only on a rebuild flip (a rare event). One instance is shared
// process-wide so every read-model component observes a single, consistent
// pointer.
//
// A view absent from the cache (never refreshed, or a name that is not a managed
// view — an externally-materialized upstream/embed collection) resolves to the
// bare name, so an unwired or nil resolver behaves exactly as the pre-blue-green
// code.
type ViewResolver struct {
	eng   core.RelationalEngine
	lease time.Duration

	mu          sync.RWMutex
	cache       map[string]viewPointer
	lastRefresh time.Time
}

// NewViewResolver builds the process-wide resolver with the default lease. eng
// backs Refresh (via its neutral Querier/Dialect); a nil eng makes Refresh a
// no-op, so a resolver built without a backend resolves every name to identity —
// the shape tests rely on.
func NewViewResolver(eng core.RelationalEngine) *ViewResolver {
	return NewViewResolverWithLease(eng, defaultResolverLease)
}

// NewViewResolverWithLease is NewViewResolver with an explicit bounded-staleness
// lease — the operator knob (mongo.rebuild.pointerLeaseSeconds) that tunes how
// long a rebuild driver waits for every pod's fence before backfilling (and thus
// how long a boot rebuild's fence waits block). A non-positive lease falls back
// to the default.
func NewViewResolverWithLease(eng core.RelationalEngine, lease time.Duration) *ViewResolver {
	if lease <= 0 {
		lease = defaultResolverLease
	}
	return &ViewResolver{eng: eng, lease: lease, cache: map[string]viewPointer{}}
}

// Active returns the collection currently serving reads for the given logical
// name: its cached active slot, or the bare name when the pointer is NULL, the
// view is absent from the cache, or the resolver is nil.
func (r *ViewResolver) Active(name string) PhysicalCollection {
	if r == nil {
		return PhysicalCollection{name: name}
	}
	r.mu.RLock()
	p, ok := r.cache[name]
	r.mu.RUnlock()
	if ok && p.active != "" {
		return PhysicalCollection{name: p.active}
	}
	return PhysicalCollection{name: name}
}

// Shadow returns the inactive slot a rebuild builds for the given view: the
// other of the two slots relative to the current active one. From the bare
// state the first shadow is <view>__0; thereafter it alternates __0 ↔ __1.
func (r *ViewResolver) Shadow(name string) PhysicalCollection {
	active := name
	if r != nil {
		r.mu.RLock()
		if p, ok := r.cache[name]; ok && p.active != "" {
			active = p.active
		}
		r.mu.RUnlock()
	}
	return PhysicalCollection{name: inactiveSlot(name, active)}
}

// inactiveSlot returns the slot that is NOT the current active one. From bare
// (active == view name or empty) the first build target is __0.
func inactiveSlot(viewName, active string) string {
	switch active {
	case viewName + slotSuffix0:
		return viewName + slotSuffix1
	case viewName + slotSuffix1:
		return viewName + slotSuffix0
	default:
		return viewName + slotSuffix0
	}
}

// sqlLoadViewPointers reads every view's slot pointers in one scan. Bare column
// identifiers are valid unquoted on every dialect; no placeholders, so it is
// dialect-agnostic.
const sqlLoadViewPointers = `SELECT view_name, active_collection, shadow_collection
FROM omnicore_mongo_views`

// Refresh reloads the whole pointer cache from the registry. Called at boot and
// (from Phase 3) on the bounded-staleness lease. A nil resolver or a resolver
// with no backend (nil eng — the test shape) is a no-op: the cache stays empty
// and every name resolves to identity. Refresh swaps the cache atomically, so
// concurrent Active/Shadow readers always see a whole, consistent snapshot.
func (r *ViewResolver) Refresh(ctx context.Context) error {
	if r == nil || r.eng == nil {
		return nil
	}
	rows, err := r.eng.Querier().Query(ctx, sqlLoadViewPointers)
	if err != nil {
		return fmt.Errorf("view resolver refresh: %w", err)
	}
	defer rows.Close()

	next := make(map[string]viewPointer)
	for rows.Next() {
		var name string
		var active, shadow *string
		if err := rows.Scan(&name, &active, &shadow); err != nil {
			return fmt.Errorf("view resolver refresh: %w", err)
		}
		p := viewPointer{}
		if active != nil {
			p.active = *active
		}
		if shadow != nil {
			p.shadow = *shadow
		}
		next[name] = p
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("view resolver refresh: %w", err)
	}

	r.mu.Lock()
	r.cache = next
	r.lastRefresh = time.Now()
	r.mu.Unlock()
	return nil
}

// ShadowActive reports whether a rebuild is in flight for the view — i.e. its
// registry row records a shadow slot — and returns that slot. The SyncEngine
// consults it per write: when true, dual-apply fans the recompose/delete into
// the shadow as well as the active slot.
func (r *ViewResolver) ShadowActive(name string) (PhysicalCollection, bool) {
	if r == nil {
		return PhysicalCollection{}, false
	}
	r.mu.RLock()
	p, ok := r.cache[name]
	r.mu.RUnlock()
	if ok && p.shadow != "" {
		return PhysicalCollection{name: p.shadow}, true
	}
	return PhysicalCollection{}, false
}

// EnsureFresh is the G3 activation fence: if the cache is older than the lease it
// re-reads the registry before the caller applies anything, and surfaces the
// error if the re-read fails so the caller can stop consuming rather than apply
// with a stale view of which rebuilds are active. A nil resolver or one with no
// backend is always "fresh" (nothing to fence).
func (r *ViewResolver) EnsureFresh(ctx context.Context) error {
	if r == nil || r.eng == nil {
		return nil
	}
	r.mu.RLock()
	stale := time.Since(r.lastRefresh) > r.lease
	r.mu.RUnlock()
	if !stale {
		return nil
	}
	return r.Refresh(ctx)
}
