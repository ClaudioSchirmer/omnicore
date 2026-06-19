package domain

import (
	"reflect"
	"sync"
)

// labelPlanCache memoizes the per-type reflection plan so the second emission
// against a given entity type is a constant-time map lookup. Same shape as
// varPlanCache in notification_vars.go; the two caches stay independent because
// the tags they walk (`tvar` vs `label`) live on different struct kinds
// (Notification structs vs Entity / AggregateValueObject structs) and the lookup
// semantics differ (tvar walks all fields once; label looks up one field at a
// time by Go identifier).
var labelPlanCache sync.Map // map[reflect.Type]map[string]string

// resolveLabelKey returns the catalog key declared on the `label:"..."` struct
// tag of t's field named fieldName, or "" when no such tag exists.
//
// The function is the single primitive consumed by both Rules.AddNotification
// (where the emission of a notification needs the catalog key for the wire
// FieldLabel) and infra/audit_builder (where the diff walk needs the key for
// FieldChange.FieldLabelKey). Keeping the resolution here ensures both
// surfaces honor the same skip rules:
//
//   - Empty t, nil t, non-struct kind → "".
//   - Pointer t → unwrapped to the element type.
//   - Field absent on the type → "" (defensive; callers may pass typo'd names).
//   - Unexported field → "" (the framework never reads tags off unexported
//     state; AddNotification is called with exported Go identifiers in
//     practice).
//   - Tag value "-" → "" (mirror of `json:"-"` opt-out).
//   - Tag value "" → "" (empty declaration is a no-op).
//
// The plan is built once per (type, fieldName) tuple inside the type's plan
// map and cached so repeated emissions on the same field do not pay the
// reflection cost twice.
func resolveLabelKey(t reflect.Type, fieldName string) string {
	if t == nil || fieldName == "" {
		return ""
	}
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return ""
	}
	plan := loadLabelPlan(t)
	return plan[fieldName]
}

// loadLabelPlan returns the cached or freshly-built (fieldName → labelKey) map
// for t. The plan is keyed by Go field name (PascalCase identifier) and stores
// only fields whose `label` tag survives the resolveLabelKey rules; fields
// without a label tag are absent from the map (lookup returns "" naturally).
func loadLabelPlan(t reflect.Type) map[string]string {
	if cached, ok := labelPlanCache.Load(t); ok {
		return cached.(map[string]string)
	}
	plan := buildLabelPlan(t)
	labelPlanCache.Store(t, plan)
	return plan
}

// buildLabelPlan walks t's exported fields once, recording each field whose
// `label` tag is present, non-empty, and non-"-". Anonymous embedded structs flatten into the same plan so
// promoted-field lookups (e.g. via BaseEntity embed) reach their label tag
// through the same Go identifier the caller used on the outer type.
func buildLabelPlan(t reflect.Type) map[string]string {
	out := map[string]string{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		if f.Anonymous && f.Type.Kind() == reflect.Struct {
			for name, key := range buildLabelPlan(f.Type) {
				if _, exists := out[name]; !exists {
					out[name] = key
				}
			}
			continue
		}
		tag, ok := f.Tag.Lookup("label")
		if !ok || tag == "" || tag == "-" {
			continue
		}
		out[f.Name] = tag
	}
	return out
}
