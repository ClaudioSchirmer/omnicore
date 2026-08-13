//go:build postgres

package postgres

import "github.com/ClaudioSchirmer/omnicore/domain"

// T3 (aggregate child equality) migration for this package's unit test AVO
// fixture. AggregateValueObject now requires IsSameBusinessIdentity; covChild
// has no natural business key, so it delegates to IsSameByBusinessFields — the
// faithful structural equivalent of the change tracker's prior DeepEqual guess.

func (c covChild) IsSameBusinessIdentity(o domain.AggregateValueObject) bool {
	return domain.IsSameByBusinessFields(c, o)
}

func (covChild) CollectionName() string { return "CovChilds" }
