package domain

// AggregateRootProvider is implemented by entities that embed AggregateRoot and
// want to be persisted atomically with their child collections.
//
// Table and ParentID names are declared explicitly in infra via the per-Repository
// fwinfra.TableSchema (root + Child schemas). Domain stays DDD-pure — it does
// not pronounce table/column/ParentID.
//
// Phase 20: the root regains authority over the aggregate boundary —
// AggregateChildren() declares which AVO types belong to this aggregate.
// The framework's typed primitives (AddAggregateChild, ChangeAggregateChild,
// RemoveAggregateChild, ReplaceAggregateChildrenOf) consult this set and
// reject items whose type is not declared, emitting
// InvalidAggregateChildNotification on the root's NotificationContext.
//
// Cascade is symmetric and universal:
//   - Archive(root)   → Archive all children
//   - Unarchive(root) → Unarchive all children archived
//   - Delete(root)    → Delete all children via ParentID ON DELETE CASCADE
//   - Update root with StatusRemoved children → Archive those children
type AggregateRootProvider interface {
	// GetAggregateRoot returns the embedded *AggregateRoot so the persister
	// can iterate child items via AllAggregateItems().
	GetAggregateRoot() *AggregateRoot

	// AggregateChildren returns one sample instance per AVO type that belongs
	// to this aggregate. The framework reads classNameOf(sample) via reflect
	// to build the set of allowed types; the sample values themselves are
	// never used. Return a fresh slice each call (or a package-level var) —
	// callers must not mutate it.
	//
	// Example:
	//
	//	func (u *User) AggregateChildren() []domain.AggregateValueObject {
	//	    return []domain.AggregateValueObject{Address{}}
	//	}
	//
	// An empty slice declares the root has no aggregate children (still a
	// valid aggregate — useful for roots that may grow children later).
	AggregateChildren() []AggregateValueObject
}
