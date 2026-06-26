package integration

import (
	"strings"
	"testing"
)

func TestValidateSubscriptionsCovered_NilConfig(t *testing.T) {
	if err := ValidateSubscriptionsCovered(nil, nil); err != nil {
		t.Fatalf("nil config must be a no-op, got: %v", err)
	}
}

func TestValidateSubscriptionsCovered_AllCovered(t *testing.T) {
	cfg := &Config{
		Subscribes: map[string]SubscribeEntry{
			"partners": {
				Topic: "partners.events",
				Events: map[string]SubscribeEvent{
					"onboarded": {EventType: "PartnerOnboarded"},
				},
			},
		},
	}
	receivers := []*Receiver{{sourceKey: "partners", eventKey: "onboarded"}}
	if err := ValidateSubscriptionsCovered(cfg, receivers); err != nil {
		t.Fatalf("fully covered subscriptions must pass, got: %v", err)
	}
}

func TestValidateSubscriptionsCovered_OrphanDeclaration(t *testing.T) {
	cfg := &Config{
		Subscribes: map[string]SubscribeEntry{
			"partners": {
				Topic: "partners.events",
				Events: map[string]SubscribeEvent{
					"onboarded":  {EventType: "PartnerOnboarded"},
					"offboarded": {EventType: "PartnerOffboarded"},
				},
			},
		},
	}
	// Only one of the two declared events has a receiver.
	receivers := []*Receiver{{sourceKey: "partners", eventKey: "onboarded"}}
	err := ValidateSubscriptionsCovered(cfg, receivers)
	if err == nil {
		t.Fatal("expected error for declared subscription without a receiver")
	}
	if !strings.Contains(err.Error(), "integration.subscribes.partners.events.offboarded") {
		t.Errorf("error must name the orphan coordinate, got: %v", err)
	}
	if strings.Contains(err.Error(), "events.onboarded") {
		t.Errorf("covered event must not be flagged, got: %v", err)
	}
}

func TestValidateSubscriptionsCovered_NoReceiversAtAll(t *testing.T) {
	cfg := &Config{
		Subscribes: map[string]SubscribeEntry{
			"partners": {
				Topic:  "partners.events",
				Events: map[string]SubscribeEvent{"onboarded": {EventType: "PartnerOnboarded"}},
			},
		},
	}
	// Registry empty (operator forgot MountReceivers entirely).
	err := ValidateSubscriptionsCovered(cfg, nil)
	if err == nil {
		t.Fatal("expected error when no receivers are registered for declared subscriptions")
	}
	if !strings.Contains(err.Error(), "partners.events.onboarded") {
		t.Errorf("error must name the orphan, got: %v", err)
	}
}
