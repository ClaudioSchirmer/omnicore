//go:build integration && sqlserver

package sqlserver

import "github.com/ClaudioSchirmer/omnicore/domain"

// T3 (aggregate child equality) migration for this package's integration test
// AVO fixture. AggregateValueObject now requires IsSameBusinessIdentity; tag has
// no natural business key, so it delegates to IsSameByBusinessFields — the
// faithful structural equivalent of the change tracker's prior DeepEqual guess.

func (t tag) IsSameBusinessIdentity(o domain.AggregateValueObject) bool {
	return domain.IsSameByBusinessFields(t, o)
}
