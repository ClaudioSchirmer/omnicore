package audit

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/translation"
)

type stubModule struct {
	lang  configuration.Language
	table map[string]string
}

func (m stubModule) Language() configuration.Language { return m.lang }
func (m stubModule) Translations() map[string]string  { return m.table }

func newRenderTranslator(modules ...translation.Module) *translation.Translator {
	t := translation.New()
	t.Import(modules...)
	return t
}

func basePTBR() *translation.Translator {
	return newRenderTranslator(stubModule{
		lang: configuration.LangPTBR,
		table: map[string]string{
			"UserNameField":       "Nome",
			"AddressZipCodeField": "CEP",
		},
	})
}

// ─── Typed: RenderLabels ──────────────────────────────────────────────────

func TestRenderLabels_PopulatesFieldLabelClearsKey(t *testing.T) {
	ev := &AuditEvent{
		Changes: []FieldChange{
			{Field: "name", FieldLabelKey: "UserNameField", From: "Jane", To: "Janet"},
		},
	}
	RenderLabels(ev, basePTBR(), configuration.LangPTBR)
	got := ev.Changes[0]
	if got.FieldLabel != "Nome" {
		t.Errorf("FieldLabel = %q, want Nome", got.FieldLabel)
	}
	if got.FieldLabelKey != "" {
		t.Errorf("FieldLabelKey = %q, want empty (renderer must clear)", got.FieldLabelKey)
	}
}

func TestRenderLabels_LeavesUntaggedFieldChangeAlone(t *testing.T) {
	ev := &AuditEvent{
		Changes: []FieldChange{
			{Field: "email", From: "a@x", To: "b@x"}, // no LabelKey
		},
	}
	RenderLabels(ev, basePTBR(), configuration.LangPTBR)
	if ev.Changes[0].FieldLabel != "" {
		t.Errorf("untagged change must not produce FieldLabel, got %q", ev.Changes[0].FieldLabel)
	}
}

func TestRenderLabels_CatalogMissFallsBackToKey(t *testing.T) {
	ev := &AuditEvent{
		Changes: []FieldChange{
			{Field: "name", FieldLabelKey: "MissingKey"},
		},
	}
	RenderLabels(ev, basePTBR(), configuration.LangPTBR)
	if ev.Changes[0].FieldLabel != "MissingKey" {
		t.Errorf("miss FieldLabel = %q, want raw key fallback", ev.Changes[0].FieldLabel)
	}
	if ev.Changes[0].FieldLabelKey != "" {
		t.Errorf("FieldLabelKey must be cleared on miss too, got %q", ev.Changes[0].FieldLabelKey)
	}
}

func TestRenderLabels_DescendsIntoChildren(t *testing.T) {
	ev := &AuditEvent{
		Children: map[string][]ChildEvent{
			"Address": {
				{
					ID: "addr-1", Op: "updated",
					Changes: []FieldChange{
						{Field: "zip_code", FieldLabelKey: "AddressZipCodeField", From: "1", To: "2"},
					},
				},
			},
		},
	}
	RenderLabels(ev, basePTBR(), configuration.LangPTBR)
	fc := ev.Children["Address"][0].Changes[0]
	if fc.FieldLabel != "CEP" {
		t.Errorf("child FieldLabel = %q, want CEP", fc.FieldLabel)
	}
	if fc.FieldLabelKey != "" {
		t.Errorf("child FieldLabelKey not cleared: %q", fc.FieldLabelKey)
	}
}

func TestRenderLabels_NilSafe(t *testing.T) {
	// Must not panic on any of these inputs.
	RenderLabels(nil, basePTBR(), configuration.LangPTBR)
	ev := &AuditEvent{Changes: []FieldChange{{Field: "x", FieldLabelKey: "X"}}}
	RenderLabels(ev, nil, configuration.LangPTBR)
	if ev.Changes[0].FieldLabel != "" {
		t.Error("nil translator must leave FieldLabel empty")
	}
}

func TestRenderLabels_PostRenderJSONShapeMatchesNotificationWire(t *testing.T) {
	// Wire goal: the post-render JSON does NOT carry fieldLabelKey AND
	// carries fieldLabel. This matches the shape the notification envelope
	// already publishes — uniform read across both surfaces.
	ev := &AuditEvent{
		Changes: []FieldChange{
			{Field: "name", FieldLabelKey: "UserNameField", From: "a", To: "b"},
		},
	}
	RenderLabels(ev, basePTBR(), configuration.LangPTBR)
	raw, err := json.Marshal(ev.Changes[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back map[string]any
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, present := back["fieldLabelKey"]; present {
		t.Errorf("post-render JSON must not carry fieldLabelKey (omitempty), got %v", back)
	}
	if back["fieldLabel"] != "Nome" {
		t.Errorf("post-render fieldLabel = %v, want Nome", back["fieldLabel"])
	}
}

// ─── JSON: RenderLabelsInJSON ──────────────────────────────────────────────

func TestRenderLabelsInJSON_ReplacesKeyWithLabelInPlace(t *testing.T) {
	doc := map[string]any{
		"changes": []any{
			map[string]any{
				"field":         "name",
				"fieldLabelKey": "UserNameField",
				"from":          "a",
				"to":            "b",
			},
		},
	}
	RenderLabelsInJSON(doc, basePTBR(), configuration.LangPTBR)
	ch := doc["changes"].([]any)[0].(map[string]any)
	if _, has := ch["fieldLabelKey"]; has {
		t.Errorf("fieldLabelKey must be deleted, got: %v", ch)
	}
	if ch["fieldLabel"] != "Nome" {
		t.Errorf("fieldLabel = %v, want Nome", ch["fieldLabel"])
	}
}

func TestRenderLabelsInJSON_DescendsIntoChildren(t *testing.T) {
	doc := map[string]any{
		"changes": []any{},
		"children": map[string]any{
			"Address": []any{
				map[string]any{
					"id": "addr-1", "op": "updated",
					"changes": []any{
						map[string]any{
							"field":         "zip_code",
							"fieldLabelKey": "AddressZipCodeField",
							"from":          "1", "to": "2",
						},
					},
				},
			},
		},
	}
	RenderLabelsInJSON(doc, basePTBR(), configuration.LangPTBR)
	child := doc["children"].(map[string]any)["Address"].([]any)[0].(map[string]any)
	fc := child["changes"].([]any)[0].(map[string]any)
	if fc["fieldLabel"] != "CEP" {
		t.Errorf("child fieldLabel = %v, want CEP", fc["fieldLabel"])
	}
	if _, has := fc["fieldLabelKey"]; has {
		t.Errorf("child fieldLabelKey must be deleted, got: %v", fc)
	}
}

func TestRenderLabelsInJSON_LeavesUntaggedEntriesAlone(t *testing.T) {
	doc := map[string]any{
		"changes": []any{
			map[string]any{"field": "email", "from": "a@x", "to": "b@x"}, // no key
		},
	}
	want := deepCopyMap(doc)
	RenderLabelsInJSON(doc, basePTBR(), configuration.LangPTBR)
	if !reflect.DeepEqual(doc, want) {
		t.Errorf("untagged doc must be unchanged.\n got=%v\nwant=%v", doc, want)
	}
}

func TestRenderLabelsInJSON_CatalogMissFallsBackToKey(t *testing.T) {
	doc := map[string]any{
		"changes": []any{
			map[string]any{"field": "x", "fieldLabelKey": "UnknownKey"},
		},
	}
	RenderLabelsInJSON(doc, basePTBR(), configuration.LangPTBR)
	ch := doc["changes"].([]any)[0].(map[string]any)
	if ch["fieldLabel"] != "UnknownKey" {
		t.Errorf("miss fieldLabel = %v, want raw key fallback", ch["fieldLabel"])
	}
	if _, has := ch["fieldLabelKey"]; has {
		t.Errorf("miss must still delete fieldLabelKey, got: %v", ch)
	}
}

func TestRenderLabelsInJSON_NilSafe(t *testing.T) {
	RenderLabelsInJSON(nil, basePTBR(), configuration.LangPTBR)
	doc := map[string]any{"changes": []any{}}
	RenderLabelsInJSON(doc, nil, configuration.LangPTBR)
}

func TestRenderLabelsInJSON_HandlesMalformedShapes(t *testing.T) {
	// Defensive — must not panic when a real-world parsed doc carries
	// unexpected types (e.g. children as a wrong shape, changes not array).
	doc := map[string]any{
		"changes":  "not-an-array",
		"children": "not-a-map",
	}
	RenderLabelsInJSON(doc, basePTBR(), configuration.LangPTBR)
}

func TestRenderLabelsInJSON_RoundTripWithMarshalEqualsTypedRender(t *testing.T) {
	// Equivalence: rendering through the typed path and rendering through
	// the JSON path must produce identical JSON output. The two functions
	// are siblings of a single concept; drift between them is a bug.
	ev := &AuditEvent{
		Verb: "update", Kind: "delta",
		Changes: []FieldChange{
			{Field: "name", FieldLabelKey: "UserNameField", From: "a", To: "b"},
		},
		Children: map[string][]ChildEvent{
			"Address": {
				{
					ID: "addr-1", Op: "updated",
					Changes: []FieldChange{
						{Field: "zip_code", FieldLabelKey: "AddressZipCodeField", From: "1", To: "2"},
					},
				},
			},
		},
	}
	// Typed path.
	evTyped := cloneEvent(ev)
	RenderLabels(evTyped, basePTBR(), configuration.LangPTBR)
	typedJSON, _ := json.Marshal(evTyped)

	// JSON path: marshal the pristine event, parse, render in place.
	pristineJSON, _ := json.Marshal(ev)
	var jsonDoc map[string]any
	if err := json.Unmarshal(pristineJSON, &jsonDoc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	RenderLabelsInJSON(jsonDoc, basePTBR(), configuration.LangPTBR)
	jsonOut, _ := json.Marshal(jsonDoc)

	// Re-marshal both via map → string for stable comparison (json maps
	// emit sorted keys, so this normalizes both sides).
	var lhs, rhs map[string]any
	_ = json.Unmarshal(typedJSON, &lhs)
	_ = json.Unmarshal(jsonOut, &rhs)
	if !reflect.DeepEqual(lhs, rhs) {
		t.Errorf("typed and JSON paths produced different output.\n typed=%s\n json=%s", typedJSON, jsonOut)
	}
}

func deepCopyMap(m map[string]any) map[string]any {
	raw, _ := json.Marshal(m)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}

func cloneEvent(ev *AuditEvent) *AuditEvent {
	raw, _ := json.Marshal(ev)
	var out AuditEvent
	_ = json.Unmarshal(raw, &out)
	return &out
}
