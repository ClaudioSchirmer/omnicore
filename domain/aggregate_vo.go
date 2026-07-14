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
	// scoped with the inferred collection name (snake_case plural of the AVO
	// type — Address → addresses) and the iteration index — so
	// r.AddNotification("ZipCode", n) renders to the wire as
	// "addresses[0].zipCode" without the AVO knowing it is a child.
	BuildRules(actionName string, service Service, r *Rules)

	// GetID returns the existing row id when the item was loaded from the
	// database (StatusConstructor) or set by the persister after INSERT.
	// Returns an empty ID (IsEmpty()) for new items not persisted yet. The
	// AVO carries it as an exported `ID domain.ID` field — identity is a
	// type, on children exactly as on references.
	GetID() ID
}

type ValueObjectValidator interface {
	IsValid(fieldName string, ctx *NotificationContext) bool
}
