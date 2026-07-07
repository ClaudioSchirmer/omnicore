package queries

import (
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// Compile-time proof that QueryByIDBase satisfies the id-carrying half of
// QueryByID through its pointer-receiver SetPathID (ToCriteria and
// ContextName stay with the concrete query). If embedding ever drifts
// (e.g. someone removes pipeline.QueryBase) this fails to compile,
// surfacing the regression before any test runs.
var _ interface {
	SetPathID(id string)
	PathID() domain.ID
} = (*QueryByIDBase)(nil)

func TestQueryByIDBase_SetPathIDRoundtrip(t *testing.T) {
	q := &QueryByIDBase{}
	q.SetPathID("a1b2c3")
	if got := q.PathID().Value(); got != "a1b2c3" {
		t.Errorf("expected ID value 'a1b2c3', got %q", got)
	}
}

func TestQueryByIDBase_ZeroValueIsEmpty(t *testing.T) {
	var q QueryByIDBase
	if !q.PathID().IsEmpty() {
		t.Errorf("expected zero-value ID to be empty, got %q", q.PathID().Value())
	}
}
