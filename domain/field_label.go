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
var labelPlanCache sync.Map // map[reflect.Type]map[string]fieldMeta

// fieldMeta is one field's declarative naming vocabulary, read off its struct
// tags in a single walk: the `labelKey` catalog key (the translatable business
// label) and the `notifyAs` wire token (the technical field name override).
// One plan serves BOTH so every seat that resolves by field name — the named
// emission seats, the pending backfill, audit — speaks the same vocabulary the
// field-reference seat reads from the StructField directly.
type fieldMeta struct {
	label string
	wire  string
}

// resolveLabelKey returns the catalog key declared on the `labelKey:"..."` struct
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
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return ""
	}
	return loadLabelPlan(t)[fieldName].label
}

// resolveNotifyAs returns the `notifyAs:"..."` wire token declared on t's
// field named fieldName, or "" when none is — the name-keyed twin of the
// atlas's tag read, honoring the same skip rules as resolveLabelKey. It is
// what keeps a field's wire name IDENTICAL whichever seat emits about it: a
// field-reference rule, the automatic value-object pass, an enum membership
// refusal or a named emission all render the same token.
func resolveNotifyAs(t reflect.Type, fieldName string) string {
	if t == nil || fieldName == "" {
		return ""
	}
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return ""
	}
	return loadLabelPlan(t)[fieldName].wire
}

// loadLabelPlan returns the cached or freshly-built (fieldName → labelKey) map
// for t. The plan is keyed by Go field name (PascalCase identifier) and stores
// only fields whose `label` tag survives the resolveLabelKey rules; fields
// without a label tag are absent from the map (lookup returns "" naturally).
func loadLabelPlan(t reflect.Type) map[string]fieldMeta {
	if cached, ok := labelPlanCache.Load(t); ok {
		return cached.(map[string]fieldMeta)
	}
	plan := buildLabelPlan(t)
	labelPlanCache.Store(t, plan)
	return plan
}

// buildLabelPlan walks t's exported fields once, recording each field whose
// `label` tag is present, non-empty, and non-"-". Anonymous embedded structs flatten into the same plan so
// promoted-field lookups (e.g. via BaseEntity embed) reach their label tag
// through the same Go identifier the caller used on the outer type.
//
// A COMPOSITE value object field flattens the same way, by its PARTS' names: a
// composite carries several fields, its IsValid emits notifications on them
// (ctx.AddNotificationNamed("Street", …)), and the label of a part is declared on the
// value object's own field — the value object owns its vocabulary for every
// entity that uses it. Without this hop a part-level notification would ship
// with no label at all, since "Street" is not a field of the entity.
// cleanTag reads a tag honoring the shared skip rules: "" and "-" are both
// "no declaration".
func cleanTag(f reflect.StructField, key string) string {
	tag := f.Tag.Get(key)
	if tag == "-" {
		return ""
	}
	return tag
}

func buildLabelPlan(t reflect.Type) map[string]fieldMeta {
	out := map[string]fieldMeta{}
	// merge lets a DIRECT field shadow what an anonymous embed already put
	// under the same name — per declared half: a tag the outer type declares
	// wins; a half it leaves undeclared keeps the embed's value.
	merge := func(name string, meta fieldMeta) {
		cur := out[name]
		if meta.label != "" {
			cur.label = meta.label
		}
		if meta.wire != "" {
			cur.wire = meta.wire
		}
		out[name] = cur
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		if f.Anonymous && f.Type.Kind() == reflect.Struct {
			for name, meta := range buildLabelPlan(f.Type) {
				if _, exists := out[name]; !exists {
					out[name] = meta
				}
			}
			continue
		}
		merge(f.Name, fieldMeta{label: cleanTag(f, "labelKey"), wire: cleanTag(f, "notifyAs")})
		if ct, ok := compositeValueObjectType(f.Type); ok {
			// The field's own vocabulary (above) stays: a rule about the value
			// object as a whole emits on the entity's field name, with the
			// ENTITY's tags. The parts are added beside it — a part is not a
			// field of the entity, so the composite is the only type that can
			// name it (labelKey and notifyAs alike) — and never overwrite a
			// name the entity itself declares.
			for name, meta := range buildLabelPlan(ct) {
				if _, exists := out[name]; !exists {
					out[name] = meta
				}
			}
		}
	}
	return out
}

// compositeValueObjectType reports whether ft holds a COMPOSITE value object —
// a struct that owns a rule (IsValid) but yields no single scalar (no Value()),
// so its value spans several fields — and returns that type. The discriminator
// is the presence of Value(), the same one the persistence layer uses to tell a
// value object that occupies one column from one that decomposes.
func compositeValueObjectType(ft reflect.Type) (reflect.Type, bool) {
	t := ft
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil, false
	}
	zero := reflect.Zero(t).Interface()
	if !IsValueObject(zero) {
		return nil, false
	}
	if _, hasValue := ValueObjectValue(zero); hasValue {
		return nil, false
	}
	return t, true
}
