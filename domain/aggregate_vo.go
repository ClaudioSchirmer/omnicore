package domain

type AggregateValueObject interface {
	// BuildRules declares this child's validation rules. The signature is
	// intentionally identical to Entity.BuildRules so root and aggregate
	// children read the same way:
	//
	//	func (a Address) BuildRules(actionName string, svc domain.Service, r *domain.Rules) {
	//	    r.IfInsertOrUpdate(func() {
	//	        if a.Street == "" {
	//	            r.AddNotification("Street", domain.RequiredFieldNotification{})
	//	        }
	//	    })
	//	}
	//
	// actionName is propagated from the same caller that drives the root's
	// BuildRules (typically "GetInsertable"/"GetUpdatable"/"GetDeletable", or a
	// service-provided custom action). AVOs can branch by actionName to model
	// distinct validation flavours that share the same EntityMode — e.g.
	// stricter rules under an "AdminCreate" insert than a regular one.
	//
	// The framework hands in a *Rules whose NotificationContext is already
	// scoped with the DECLARED collection name (CollectionName below, rendered
	// lower-camel for the wire) and the iteration index — so
	// r.AddNotification("ZipCode", n) renders to the wire as
	// "addresses[0].zipCode" without the AVO knowing it is a child.
	BuildRules(actionName string, service Service, r *Rules)

	// CollectionName is the name of the collection this child occupies inside
	// its owning aggregate — declared, never inferred. It is the ONE name for
	// the child collection across the whole framework, with exactly two
	// consumers:
	//
	//   - the read side, where it is the document segment the projection nests
	//     the collection under and the Go segment a filter/sort path walks
	//     ("Addresses" → doc key "Addresses"; ?addresses.city resolves through
	//     it). It must therefore match the field name the read DTO declares for
	//     the collection.
	//   - the notification wire path, where the framework renders it
	//     lower-camel ("Addresses" → "addresses[0].zipCode").
	//
	// Declare the plural the DOMAIN uses, in the domain's own language —
	// "Addresses", "OrderLines", "Enderecos", "Adressen". The framework applies
	// no pluralization rule of its own: an English heuristic cannot spell a
	// Portuguese or German collection, and a name that is merely derived is a
	// name nobody declared.
	//
	// Contract: a constant, valid as an exported Go field name — first rune an
	// ASCII uppercase letter (A-Z), every other rune a letter or a digit. The
	// framework resolves it once per type from a zero value and caches it, so
	// it must not depend on the receiver's state:
	//
	//	func (Address) CollectionName() string { return "Addresses" }
	CollectionName() string

	// GetID returns the existing row id when the item was loaded from the
	// database (StatusConstructor) or set by the persister after INSERT.
	// Returns an empty ID (IsEmpty()) for new items not persisted yet. The
	// AVO carries it as an exported `ID domain.ID` field — identity is a
	// type, on children exactly as on references.
	GetID() ID

	// IsSameBusinessIdentity answers "is this the SAME child?" from the owning
	// aggregate's business point of view — NOT "did nothing change?". It is the
	// declared replacement for the old reflect.DeepEqual structural guess the
	// change tracker used: the framework matches an item against the tracked
	// collection (add/re-activate, change, remove, id write-back) exclusively
	// through this method, so ONLY the domain can say what "same" means. Two
	// Address values are the same place by Country+ZipCode+Street+Number even if
	// Label/Complement differ; DeepEqual saw them as distinct, the domain does
	// not.
	//
	// other arrives as the interface; type-assert to the concrete AVO and
	// return false on a type mismatch:
	//
	//	func (a Address) IsSameBusinessIdentity(other domain.AggregateValueObject) bool {
	//	    o, ok := other.(Address)
	//	    return ok && a.Country == o.Country && a.ZipCode == o.ZipCode &&
	//	        a.Street == o.Street && a.Number == o.Number
	//	}
	//
	// Contract: at most ONE active child per identity — a second active child
	// with the same identity is rejected as a duplicate (the framework enforces
	// this via the add path). When the aggregate's notion of identity is simply
	// "every business field", delegate to domain.IsSameByBusinessFields(a, other).
	IsSameBusinessIdentity(other AggregateValueObject) bool
}
