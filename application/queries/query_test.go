package queries

import "testing"

// Compile-time proof that QueryBaseWithID satisfies QueryWithID through
// its pointer-receiver SetPathID. If embedding ever drifts (e.g. someone
// removes pipeline.QueryBase) this fails to compile, surfacing the regression
// before any test runs.
var _ QueryWithID = (*QueryBaseWithID)(nil)

func TestQueryBaseWithID_SetGetIDRoundtrip(t *testing.T) {
	q := &QueryBaseWithID{}
	q.SetPathID("a1b2c3")
	if got := q.GetID().Value(); got != "a1b2c3" {
		t.Errorf("expected ID value 'a1b2c3', got %q", got)
	}
}

func TestQueryBaseWithID_ZeroValueIsEmpty(t *testing.T) {
	var q QueryBaseWithID
	if !q.GetID().IsEmpty() {
		t.Errorf("expected zero-value ID to be empty, got %q", q.GetID().Value())
	}
}
