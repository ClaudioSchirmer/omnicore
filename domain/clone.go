package domain

import (
	"encoding/json"
	"reflect"
)

// cloneEntity returns a fresh instance of e's concrete type with the exported
// fields copied via JSON round-trip. Private fields of BaseEntity / AggregateRoot
// (notifCtx, events, signature, aggregates) are NOT copied — the clone is a
// frozen "ghost" of the loaded state, used as read-only old-state snapshot
// (consumed by domain.Old and the auditor). Returns nil when e is nil or the
// round-trip fails (defensive — degrade to no Old() rather than panic).
func cloneEntity(e Entity) Entity {
	if e == nil {
		return nil
	}
	data, err := json.Marshal(e)
	if err != nil {
		return nil
	}
	t := reflect.TypeOf(e)
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	cloneVal := reflect.New(t)
	if err := json.Unmarshal(data, cloneVal.Interface()); err != nil {
		return nil
	}
	clone, ok := cloneVal.Interface().(Entity)
	if !ok {
		return nil
	}
	return clone
}

// captureOld snapshots the entity's current state and stores it on the
// entity itself, accessible later via Old() / domain.Old. It OVERWRITES any
// existing snapshot — the framework's own paths call captureOldIfAbsent
// instead, so the earliest (birth-time) snapshot always wins.
//
// For aggregate roots (entities implementing AggregateRootProvider), the
// clone also receives a deep copy of the aggregate's current children so
// Old() can expose Addresses, OrderLines, etc. as they were before mutation.
func captureOld(self Entity) {
	if self == nil {
		return
	}
	clone := cloneEntity(self)
	if clone == nil {
		return
	}
	if origProv, ok := self.(AggregateRootProvider); ok {
		if cloneProv, ok2 := clone.(AggregateRootProvider); ok2 {
			origAR := origProv.GetAggregateRoot()
			cloneAR := cloneProv.GetAggregateRoot()
			if origAR != nil && cloneAR != nil {
				cloneAR.aggregates = copyAggregateMap(origAR.aggregates)
			}
		}
	}
	self.setOldEntity(clone)
}

// captureOldIfAbsent is the framework's ONE capture rule, identical for all
// five state-changing verbs (Update, PartialUpdate, Archive, Unarchive,
// Delete): snapshot the entity as it was BORN — hydrated from the system of
// record — and never let a later call replace it.
//
// The snapshot is taken at hydration (CaptureOld, called by the relational
// loader's single-entity path and by the write-side load helpers), so by the
// time any Get* runs, Old() already holds the persisted state and every
// mutation that happened in between — a Command's ApplyTo, a BuildRules
// closure, a manual handler's own domain calls — is correctly EXCLUDED from
// it. This no-ops in that case.
//
// It still captures when no snapshot exists, which is the floor for an entity
// built by hand and never loaded (Old() would otherwise stay nil). Insert is
// the one verb that never captures at all: a freshly constructed entity has no
// prior state by definition, so Old() returns the zero value of T.
func captureOldIfAbsent(self Entity) {
	if self == nil || self.Old() != nil {
		return
	}
	captureOld(self)
}

// copyAggregateMap shallow-copies the aggregate items map so the clone owns its
// own slice headers. Items themselves are value types (AggregateValueObject is
// a value-type contract per the framework convention), so value copy is enough.
func copyAggregateMap(src map[string][]aggregateItemEntry) map[string][]aggregateItemEntry {
	if src == nil {
		return nil
	}
	out := make(map[string][]aggregateItemEntry, len(src))
	for k, list := range src {
		cp := make([]aggregateItemEntry, len(list))
		copy(cp, list)
		out[k] = cp
	}
	return out
}

// Old returns the pre-mutation snapshot of e as the same type T, or the zero
// value of T when no snapshot exists (Insert path, or entity hydrated outside
// the framework's loader / Get* flow). Designed to be called inside BuildRules
// for transition-aware invariants:
//
//	r.IfUpdate(func() {
//	    if old := domain.Old(u); old != nil {
//	        if old.Email != u.Email && u.Activated {
//	            r.AddNotification("Email", EmailLockedAfterActivationNotification{})
//	        }
//	    }
//	})
//
// The returned entity is a read-only ghost — its NotificationContext is nil,
// calling mutator domain methods on it is a no-op. Aggregates also expose the
// prior children via the usual helpers (GetCurrentItemsOf[Address]).
func Old[T Entity](e T) T {
	var zero T
	p := e.Old()
	if p == nil {
		return zero
	}
	typed, ok := p.(T)
	if !ok {
		return zero
	}
	return typed
}
