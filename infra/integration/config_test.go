package integration

import (
	"strings"
	"testing"
)

func TestConfigApplyDefaults(t *testing.T) {
	c := &Config{
		Subscribes: map[string]SubscribeEntry{
			"partners": {Topic: "partners.events"},
		},
	}
	c.ApplyDefaults("orders")
	if c.Defaults.ConsumerGroup != "orders-integration" {
		t.Fatalf("expected default consumerGroup orders-integration, got %q", c.Defaults.ConsumerGroup)
	}
	if c.Subscribes["partners"].ConsumerGroup != "orders-integration" {
		t.Fatalf("expected per-source default propagated, got %q", c.Subscribes["partners"].ConsumerGroup)
	}
}

func TestConfigValidateMissingFields(t *testing.T) {
	c := &Config{
		Publishes: PublishConfig{
			Events: map[string]PublishEvent{
				"x": {EventType: "", Version: -1},
			},
		},
		Subscribes: map[string]SubscribeEntry{
			"src": {Topic: "", Events: map[string]SubscribeEvent{
				"y": {EventType: ""},
			}},
		},
	}
	err := c.Validate()
	if err == nil {
		t.Fatal("expected validation errors")
	}
	if !strings.Contains(err.Error(), "publishes.events.x.eventType") {
		t.Errorf("expected eventType missing diagnostic, got: %v", err)
	}
	if !strings.Contains(err.Error(), "subscribes.src.topic") {
		t.Errorf("expected topic missing diagnostic, got: %v", err)
	}
}

func TestConfigLookupPublish(t *testing.T) {
	c := &Config{
		Publishes: PublishConfig{
			Events: map[string]PublishEvent{
				"k": {EventType: "T", Aggregate: "A", Version: 2},
			},
		},
	}
	got, ok := c.LookupPublish("k")
	if !ok || got.EventType != "T" {
		t.Fatalf("expected lookup hit, got %+v ok=%v", got, ok)
	}
	if _, ok := c.LookupPublish("missing"); ok {
		t.Fatal("expected miss for unknown key")
	}
}

func TestConfigLookupSubscribe(t *testing.T) {
	c := &Config{
		Subscribes: map[string]SubscribeEntry{
			"src": {
				Topic: "topic",
				Events: map[string]SubscribeEvent{
					"ev": {EventType: "Ev"},
				},
			},
		},
	}
	topic, evType, ok := c.LookupSubscribe("src", "ev")
	if !ok || topic != "topic" || evType != "Ev" {
		t.Fatalf("expected hit, got topic=%q type=%q ok=%v", topic, evType, ok)
	}
	if _, _, ok := c.LookupSubscribe("src", "nope"); ok {
		t.Fatal("expected miss for unknown event key")
	}
	if _, _, ok := c.LookupSubscribe("nope", "ev"); ok {
		t.Fatal("expected miss for unknown source key")
	}
}
