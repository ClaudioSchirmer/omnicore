package query

import "github.com/ClaudioSchirmer/omnicore/domain"

// T3 (aggregate child equality) migration for this package's test AVO fixtures.
// AggregateValueObject now requires IsSameBusinessIdentity; these fixtures have
// no natural business key, so they delegate to IsSameByBusinessFields — the
// faithful structural equivalent of the change tracker's prior DeepEqual guess.

func (c composerRoleChild) IsSameBusinessIdentity(o domain.AggregateValueObject) bool {
	return domain.IsSameByBusinessFields(c, o)
}

func (c csComposeVO) IsSameBusinessIdentity(o domain.AggregateValueObject) bool {
	return domain.IsSameByBusinessFields(c, o)
}

func (c expAddr) IsSameBusinessIdentity(o domain.AggregateValueObject) bool {
	return domain.IsSameByBusinessFields(c, o)
}

func (c fakeVO) IsSameBusinessIdentity(o domain.AggregateValueObject) bool {
	return domain.IsSameByBusinessFields(c, o)
}
