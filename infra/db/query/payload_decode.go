package query

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strings"
	"time"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// The read-side half of the v2 outbox contract: decodePayloadEvent turns the
// raw payload into TYPED projection input — the same value shapes the
// composer's relational read produces — using the schema's PayloadColumnTypes
// as the restoration map. JSON alone would flatten every number to float64,
// every timestamp to a string and every []byte to base64; decoding against
// the declared Go types is what makes the payload-direct document
// byte-equivalent to the re-read one (numbers ride json.Number so int64
// precision survives).
//
// A payload without the "_ids" block is NOT a v2 event (pre-v2 backlog, the
// replay admin's synthetic rows, a foreign producer) — the caller logs a
// warning and SKIPS the event (maintainer decision: no fallback reprocessing;
// the post-upgrade rebuild converges the backlog).

// childOp is one surgically-applicable child operation carried by the payload.
type childOp struct {
	Op     string   // insert | update | archive | delete | noop
	Fields Document // typed via the child schema; carries the child PK column
}

// decodedEvent is the typed projection input of one v2 event.
type decodedEvent struct {
	IDs          payloadIDs
	Scalars      Document             // typed, column-keyed, reserved keys stripped
	Children     map[string][]childOp // by child Go type name
	BaseChildren map[string][]childOp
}

// decodeRawPayload parses the payload bytes ONCE into the raw JSON map
// (numbers as json.Number). ok=false when the payload is empty, malformed or
// not a v2 event (no "_ids" block). The map is the shared input of every
// per-view coercion of one event: SyncEngine.process parses here once and runs
// coercePayloadEvent per view over it — the typed half is schema-dependent
// (PayloadColumnTypes differ per view), the parse is not.
func decodeRawPayload(payload []byte) (map[string]any, bool) {
	if len(payload) == 0 {
		return nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	var raw map[string]any
	if err := dec.Decode(&raw); err != nil {
		return nil, false
	}
	if _, ok := raw["_ids"].(map[string]any); !ok {
		return nil, false
	}
	return raw, true
}

// rawPayloadIDs reads the structural identity from a decodeRawPayload result
// (which guaranteed the "_ids" block exists).
func rawPayloadIDs(raw map[string]any) payloadIDs {
	ids, _ := raw["_ids"].(map[string]any)
	return decodeIDsBlock(ids)
}

// coercePayloadEvent restores one already-parsed raw payload into the TYPED
// projection input of one view. It reads the shared raw map and NEVER mutates
// it — every produced Document/childOp is freshly built — so any number of
// views coerce over the same parse.
func coercePayloadEvent(schema *core.TableSchema, raw map[string]any) *decodedEvent {
	ev := &decodedEvent{
		IDs:     rawPayloadIDs(raw),
		Scalars: Document{},
	}
	types := schema.PayloadColumnTypes()
	for k, v := range raw {
		if strings.HasPrefix(k, "_") {
			continue
		}
		ev.Scalars[k] = coercePayloadValue(v, types[k])
	}
	ev.Children = decodeChildGroups(schema, raw["_children"])
	ev.BaseChildren = decodeChildGroups(schema, raw["_base_children"])
	return ev
}

// decodePayloadEvent decodes payload against the view's root schema — parse +
// coerce in one call, for callers holding a single view. ok=false when the
// payload is not a v2 event (no _ids) or is malformed. Multi-view callers
// parse once (decodeRawPayload) and coerce per view instead.
func decodePayloadEvent(schema *core.TableSchema, payload []byte) (*decodedEvent, bool) {
	raw, ok := decodeRawPayload(payload)
	if !ok {
		return nil, false
	}
	return coercePayloadEvent(schema, raw), true
}

// decodeIDsBlock reads the _ids map (json.Number-safe — parsePayloadIDs stays
// for the routing-only fast path, this one feeds the projector).
func decodeIDsBlock(m map[string]any) payloadIDs {
	ids := payloadIDs{}
	if s, ok := m["id"].(string); ok {
		ids.ID = s
	}
	if s, ok := m["base_id"].(string); ok {
		ids.BaseID = s
	}
	if n, ok := m["revision"].(json.Number); ok {
		if v, err := n.Int64(); err == nil {
			ids.Revision = v
		}
	}
	if n, ok := m["base_revision"].(json.Number); ok {
		if v, err := n.Int64(); err == nil {
			ids.BaseRevision = v
		}
	}
	if b, ok := m["base_purged"].(bool); ok {
		ids.BasePurged = b
	}
	return ids
}

// decodeChildGroups decodes one _children/_base_children block: each group's
// items typed via the CHILD's schema (resolved by Go type name on the root or
// its shared base). Unknown groups are dropped — the view cannot project a
// child it does not declare.
func decodeChildGroups(schema *core.TableSchema, raw any) map[string][]childOp {
	groups, ok := raw.(map[string]any)
	if !ok || len(groups) == 0 {
		return nil
	}
	out := make(map[string][]childOp, len(groups))
	for typeName, itemsRaw := range groups {
		child, _, ok := schema.ResolveAggregateChild(typeName)
		if !ok {
			continue
		}
		items, ok := itemsRaw.([]any)
		if !ok {
			continue
		}
		types := child.PayloadColumnTypes()
		ops := make([]childOp, 0, len(items))
		for _, it := range items {
			m, ok := it.(map[string]any)
			if !ok {
				continue
			}
			op := childOp{Fields: Document{}}
			for k, v := range m {
				if k == "_op" {
					op.Op, _ = v.(string)
					continue
				}
				if strings.HasPrefix(k, "_") {
					continue
				}
				op.Fields[k] = coercePayloadValue(v, types[k])
			}
			if op.Op == "" {
				op.Op = "noop"
			}
			ops = append(ops, op)
		}
		if len(ops) > 0 {
			out[typeName] = ops
		}
	}
	return out
}

// coercePayloadValue restores one JSON value to the Go-native shape the
// composer produces for a column of the given declared type. A nil target
// type (a column the schema does not know) passes the value through with
// json.Number normalized (int64 when integral, float64 otherwise) so an
// unknown-but-numeric value never lands as a string.
func coercePayloadValue(v any, t reflect.Type) any {
	if v == nil {
		return nil
	}
	if t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch {
	case t == nil:
		if n, ok := v.(json.Number); ok {
			return normalizeNumber(n)
		}
		return v
	case t == timeType:
		if s, ok := v.(string); ok {
			if ts, err := time.Parse(time.RFC3339Nano, s); err == nil {
				return ts.UTC()
			}
		}
		return v
	case t.Kind() == reflect.String || t == idType:
		return v // canonical string on the wire and in the document alike
	case t.Kind() == reflect.Bool:
		return v
	case t.Kind() >= reflect.Int && t.Kind() <= reflect.Int64:
		if n, ok := v.(json.Number); ok {
			if i, err := n.Int64(); err == nil {
				return i
			}
		}
		return v
	case t.Kind() == reflect.Float32 || t.Kind() == reflect.Float64:
		if n, ok := v.(json.Number); ok {
			if f, err := n.Float64(); err == nil {
				return f
			}
		}
		return v
	case t == rawMessageType:
		// json.RawMessage travels as inline JSON — re-render the fragment.
		b, err := json.Marshal(v)
		if err != nil {
			return v
		}
		return b
	case t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8:
		// []byte travels base64-encoded (encoding/json's []byte convention).
		if s, ok := v.(string); ok {
			if b, err := base64.StdEncoding.DecodeString(s); err == nil {
				return b
			}
		}
		return v
	default:
		return v
	}
}

// normalizeNumber renders a schema-unknown json.Number in the least surprising
// shape: int64 when integral, float64 otherwise.
func normalizeNumber(n json.Number) any {
	if !strings.ContainsAny(n.String(), ".eE") {
		if i, err := n.Int64(); err == nil {
			return i
		}
	}
	if f, err := n.Float64(); err == nil {
		return f
	}
	return n.String()
}

var (
	timeType       = reflect.TypeOf(time.Time{})
	rawMessageType = reflect.TypeOf(json.RawMessage(nil))
	idType         = reflect.TypeOf(domain.ID{})
)
