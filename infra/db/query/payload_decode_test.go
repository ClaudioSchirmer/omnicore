package query

import (
	"testing"
	"time"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// The typed decode restores the exact value shapes the composer's relational
// read produces — the equivalence foundation of the payload-direct projection.

type pdRoot struct {
	ID     string
	Name   string
	Age    int64
	Active bool
	Nick   *string
	Photo  []byte
}

type pdChild struct {
	ID    string
	Label string
	Rank  int64
}

func pdSchema() *core.TableSchema {
	child := core.NewTableSchema[*pdChild]("pd_children").PK("id").FK("root_id").
		Field("Label", "label").Field("Rank", "rank")
	return core.NewTableSchema[*pdRoot]("pd_roots").PK("id").
		Field("Name", "name").Field("Age", "age").Field("Active", "active").
		Field("Nick", "nick").Field("Photo", "photo").
		SoftDelete("deleted_at").CreatedAt("created_at").UpdatedAt("updated_at").
		Child(child)
}

func TestDecodePayloadEvent_TypedRestoration(t *testing.T) {
	payload := []byte(`{
		"name": "Ana",
		"age": 9007199254740993,
		"active": true,
		"nick": null,
		"photo": "aGk=",
		"created_at": "2026-07-20T12:00:00.000001Z",
		"_ids": {"id": "r1", "base_id": "b1", "base_revision": 42},
		"_children": {"pdChild": [
			{"_op": "insert", "id": "c1", "label": "x", "rank": 5},
			{"_op": "archive", "id": "c2"}
		]}
	}`)
	ev, ok := decodePayloadEvent(pdSchema(), payload)
	if !ok {
		t.Fatal("a v2 payload must decode")
	}
	if ev.IDs.ID != "r1" || ev.IDs.BaseID != "b1" || ev.IDs.BaseRevision != 42 {
		t.Fatalf("_ids = %+v", ev.IDs)
	}
	// int64 precision above 2^53 survives (json.Number, never float64).
	if got, ok := ev.Scalars["age"].(int64); !ok || got != 9007199254740993 {
		t.Errorf("age = %v (%T), want int64 9007199254740993", ev.Scalars["age"], ev.Scalars["age"])
	}
	if got, ok := ev.Scalars["created_at"].(time.Time); !ok || !got.Equal(time.Date(2026, 7, 20, 12, 0, 0, 1000, time.UTC)) {
		t.Errorf("created_at = %v (%T), want the parsed UTC time.Time", ev.Scalars["created_at"], ev.Scalars["created_at"])
	}
	if got, ok := ev.Scalars["photo"].([]byte); !ok || string(got) != "hi" {
		t.Errorf("photo = %v (%T), want the base64-decoded []byte", ev.Scalars["photo"], ev.Scalars["photo"])
	}
	if v, has := ev.Scalars["nick"]; !has || v != nil {
		t.Errorf("nick must decode as explicit nil, got %v", ev.Scalars)
	}
	if _, has := ev.Scalars["_ids"]; has {
		t.Error("reserved keys must not leak into the scalars")
	}
	ops := ev.Children["pdChild"]
	if len(ops) != 2 || ops[0].Op != "insert" || ops[1].Op != "archive" {
		t.Fatalf("children ops = %+v", ops)
	}
	if got, ok := ops[0].Fields["rank"].(int64); !ok || got != 5 {
		t.Errorf("child rank = %v (%T), want int64 5", ops[0].Fields["rank"], ops[0].Fields["rank"])
	}
	if ops[1].Fields["id"] != "c2" {
		t.Errorf("archive op must carry the child PK, got %v", ops[1].Fields)
	}
}

func TestDecodePayloadEvent_NonV2Rejected(t *testing.T) {
	if _, ok := decodePayloadEvent(pdSchema(), nil); ok {
		t.Error("empty payload is not v2")
	}
	if _, ok := decodePayloadEvent(pdSchema(), []byte(`{"name":"Ana"}`)); ok {
		t.Error("a payload without _ids is not v2 — WARNING + skip at the caller")
	}
	if _, ok := decodePayloadEvent(pdSchema(), []byte(`garbage`)); ok {
		t.Error("malformed payload is not v2")
	}
}

func TestDecodePayloadEvent_UnknownChildGroupDropped(t *testing.T) {
	ev, ok := decodePayloadEvent(pdSchema(), []byte(`{"_ids":{"id":"r1"},"_children":{"Ghost":[{"_op":"insert","id":"g1"}]}}`))
	if !ok {
		t.Fatal("decode failed")
	}
	if len(ev.Children) != 0 {
		t.Errorf("an undeclared child group must be dropped, got %v", ev.Children)
	}
}
