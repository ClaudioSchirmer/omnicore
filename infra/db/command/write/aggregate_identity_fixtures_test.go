package write

import "github.com/ClaudioSchirmer/omnicore/domain"

// T3 (aggregate child equality) migration for this package's test AVO fixtures.
// AggregateValueObject now requires IsSameBusinessIdentity; these fixtures have
// no natural business key, so they delegate to IsSameByBusinessFields — the
// faithful structural equivalent of the change tracker's prior DeepEqual guess.

func (c aggRoleChild) IsSameBusinessIdentity(o domain.AggregateValueObject) bool {
	return domain.IsSameByBusinessFields(c, o)
}

func (c aggWriteChild) IsSameBusinessIdentity(o domain.AggregateValueObject) bool {
	return domain.IsSameByBusinessFields(c, o)
}

func (c bcAddr) IsSameBusinessIdentity(o domain.AggregateValueObject) bool {
	return domain.IsSameByBusinessFields(c, o)
}

func (c cascadeBaseChild) IsSameBusinessIdentity(o domain.AggregateValueObject) bool {
	return domain.IsSameByBusinessFields(c, o)
}

func (c covChild) IsSameBusinessIdentity(o domain.AggregateValueObject) bool {
	return domain.IsSameByBusinessFields(c, o)
}

func (c csChild) IsSameBusinessIdentity(o domain.AggregateValueObject) bool {
	return domain.IsSameByBusinessFields(c, o)
}

func (c labelTestAddress) IsSameBusinessIdentity(o domain.AggregateValueObject) bool {
	return domain.IsSameByBusinessFields(c, o)
}

func (c sbAuditAddr) IsSameBusinessIdentity(o domain.AggregateValueObject) bool {
	return domain.IsSameByBusinessFields(c, o)
}
