package responses

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"sync"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// Map converts an application-layer Result into the typed wire Response
// TResp using TResp's json tags — the generic TResult→TResp mapper behind
// every Response's FromResult method. The Result is application-pure (no
// wire tags), so its JSON form is keyed by Go field name; Map reads each
// Response field's same-named Result field and writes it under the field's
// json wire name, recursing through nested structs, slices of structs and
// pointer fields. The canonical usage mirrors the command side:
//
//	func (FindUsersByParamsResponse) FromResult(r appqueries.FindUsersResult) FindUsersByParamsResponse {
//	    return fwresponses.AutoFromResult[FindUsersByParamsResponse](r)
//	}
//
// A Response that needs renaming or reshaping beyond the tags writes its
// FromResult by hand instead — the constructors accept any func(TResult)
// TResp. Read-side COMPUTATION does not belong here: derived fields,
// ctx-aware shaping and error paths live in the Query's FromQueryResult hook in
// the application layer, so every transport surface sees the same values.
//
// Tag semantics:
//
//	json:"<wire>"  declares the field's name on the outgoing JSON envelope.
//	               Same meaning as encoding/json everywhere else. The source
//	               key is the field's Go name on the Result.
//
// Normalizations applied:
//   - Nil slice fields → empty typed slice at every level (wire output
//     carries "[]" rather than "null" on non-sparse shapes).
//   - EnumValueObject fields carrying an out-of-set value converge to the
//     Unknown sentinel (idempotent with the application-side fill).
//
// The Result↔Response name alignment is boot-guarded by the constructors
// (queryschema.ValidateResultAlignment), so a Response field with no Result
// backing fails at mount, not silently at runtime.
func AutoFromResult[TResp AutoMapper, TResult any](result TResult) TResp {
	var out TResp
	plan := planFor(reflect.TypeOf(out))
	srcV := reflect.ValueOf(result)
	for srcV.Kind() == reflect.Pointer && !srcV.IsNil() {
		srcV = srcV.Elem()
	}
	respType := reflect.TypeOf(out)
	if !srcV.IsValid() || (srcV.Kind() == reflect.Pointer && srcV.IsNil()) {
		// No Result to read (a nil pointer Result). There is nothing to copy
		// and nothing wrong with the pair — answer the zero Response, with the
		// normalizations below still applied so the wire shape stays regular.
		normalizeSlices(reflect.ValueOf(&out).Elem(), plan)
		convergeEnums(reflect.ValueOf(&out).Elem(), plan)
		return out
	}
	cp := pairCopierFor(srcTypeOf(srcV), respType)
	if cp == nil {
		// The route constructors validate this very pair at Mount, so a
		// service wired through them never reaches here. What does is a Map
		// call the framework never saw — a hand-rolled handler or a test —
		// carrying a pair that cannot be copied. That is a declaration the
		// type cannot honor (it embedded Auto), so it fails loudly with the
		// diagnostic the boot guard would have printed, rather than quietly
		// costing a serialization round trip on every request.
		panic(FormatAutoFromResultGuard(reflect.TypeOf(result), respType,
			AutoFromResultReason(reflect.TypeOf(result), respType)))
	}
	cp(srcV, reflect.ValueOf(&out).Elem())
	normalizeSlices(reflect.ValueOf(&out).Elem(), plan)
	convergeEnums(reflect.ValueOf(&out).Elem(), plan)
	return out
}

// srcTypeOf answers the concrete struct type behind a (deref'd) source value,
// or nil when there is none.
func srcTypeOf(v reflect.Value) reflect.Type {
	if !v.IsValid() || v.Kind() != reflect.Struct {
		return nil
	}
	return v.Type()
}

// goDocOf renders a (tagless) Result value as its Go-field-keyed document
// via a JSON round-trip. Numbers decode as json.Number so 64-bit integers
// survive the trip without float64 truncation.
func goDocOf(result any) map[string]any {
	raw, err := json.Marshal(result)
	if err != nil {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return nil
	}
	return doc
}

// convergeEnums walks v per plan and maps every EnumValueObject field whose
// populated value is out of its declared member set to the Unknown sentinel —
// the read-side twin of the entity reconstruction's converge. Recurses into
// nested structs and slice-of-struct elements so an enum at any depth is covered.
func convergeEnums(v reflect.Value, plan *typePlan) {
	if plan == nil || !v.IsValid() {
		return
	}
	for _, f := range plan.fields {
		field := v.FieldByIndex(f.fieldIndex)
		if !field.IsValid() {
			continue
		}
		if f.enumType != nil {
			convergeEnumField(field)
			continue
		}
		switch f.kind {
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
				convergeEnums(elem, f.nested)
			}
		case fkSliceOfStruct:
			if field.Kind() == reflect.Slice && f.nested != nil {
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
						convergeEnums(elem, f.nested)
					}
				}
			}
		}
	}
}

// convergeEnumField converges one populated enum field (a nil pointer is left
// untouched — absence, not Unknown).
func convergeEnumField(field reflect.Value) {
	fv := field
	if fv.Kind() == reflect.Pointer {
		if fv.IsNil() {
			return
		}
		fv = fv.Elem()
	}
	raw, ok := domain.ValueObjectValue(fv.Interface())
	if !ok {
		return
	}
	converged, err := domain.NewValueObjectValue(fv.Type(), raw)
	if err != nil {
		return
	}
	if fv.CanSet() {
		fv.Set(reflect.ValueOf(converged))
	}
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
	// enumType is non-nil when the field is an EnumValueObject (deref'd of any
	// pointer). After the JSON round-trip populates the field, convergeEnums maps
	// an out-of-set value to the Unknown sentinel — parity with the write-side
	// entity reconstruction, so a document carrying a stale/tampered enum
	// value never surfaces as a phantom member on the wire.
	enumType reflect.Type
}

// typePlan is the cached projection plan for one TResp type. Built once per
// reflect.Type via planFor; consulted on every call to Map.
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
		// The Result's JSON form is keyed by Go field name (Results carry no
		// wire tags). So the source key is the Go field name; the json tag
		// names the wire output.
		sourceKey := f.Name

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
		if domain.IsEnumValueObject(reflect.Zero(ft).Interface()) {
			entry.enumType = ft
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

// remapDoc produces a new doc where each entry's key is the wire name and
// each value has been recursively remapped according to plan. Entries that
// the plan does not declare are dropped (they would not land in TResp anyway).
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

// asMap normalizes any "string-keyed map-like" value into the uniform
// map[string]any the recursion expects. Reflection sees through named map
// types and is type-system honest.
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
// into the uniform []map[string]any shape. Returns (_, false) when the value
// is not a slice of documents (e.g. []string), letting the caller fall
// through and copy the value verbatim.
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
