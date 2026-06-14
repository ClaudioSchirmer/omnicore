package responses

import (
	"encoding/json"
	"reflect"
	"strings"
	"sync"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// AutoFromDoc projects a Mongo view document into the typed wire shape R
// using R's struct tags. The framework's canonical tag-driven projector for
// HandleQueryWith{Params,ID} when the wire contract is declared but the
// projection logic is mechanical:
//
//	users.Get("/:id", fwweb.HandleQueryWithID(d.Pipeline,
//	    requests.FindUserByIDRequest{},
//	    fwresponses.AutoFromDoc[requests.FindUserByIDResponse],
//	    handler))
//
// Tag semantics:
//
//	json:"<wire>"  declares the field's name on the outgoing JSON envelope.
//	               Same meaning as encoding/json everywhere else.
//
//	view:"<key>"   declares the field's source key inside the view document.
//	               Optional — when absent, the helper looks up the field
//	               under domain.PascalToSnake(jsonName), matching the
//	               framework's Postgres column-naming convention
//	               (zipCode → zip_code, createdAt → created_at, CPF → cpf).
//	               Declare view: only when the doc field truly diverges from
//	               that default (legacy schemas, vendor-shaped projections):
//
//	                   type AddressOutput struct {
//	                       PostalCode string `json:"postalCode" view:"cep"`
//	                   }
//
//	               Recurses through nested structs, slices of structs, and
//	               pointer-to-struct fields. The tag on a slice field
//	               renames the source key at that level; tags inside the
//	               element type rename inside each element.
//
// Normalizations applied:
//   - Top-level _id → id when "id" is absent and "_id" is a string. Matches
//     the consumer FromDoc convention and parallels the RawDoc passthrough.
//   - Nil slice fields → empty typed slice at every level. Wire output
//     carries "[]" rather than "null".
//
// For wire shapes that need logic beyond tag-driven projection (derived
// fields, conditional projection, ctx-aware shaping) the consumer declares
// its own FromDoc method — the wrapper signature func(map[string]any) R
// accepts either.
func AutoFromDoc[R any](doc map[string]any) R {
	var out R
	plan := planFor(reflect.TypeOf(out))
	renamed := remapDoc(applyIDFallback(doc), plan)
	if raw, err := json.Marshal(renamed); err == nil {
		_ = json.Unmarshal(raw, &out)
	}
	normalizeSlices(reflect.ValueOf(&out).Elem(), plan)
	return out
}

// applyIDFallback returns a doc with id ← _id when "id" is absent and "_id"
// is a string. Top-level only — Mongo's _id is a property of the document
// root; embedded sub-documents never carry one. Does not mutate the input;
// allocates a shallow copy only when a rewrite is needed.
func applyIDFallback(doc map[string]any) map[string]any {
	if doc == nil {
		return doc
	}
	if _, hasID := doc["id"]; hasID {
		return doc
	}
	v, ok := doc["_id"]
	if !ok {
		return doc
	}
	s, ok := v.(string)
	if !ok {
		return doc
	}
	patched := make(map[string]any, len(doc)+1)
	for k, val := range doc {
		patched[k] = val
	}
	patched["id"] = s
	return patched
}

// fieldKind classifies a destination field for the walker so remap and
// normalizeSlices can branch without re-running reflection.
type fieldKind int

const (
	fkScalar fieldKind = iota
	fkStruct
	fkSlice
	fkSliceOfStruct
)

// fieldEntry is one rule in the projection plan: read doc[sourceKey], write
// it under destKey in the renamed map, and recurse via nested when the
// destination is a struct or slice of struct.
type fieldEntry struct {
	fieldIndex []int
	sourceKey  string
	destKey    string
	kind       fieldKind
	nested     *typePlan
}

// typePlan is the cached projection plan for one R type. Built once per
// reflect.Type via planFor; consulted on every call to AutoFromDoc.
type typePlan struct {
	fields []fieldEntry
}

var planCache sync.Map // map[reflect.Type]*typePlan

// planFor returns (and memoizes) the projection plan for t. Pointer types
// are dereferenced; non-struct types yield nil (the helper degrades to a
// no-op renaming step, leaving encoding/json to do whatever it can).
func planFor(t reflect.Type) *typePlan {
	if t == nil {
		return nil
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}
	if v, ok := planCache.Load(t); ok {
		return v.(*typePlan)
	}
	plan := &typePlan{}
	// Store BEFORE recursing so self-referential types terminate (the in-
	// progress plan is consulted by nested calls and filled in afterwards).
	planCache.Store(t, plan)
	buildPlan(plan, t, nil)
	return plan
}

// buildPlan walks t's fields and accumulates entries into plan. basePath
// carries the field index path used for FieldByIndex when t is an embedded
// (anonymous) struct being promoted from an enclosing type.
func buildPlan(plan *typePlan, t reflect.Type, basePath []int) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		path := append(append([]int{}, basePath...), i)

		// Anonymous struct embedded — promote its fields up to this level
		// without renaming the source key (encoding/json convention).
		// Runs BEFORE the IsExported check so unexported anonymous structs
		// still surface their exported fields, matching encoding/json.
		if f.Anonymous {
			ft := f.Type
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				buildPlan(plan, ft, path)
				continue
			}
		}
		if !f.IsExported() {
			continue
		}

		destKey, skip := wireName(f)
		if skip {
			continue
		}
		sourceKey := docSourceKey(f, destKey)

		entry := fieldEntry{
			fieldIndex: path,
			sourceKey:  sourceKey,
			destKey:    destKey,
			kind:       fkScalar,
		}

		ft := f.Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		switch ft.Kind() {
		case reflect.Struct:
			entry.kind = fkStruct
			entry.nested = planFor(ft)
		case reflect.Slice:
			elem := ft.Elem()
			for elem.Kind() == reflect.Pointer {
				elem = elem.Elem()
			}
			if elem.Kind() == reflect.Struct {
				entry.kind = fkSliceOfStruct
				entry.nested = planFor(elem)
			} else {
				entry.kind = fkSlice
			}
		}

		plan.fields = append(plan.fields, entry)
	}
}

// wireName returns the JSON wire name for f and whether the field should be
// skipped entirely (json:"-"). Falls back to the Go field name when the json
// tag is absent or empty; strips the ",omitempty"/",string" modifiers.
func wireName(f reflect.StructField) (string, bool) {
	tag := f.Tag.Get("json")
	if tag == "-" {
		return "", true
	}
	if tag == "" {
		return f.Name, false
	}
	name, _, _ := strings.Cut(tag, ",")
	if name == "" {
		return f.Name, false
	}
	return name, false
}

// docSourceKey returns the source key used to look up f's value inside the
// view doc. The view: tag wins when present and non-"-"; otherwise the
// fallback (the wire name) is converted via domain.PascalToSnake to align
// with the framework's Postgres column-naming convention (zipCode →
// zip_code, ID-as-is, CPF → cpf). The conversion is idempotent on names
// that are already snake_case.
func docSourceKey(f reflect.StructField, fallback string) string {
	if v := f.Tag.Get("view"); v != "" && v != "-" {
		return v
	}
	return domain.PascalToSnake(fallback)
}

// remapDoc produces a new doc where each entry's key is the wire name and
// each value has been recursively remapped according to plan. Entries that
// the plan does not declare are dropped (they would not land in R anyway).
// Returns the input unchanged when plan is nil — the helper falls back to a
// plain JSON round-trip in that case.
func remapDoc(doc map[string]any, plan *typePlan) map[string]any {
	if plan == nil || doc == nil {
		return doc
	}
	out := make(map[string]any, len(plan.fields))
	for _, f := range plan.fields {
		v, ok := doc[f.sourceKey]
		if !ok {
			continue
		}
		out[f.destKey] = remapValue(v, f)
	}
	return out
}

// remapValue dispatches a single value through the appropriate recursion
// branch. Anything whose dynamic type does not match the planned kind passes
// through verbatim — encoding/json handles the rest (TextUnmarshaler on
// time.Time, [16]byte for uuids, scalar coercion, …).
func remapValue(v any, f fieldEntry) any {
	switch f.kind {
	case fkStruct:
		if m, ok := asMap(v); ok && f.nested != nil {
			return remapDoc(m, f.nested)
		}
	case fkSliceOfStruct:
		if f.nested != nil {
			if items, ok := asSliceOfMaps(v); ok {
				out := make([]map[string]any, len(items))
				for i, item := range items {
					out[i] = remapDoc(item, f.nested)
				}
				return out
			}
		}
	}
	return v
}

// asMap normalizes any "string-keyed map-like" value — plain map[string]any
// as well as the mongo-driver's bson.M (a named type sharing the same
// underlying shape but distinct under a type assertion) — into the uniform
// map[string]any the recursion expects. Reflection avoids importing bson in
// the web layer; reflect.Kind sees through any named map type and is
// type-system honest.
func asMap(v any) (map[string]any, bool) {
	if m, ok := v.(map[string]any); ok {
		return m, true
	}
	rv := reflect.ValueOf(v)
	if !rv.IsValid() || rv.Kind() != reflect.Map {
		return nil, false
	}
	if rv.Type().Key().Kind() != reflect.String {
		return nil, false
	}
	out := make(map[string]any, rv.Len())
	iter := rv.MapRange()
	for iter.Next() {
		out[iter.Key().String()] = iter.Value().Interface()
	}
	return out, true
}

// asSliceOfMaps normalizes any slice-like value carrying map-like elements
// into the uniform []map[string]any shape. Covers the mongo-driver's bson.A
// (a named []any) plus plain []any and []map[string]any. Returns (_, false)
// when the value is not a slice of documents (e.g. []string), letting the
// caller fall through and copy the value verbatim.
func asSliceOfMaps(v any) ([]map[string]any, bool) {
	rv := reflect.ValueOf(v)
	if !rv.IsValid() || rv.Kind() != reflect.Slice {
		return nil, false
	}
	out := make([]map[string]any, 0, rv.Len())
	for i := 0; i < rv.Len(); i++ {
		m, ok := asMap(rv.Index(i).Interface())
		if !ok {
			return nil, false
		}
		out = append(out, m)
	}
	return out, true
}

// normalizeSlices walks v according to plan and replaces nil slice fields
// with empty typed slices. Recurses into nested structs and slice elements
// so the rule applies at every depth — wire output is consistent regardless
// of where the slice lives.
func normalizeSlices(v reflect.Value, plan *typePlan) {
	if plan == nil || !v.IsValid() {
		return
	}
	for _, f := range plan.fields {
		field := v.FieldByIndex(f.fieldIndex)
		if !field.IsValid() {
			continue
		}
		switch f.kind {
		case fkSlice:
			if field.Kind() == reflect.Slice && field.IsNil() && field.CanSet() {
				field.Set(reflect.MakeSlice(field.Type(), 0, 0))
			}
		case fkSliceOfStruct:
			if field.Kind() == reflect.Slice {
				if field.IsNil() && field.CanSet() {
					field.Set(reflect.MakeSlice(field.Type(), 0, 0))
				}
				if f.nested != nil {
					for i := 0; i < field.Len(); i++ {
						elem := field.Index(i)
						for elem.Kind() == reflect.Pointer {
							if elem.IsNil() {
								elem = reflect.Value{}
								break
							}
							elem = elem.Elem()
						}
						if elem.IsValid() && elem.Kind() == reflect.Struct {
							normalizeSlices(elem, f.nested)
						}
					}
				}
			}
		case fkStruct:
			elem := field
			for elem.Kind() == reflect.Pointer {
				if elem.IsNil() {
					elem = reflect.Value{}
					break
				}
				elem = elem.Elem()
			}
			if elem.IsValid() && elem.Kind() == reflect.Struct && f.nested != nil {
				normalizeSlices(elem, f.nested)
			}
		}
	}
}
