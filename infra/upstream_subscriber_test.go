package infra

import (
	"reflect"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestUpstreamSubscriber_DecodePayload_RawJSON(t *testing.T) {
	s := &UpstreamSubscriber{}
	got, err := s.decodePayload([]byte(`{"name":"Alice","email":"alice@x"}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["name"] != "Alice" || got["email"] != "alice@x" {
		t.Errorf("got %+v", got)
	}
}

func TestUpstreamSubscriber_DecodePayload_NestedPayloadEnvelope(t *testing.T) {
	s := &UpstreamSubscriber{}
	got, err := s.decodePayload([]byte(`{"schema":{},"payload":{"name":"Alice"}}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["name"] != "Alice" {
		t.Errorf("expected payload to be unwrapped, got %+v", got)
	}
}

func TestUpstreamSubscriber_DecodePayload_Empty(t *testing.T) {
	s := &UpstreamSubscriber{}
	got, err := s.decodePayload(nil)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty map, got %+v", got)
	}
}

func TestUpstreamSubscriber_ApplyFilter_KeepsOnlyAllowed(t *testing.T) {
	s := &UpstreamSubscriber{cfg: UpstreamSubscriberConfig{Filter: []string{"name", "email"}}}
	in := bson.M{"name": "Alice", "email": "a@x", "ssn": "secret", "address": "x"}
	got := s.applyFilter(in)
	if len(got) != 2 || got["name"] != "Alice" || got["email"] != "a@x" {
		t.Errorf("filter dropped allowed keys or kept disallowed: %+v", got)
	}
	if _, present := got["ssn"]; present {
		t.Error("ssn should have been dropped")
	}
}

func TestUpstreamSubscriber_ApplyFilter_EmptyAllowlist(t *testing.T) {
	// applyFilter is only called when len(Filter) > 0; with an empty
	// allowlist every key is filtered out. This documents that the
	// caller (processMessage) skips the call when Filter is empty.
	s := &UpstreamSubscriber{cfg: UpstreamSubscriberConfig{}}
	in := bson.M{"name": "Alice"}
	got := s.applyFilter(in)
	if len(got) != 0 {
		t.Errorf("empty filter should produce empty map, got %+v", got)
	}
}

func TestUpstreamSubscriber_ParseOffsetSeek_Symbolic(t *testing.T) {
	for _, sf := range []string{upstreamStartFromLatest, upstreamStartFromEarliest, ""} {
		s := &UpstreamSubscriber{cfg: UpstreamSubscriberConfig{StartFrom: sf}}
		if err := s.parseOffsetSeek(); err != nil {
			t.Errorf("symbolic StartFrom=%q: %v", sf, err)
		}
		if s.offsetSeekTarget != nil {
			t.Errorf("symbolic StartFrom=%q should leave offsetSeekTarget nil, got %v", sf, *s.offsetSeekTarget)
		}
	}
}

func TestUpstreamSubscriber_ParseOffsetSeek_Numeric(t *testing.T) {
	s := &UpstreamSubscriber{cfg: UpstreamSubscriberConfig{StartFrom: "offset:42"}}
	if err := s.parseOffsetSeek(); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if s.offsetSeekTarget == nil || *s.offsetSeekTarget != 42 {
		t.Errorf("offsetSeekTarget = %v, want 42", s.offsetSeekTarget)
	}
}

func TestUpstreamSubscriber_JoinFieldFor_DirectEmbed(t *testing.T) {
	s := &UpstreamSubscriber{cfg: UpstreamSubscriberConfig{Collection: "users"}}
	v := View("orders").Root("orders").
		Embed("buyer", FromMongo("users").On("buyer_id")).
		Version(1)
	if got := s.joinFieldFor(v); got != "buyer_id" {
		t.Errorf("joinFieldFor = %q, want buyer_id", got)
	}
}

func TestUpstreamSubscriber_JoinFieldFor_NonMatchingCollectionReturnsEmpty(t *testing.T) {
	s := &UpstreamSubscriber{cfg: UpstreamSubscriberConfig{Collection: "products"}}
	v := View("orders").Root("orders").
		Embed("buyer", FromMongo("users").On("buyer_id")).
		Version(1)
	if got := s.joinFieldFor(v); got != "" {
		t.Errorf("non-matching collection should return empty, got %q", got)
	}
}

func TestUpstreamSubscriber_FindMongoJoinField_NestedEmbed(t *testing.T) {
	// Build an outer source whose embed targets an upstream Mongo
	// collection (rare but legal — nested composition).
	outer := From("level1").EmbedMany("inner", FromMongo("upstream_x").On("level1_id"))
	embeds := []embedDef{{field: "level0", source: outer, many: false}}
	if got := findMongoJoinField(embeds, "upstream_x"); got != "level1_id" {
		t.Errorf("expected nested Mongo join field, got %q", got)
	}
	if got := findMongoJoinField(embeds, "unrelated"); got != "" {
		t.Errorf("expected empty for unrelated collection, got %q", got)
	}
}

func TestUpstreamMetrics_IncrementsAndSnapshots(t *testing.T) {
	m := newUpstreamMetrics()
	m.inc("t1", "v1", "compose")
	m.inc("t1", "v1", "compose")
	m.inc("t1", "v2", "upsert")
	snap := m.Snapshot()
	if snap["t1|v1|compose"] != 2 {
		t.Errorf("compose counter = %d", snap["t1|v1|compose"])
	}
	if snap["t1|v2|upsert"] != 1 {
		t.Errorf("upsert counter = %d", snap["t1|v2|upsert"])
	}
}

func TestUpstreamMetrics_NilSafe(t *testing.T) {
	var m *upstreamMetrics // nil
	m.inc("a", "b", "c")   // should not panic
	if got := m.Snapshot(); got != nil {
		t.Errorf("nil Snapshot should be nil, got %v", got)
	}
}

func TestUpstreamSubscriberConfig_NewUpstreamSubscriber_RequiresTopicAndCollection(t *testing.T) {
	if _, err := NewUpstreamSubscriber(nil, nil, nil,
		UpstreamSubscriberConfig{Collection: "users"}, nil, nil, nil); err == nil {
		t.Error("expected error on missing Topic")
	}
	if _, err := NewUpstreamSubscriber(nil, nil, nil,
		UpstreamSubscriberConfig{Topic: "users.events"}, nil, nil, nil); err == nil {
		t.Error("expected error on missing Collection")
	}
}

func TestUpstreamSubscriberConfig_NewUpstreamSubscriber_ParsesOffset(t *testing.T) {
	s, err := NewUpstreamSubscriber(nil, nil, nil,
		UpstreamSubscriberConfig{
			Topic:      "users.events",
			Collection: "users",
			StartFrom:  "offset:7",
		}, nil, nil, nil)
	if err != nil {
		t.Fatalf("constructor: %v", err)
	}
	if s.offsetSeekTarget == nil || *s.offsetSeekTarget != 7 {
		t.Errorf("offsetSeekTarget not parsed; got %v", s.offsetSeekTarget)
	}
}

// Sanity: bson.M is map[string]any, so applyFilter preserves value types
// like nested maps verbatim (deep copy is not the contract).
func TestUpstreamSubscriber_ApplyFilter_PreservesValueShape(t *testing.T) {
	s := &UpstreamSubscriber{cfg: UpstreamSubscriberConfig{Filter: []string{"meta"}}}
	nested := map[string]any{"tier": "gold"}
	in := bson.M{"meta": nested, "name": "x"}
	got := s.applyFilter(in)
	want := bson.M{"meta": nested}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}
