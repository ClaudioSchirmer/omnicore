package domain

import "testing"

// Stamp is the domain's half of a stamped field: it names WHEN, never with what
// value. What it accumulates belongs to one write, not to the entity's state.

type stampEntity struct {
	BaseEntity
	Name string
}

func TestStamp_AccumulatesRequestsInOrder(t *testing.T) {
	e := &stampEntity{}
	if got := RequestedStamps(e); got != nil {
		t.Fatalf("a fresh entity has requested nothing, got %v", got)
	}
	e.Stamp("PaidAt")
	e.Stamp("ShippedAt")
	got := RequestedStamps(e)
	if len(got) != 2 || got[0] != "PaidAt" || got[1] != "ShippedAt" {
		t.Fatalf("requests must survive in order, got %v", got)
	}
}

// Two rules may reach the same conclusion; that is one stamp, not two columns.
func TestStamp_IsIdempotent(t *testing.T) {
	e := &stampEntity{}
	e.Stamp("PaidAt")
	e.Stamp("PaidAt")
	if got := RequestedStamps(e); len(got) != 1 {
		t.Fatalf("asking twice is asking once, got %v", got)
	}
}

// The request is intent for one write, so it must stay out of everything that
// describes the entity. Business identity is the sharpest test of that: it
// compares two values structurally, and the carrier is unexported precisely so
// it takes no part.
type stampItem struct {
	Managed
	Label string
}

func (i stampItem) IsSameBusinessIdentity(other AggregateValueObject) bool {
	return IsSameByBusinessFields(i, other)
}
func (stampItem) CollectionName() string             { return "items" }
func (stampItem) BuildRules(string, Service, *Rules) {}

func TestStamp_DoesNotAffectBusinessIdentity(t *testing.T) {
	a := stampItem{Label: "x"}
	b := stampItem{Label: "x"}
	a.Stamp("PaidAt")
	if !a.IsSameBusinessIdentity(b) {
		t.Fatal("a stamp request must not enter business identity")
	}
}

// An aggregate child embeds the carrier directly (not through BaseEntity), so
// the seam has to work on it too — even though attaching a stamped field to a
// child schema is refused at boot, the promotion itself must be sound.
func TestStamp_WorksThroughADirectlyEmbeddedCarrier(t *testing.T) {
	type item struct {
		Managed
		Label string
	}
	i := &item{Label: "l"}
	i.Stamp("DoneAt")
	if got := RequestedStamps(i); len(got) != 1 || got[0] != "DoneAt" {
		t.Fatalf("the carrier promotes Stamp to any embedder, got %v", got)
	}
}
