//go:build integration && postgres

package postgres

import "github.com/ClaudioSchirmer/omnicore/domain"

// T3 (aggregate child equality) migration for this package's INTEGRATION test
// AVO fixtures. AggregateValueObject now requires IsSameBusinessIdentity; these
// fixtures have no natural business key, so they delegate to
// IsSameByBusinessFields — the faithful structural equivalent of the change
// tracker's prior DeepEqual guess.

func (c aggChannel) IsSameBusinessIdentity(o domain.AggregateValueObject) bool {
	return domain.IsSameByBusinessFields(c, o)
}

func (l lineItem) IsSameBusinessIdentity(o domain.AggregateValueObject) bool {
	return domain.IsSameByBusinessFields(l, o)
}

func (v loaderNoteVO) IsSameBusinessIdentity(o domain.AggregateValueObject) bool {
	return domain.IsSameByBusinessFields(v, o)
}

func (v loaderTagVO) IsSameBusinessIdentity(o domain.AggregateValueObject) bool {
	return domain.IsSameByBusinessFields(v, o)
}

func (v schemaTag) IsSameBusinessIdentity(o domain.AggregateValueObject) bool {
	return domain.IsSameByBusinessFields(v, o)
}
