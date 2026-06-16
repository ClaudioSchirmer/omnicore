package audit

import (
	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/translation"
)

// RenderLabels walks ev in place and replaces every FieldLabelKey on its
// changes (top-level + per-child) with the rendered translation in the
// supplied locale. After the call:
//
//   - FieldChange.FieldLabelKey is cleared (omitempty elides it from any
//     subsequent JSON serialization).
//   - FieldChange.FieldLabel carries the rendered string (or the raw key
//     on catalog miss — symmetric with Translator.Render's existing
//     fallback for MessageDTO.Message).
//
// Snapshot blocks (kind=snapshot on Insert / Delete) are NOT touched:
// snapshot is `map[col]value` with no schema for labels, and adding labels
// there would require a parallel format. RenderLabels stays scoped to the
// shape audit reads today.
//
// Children traversal walks ev.Children and applies the same rule to each
// ChildEvent.Changes — keyed by Go typeName so the helper does not need to
// know the consumer's child struct names.
//
// Catalog miss: Translator.Render returns the raw key and emits the
// existing `slog.Warn("translation.key.missing", "lang", lang, "key", key)`
// dedup'd by (lang, key). FieldLabel ends up carrying the raw key — same
// observable result the notification wire produces under the same drift.
//
// Safe on a nil ev (no-op). Safe on a nil Translator (no-op). The function
// is the canonical single primitive both BI / SQL consumers and in-process
// Go readers go through; the JSON variant is RenderLabelsInJSON below.
func RenderLabels(ev *AuditEvent, t *translation.Translator, lang configuration.Language) {
	if ev == nil || t == nil {
		return
	}
	renderChanges(ev.Changes, t, lang)
	for _, children := range ev.Children {
		for i := range children {
			renderChanges(children[i].Changes, t, lang)
		}
	}
}

// renderChanges walks a slice of FieldChange in place. Both call sites
// (root + per-child) share this helper so the rule is in one place.
func renderChanges(changes []FieldChange, t *translation.Translator, lang configuration.Language) {
	for i := range changes {
		key := changes[i].FieldLabelKey
		if key == "" {
			continue
		}
		changes[i].FieldLabel = t.Render(lang, key, nil)
		changes[i].FieldLabelKey = ""
	}
}

// RenderLabelsInJSON is the JSON-form sibling of RenderLabels. It walks the
// parsed audit document in place, descending through the same audit shape
// (top-level "changes" + "children".<type>[].changes), and replaces every
// "fieldLabelKey" entry with a "fieldLabel" entry carrying the rendered
// string. The original "fieldLabelKey" entry is deleted from the map so the
// post-render shape matches the canonical wire shape (the same one the
// notification envelope already publishes).
//
// Intended for BI tools / SQL consumers that read the audit_events jsonb
// column as a generic map without first deserializing into the typed
// AuditEvent. Same catalog-miss semantics as RenderLabels: Translator.Render
// is consulted; raw key reaches "fieldLabel" on miss, slog.Warn fires once.
//
// Safe on nil doc / nil Translator (no-op). Children typeNames are read off
// the dynamic map keys so the function knows no domain vocabulary.
func RenderLabelsInJSON(doc map[string]any, t *translation.Translator, lang configuration.Language) {
	if doc == nil || t == nil {
		return
	}
	if raw, ok := doc["changes"]; ok {
		renderChangesInJSON(raw, t, lang)
	}
	children, ok := doc["children"].(map[string]any)
	if !ok {
		return
	}
	for _, entries := range children {
		entriesList, ok := entries.([]any)
		if !ok {
			continue
		}
		for _, entry := range entriesList {
			child, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			if raw, ok := child["changes"]; ok {
				renderChangesInJSON(raw, t, lang)
			}
		}
	}
}

// renderChangesInJSON walks a JSON `changes` array (`[]any` of
// `map[string]any`) in place. Mirrors renderChanges on the typed path.
func renderChangesInJSON(raw any, t *translation.Translator, lang configuration.Language) {
	list, ok := raw.([]any)
	if !ok {
		return
	}
	for _, entry := range list {
		ch, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		key, ok := ch["fieldLabelKey"].(string)
		if !ok || key == "" {
			continue
		}
		ch["fieldLabel"] = t.Render(lang, key, nil)
		delete(ch, "fieldLabelKey")
	}
}
