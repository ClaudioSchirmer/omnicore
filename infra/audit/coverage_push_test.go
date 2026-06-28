package audit

import (
	"context"
	"testing"
	"time"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/google/uuid"
)

// TestInsertAuditEvent_PayloadMarshalError drives the buildAuditPayload error
// branch: a Snapshot carrying an unmarshalable value (a channel) makes
// json.Marshal fail before any Exec is attempted.
func TestInsertAuditEvent_PayloadMarshalError(t *testing.T) {
	tx := &fakeTx{}
	ev := sampleEvent()
	ev.Snapshot = map[string]any{"bad": make(chan int)}
	err := InsertAuditEvent(context.Background(), tx, pgPlaceholder, ev)
	if err == nil {
		t.Fatal("expected marshal error")
	}
	if tx.calls != 0 {
		t.Fatalf("Exec must not run when payload marshal fails, got %d calls", tx.calls)
	}
}

// TestEchoSlog_AllOptionalBlocksEmitted populates ActorIssuer, ActorClaims,
// Snapshot and Changes so every omitempty branch in EchoSlog runs.
func TestEchoSlog_AllOptionalBlocksEmitted(t *testing.T) {
	logger, buf := newCaptureLogger()
	EchoSlog(nil, logger, AuditEvent{
		ThreadID:    uuid.NewString(),
		EntityType:  "User",
		EntityID:    uuid.NewString(),
		Verb:        "update",
		ActionName:  "GetUpdatable",
		Kind:        "delta",
		Actor:       "user-7",
		ActorIssuer: "https://idp.example",
		ActorClaims: map[string]any{"roles": []string{"admin"}},
		DateTime:    time.Now().UTC(),
		Snapshot:    map[string]any{"name": "alice"},
		Changes:     []FieldChange{{Field: "name", From: "a", To: "b"}},
	})
	entry := extractAuditLogLine(t, buf)
	for _, k := range []string{"actorIssuer", "actorClaims", "snapshot", "changes"} {
		if _, ok := entry[k]; !ok {
			t.Errorf("expected %q on the audit slog line", k)
		}
	}
}

// TestRenderLabelsInJSON_DeepMalformedShapes drives the type-assertion
// "else" branches not covered elsewhere: a children entry not a list, a
// list element not a map, a changes value not a list, a changes element not
// a map, and a changes element with a missing/empty fieldLabelKey.
func TestRenderLabelsInJSON_DeepMalformedShapes(t *testing.T) {
	// children entries not a list, then a list element that is not a map.
	RenderLabelsInJSON(map[string]any{
		"children": map[string]any{
			"NotAList": "scalar",
			"Address":  []any{"not-a-map"},
		},
	}, basePTBR(), configuration.LangPTBR)

	// child map with a changes block holding a missing-key and a populated key.
	doc := map[string]any{
		"children": map[string]any{
			"Address": []any{
				map[string]any{
					"changes": []any{
						map[string]any{"field": "street"},                      // no fieldLabelKey
						map[string]any{"fieldLabelKey": ""},                    // empty key
						map[string]any{"fieldLabelKey": "AddressZipCodeField"}, // rendered
						"not-a-map", // skipped
					},
				},
				map[string]any{"changes": 42}, // changes not a list → ignored
			},
		},
	}
	RenderLabelsInJSON(doc, basePTBR(), configuration.LangPTBR)

	addr := doc["children"].(map[string]any)["Address"].([]any)
	changes := addr[0].(map[string]any)["changes"].([]any)
	rendered := changes[2].(map[string]any)
	if rendered["fieldLabel"] != "CEP" {
		t.Errorf("expected rendered fieldLabel=CEP, got %v", rendered["fieldLabel"])
	}
	if _, ok := rendered["fieldLabelKey"]; ok {
		t.Error("expected fieldLabelKey removed after render")
	}
}
