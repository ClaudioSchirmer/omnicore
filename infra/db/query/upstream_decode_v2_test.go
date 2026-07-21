package query

import "testing"

// decodePayload tolerates three producer generations: the Debezium "payload"
// envelope, the legacy aggregate {"root","children"} shape, and the v2 flat
// shape whose "_" reserved keys are stripped from the mirror unless the
// consumer's Filter allowlists them.

func decodeSub(filter ...string) *UpstreamSubscriber {
	return &UpstreamSubscriber{cfg: UpstreamSubscriberConfig{Collection: "c", Topic: "t", Filter: filter}}
}

func TestDecodePayload_LegacyRootShapeMergesFlat(t *testing.T) {
	doc, err := decodeSub().decodePayload([]byte(`{"root":{"name":"Ana","email":"a@x"},"children":{"Addr":[{"street":"s"}]}}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if doc["name"] != "Ana" || doc["email"] != "a@x" {
		t.Errorf("legacy root fields must merge flat to the top, got %v", doc)
	}
	if _, has := doc["root"]; has {
		t.Errorf("the root envelope must not survive, got %v", doc)
	}
	if _, has := doc["children"]; has {
		t.Errorf("the legacy children block is informational and dropped, got %v", doc)
	}
}

func TestDecodePayload_V2ReservedKeysStripped(t *testing.T) {
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

// resolveBaseID answers payload-first: the v2 _ids.base_id wins over every
// legacy branch and touches no database.
func TestResolveBaseID_PayloadFirst(t *testing.T) {
	role := fanOutRoleSchema()
	s := &SyncEngine{}
	ev := kafkaEvent{
		AggregateID: "r1",
		EventType:   "UPDATED",
		Payload:     []byte(`{"_ids":{"id":"r1","base_id":"b1","base_revision":2}}`),
	}
	got, err := s.resolveBaseID(nil, ev, roleDef{schema: role})
	if err != nil || got != "b1" {
		t.Fatalf("resolveBaseID = %q, %v — want the payload's base id with no DB access", got, err)
	}
}
