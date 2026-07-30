package query

import "testing"

// decodePayload turns a raw upstream outbox payload into the mirror document:
// a Debezium "payload" envelope is unwrapped, and framework "_"-reserved keys
// are stripped unless the consumer's Filter allowlists them.

func decodeSub(filter ...string) *UpstreamSubscriber {
	return &UpstreamSubscriber{cfg: UpstreamSubscriberConfig{Collection: "c", Topic: "t", Fields: filter}}
}

func TestDecodePayload_DebeziumEnvelopeUnwrapped(t *testing.T) {
	doc, err := decodeSub().decodePayload([]byte(`{"payload":{"name":"Ana"}}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if doc["name"] != "Ana" {
		t.Errorf("the Debezium payload envelope must be unwrapped, got %v", doc)
	}
}

// A non-omnicore upstream sends flat JSON with no framework keys — the mirror
// takes it verbatim (the join is by the externalView's JoinColumn, not _ids),
// so a foreign producer needs no _ids block.
func TestDecodePayload_ForeignFlatPayloadPassesThrough(t *testing.T) {
	doc, err := decodeSub().decodePayload([]byte(`{"name":"Ana","email":"a@x"}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if doc["name"] != "Ana" || doc["email"] != "a@x" {
		t.Errorf("a foreign flat payload must pass through, got %v", doc)
	}
}

func TestDecodePayload_ReservedKeysStripped(t *testing.T) {
	raw := []byte(`{"name":"Ana","_ids":{"id":"r1"},"_children":{"X":[]},"_base_children":{"Y":[]}}`)
	doc, err := decodeSub().decodePayload(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if doc["name"] != "Ana" {
		t.Errorf("business fields must survive, got %v", doc)
	}
	for _, k := range []string{"_ids", "_children", "_base_children"} {
		if _, has := doc[k]; has {
			t.Errorf("reserved key %q must be stripped from the mirror, got %v", k, doc)
		}
	}
}

func TestDecodePayload_FilterKeepsReservedKey(t *testing.T) {
	raw := []byte(`{"name":"Ana","_ids":{"id":"r1"}}`)
	doc, err := decodeSub("name", "_ids").decodePayload(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, has := doc["_ids"]; !has {
		t.Errorf("a Filter-allowlisted reserved key must survive, got %v", doc)
	}
}
