package integration

import (
	"strings"
	"testing"
)

func rcv(sourceKey, eventKey, topic, group, wireEventType string, workers int) *Receiver {
	return &Receiver{
		sourceKey:     sourceKey,
		eventKey:      eventKey,
		topic:         topic,
		consumerGroup: group,
		wireEventType: wireEventType,
		workers:       workers,
		startFrom:     "latest",
	}
}

func TestGroupReceivers_TwoEventsOneSource_SingleGroup(t *testing.T) {
	// The canonical From("partners").On(A).On(B) shape: same topic + group,
	// two event types. Must collapse to ONE consumer group with both
	// event types routed inside it (the whole point of the demux fix).
	recs := []*Receiver{
		rcv("partners", "onboarded", "partners.events", "svc-int", "PartnerOnboarded", 2),
		rcv("partners", "offboarded", "partners.events", "svc-int", "PartnerOffboarded", 2),
	}
	groups, err := groupReceivers(recs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("expected 1 consumer group, got %d", len(groups))
	}
	var g *receiverGroup
	for _, v := range groups {
		g = v
	}
	if len(g.byEventType) != 2 {
		t.Fatalf("expected 2 event types in the group, got %d", len(g.byEventType))
	}
	if g.byEventType["PartnerOnboarded"] == nil || g.byEventType["PartnerOffboarded"] == nil {
		t.Errorf("both event types must be routable, got %+v", g.byEventType)
	}
}

func TestGroupReceivers_DifferentTopics_SeparateGroups(t *testing.T) {
	recs := []*Receiver{
		rcv("a", "x", "a.events", "svc-int", "X", 1),
		rcv("b", "y", "b.events", "svc-int", "Y", 1),
	}
	groups, err := groupReceivers(recs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(groups) != 2 {
		t.Fatalf("distinct topics must produce distinct groups, got %d", len(groups))
	}
}

func TestGroupReceivers_DuplicateEventType_Rejected(t *testing.T) {
	recs := []*Receiver{
		rcv("partners", "a", "partners.events", "svc-int", "PartnerOnboarded", 1),
		rcv("partners", "b", "partners.events", "svc-int", "PartnerOnboarded", 1),
	}
	_, err := groupReceivers(recs)
	if err == nil {
		t.Fatal("expected error: two receivers on the same (topic,group,event_type)")
	}
	if !strings.Contains(err.Error(), "PartnerOnboarded") || !strings.Contains(err.Error(), "two handlers") {
		t.Errorf("error must name the colliding event type, got: %v", err)
	}
}

func TestGroupReceivers_WorkersMax(t *testing.T) {
	recs := []*Receiver{
		rcv("p", "a", "t", "g", "A", 2),
		rcv("p", "b", "t", "g", "B", 8),
	}
	groups, _ := groupReceivers(recs)
	for _, g := range groups {
		if g.workers != 8 {
			t.Errorf("group workers must be the max across receivers, got %d", g.workers)
		}
	}
}

func TestReceiverGroup_Route(t *testing.T) {
	a := rcv("p", "a", "t", "g", "A", 1)
	b := rcv("p", "b", "t", "g", "B", 1)
	multi := &receiverGroup{byEventType: map[string]*Receiver{"A": a, "B": b}}
	single := &receiverGroup{byEventType: map[string]*Receiver{"A": a}}

	// Matching event_type routes to the right receiver — this is exactly
	// what the per-receiver topology got wrong (B's reader would drop A).
	if got := multi.route("A"); got != a {
		t.Errorf("event_type A must route to receiver a")
	}
	if got := multi.route("B"); got != b {
		t.Errorf("event_type B must route to receiver b")
	}
	// Foreign / unmatched event_type on a multi-receiver group → skip.
	if got := multi.route("Z"); got != nil {
		t.Errorf("unknown event_type must be unroutable, got %v", got)
	}
	// Absent event_type, multiple receivers → unroutable.
	if got := multi.route(""); got != nil {
		t.Errorf("absent event_type with >1 receiver must be unroutable, got %v", got)
	}
	// Absent event_type, single receiver → back-compat delivery.
	if got := single.route(""); got != a {
		t.Errorf("absent event_type with a single receiver must deliver to it")
	}
	// Present-but-unmatched on a single-receiver group → skip (a foreign
	// event must NOT be force-fed to the lone handler).
	if got := single.route("B"); got != nil {
		t.Errorf("present-but-unmatched event_type must be unroutable even on a single-receiver group, got %v", got)
	}
}
