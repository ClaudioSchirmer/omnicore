package queries

import (
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// The direct document fill: ResultFromDoc's hot path. The canonical view
// document is a Go-field-keyed map[string]any, so filling a TResult used to be
// a json.Marshal + json.Unmarshal round-trip — two full codec passes per
// document, paid once per page item on every read. This file replaces that
// with a reflection fill driven by a cached per-type plan, preserving the
// round-trip's semantics field by field:
//
//   - matching is by Go field name, exact first, then case-insensitive
//     (encoding/json's key folding), promoted embedded fields included;
//   - a doc key with no field is dropped; a field with no doc key stays zero;
//   - a value the fast path cannot coerce goes through a PER-FIELD JSON
//     round-trip — the same coercion the whole-doc trip applied, at a
//     fraction of the cost and only where actually needed;
//   - a field whose type declares its own json.Unmarshaler keeps its custom
//     decode via that same per-field fallback (domain.ID and time.Time are
//     special-cased for speed — their round-trip is value-identity);
//   - a TResult that is not a plain struct (map, pointer, scalar) keeps the
//     legacy whole-doc round-trip, bit-for-bit.
//
// The fill only writes fields; the read-side normalizations (nil-slice →
// empty, enum converge) still run after it, exactly as before.

// fillKind classifies a fill target so setFilledValue can branch without
// re-running reflection.
type fillKind int

const (
	fkJSONFallback fillKind = iota // per-field JSON round-trip (exotic targets)
	fkString
	fkBool
	fkInt
	fkUint
	fkFloat
	fkTime     // exactly time.Time
	fkDomainID // exactly domain.ID
	fkStruct
	fkSlice
)

// fillTarget describes how to set one value shape: the deref'd target type,
// its kind class, and — for structs and slices — the nested plan / element
// target the recursion descends into.
type fillTarget struct {
	typ    reflect.Type // deref'd target type
	kind   fillKind
	nested *fillPlan   // struct targets: the nested plan
	elem   *fillTarget // slice targets: the element target
}

// fillField is one settable field in a fillPlan: its Go name, the
// FieldByIndex path (possibly through promoted anonymous embeds) and the
// target descriptor shared with the element recursion.
type fillField struct {
	name   string
	index  []int
	target fillTarget
}

// fillPlan is the cached fill plan for one struct type.
type fillPlan struct {
	fields []fillField
	byName map[string]int // exact Go field name → fields index
	byFold map[string]int // lower-cased name → fields index (first wins)
}

var (
	fillPlanCache = sync.Map{} // map[reflect.Type]*fillPlan

	fillTimeType        = reflect.TypeOf(time.Time{})
	fillDomainIDType    = reflect.TypeOf(domain.ID{})
	fillUnmarshalerType = reflect.TypeOf((*json.Unmarshaler)(nil)).Elem()
)

// fillPlanFor returns (and memoizes) the fill plan for t, or nil when t is
// not a plain struct type — the caller then keeps the legacy whole-doc JSON
// round-trip. Pointer TResults deliberately yield nil: their downstream
// normalization contract predates this fill and is preserved untouched.
//
// The plan graph is built OFF-CACHE and published only once complete: a plan
// visible to another goroutine while its maps are still being written is a
// concurrent map read/write — a runtime crash, not a recoverable panic — and
// the first requests after boot are exactly the moment two goroutines race
// for the same not-yet-cached type. `building` is this goroutine's private
// in-progress set; it breaks self-referential cycles, and the pointer it
// hands back on a cycle is complete before anything can read it, because
// nothing is published until the top-level build returns.
func fillPlanFor(t reflect.Type) *fillPlan {
	if t == nil || t.Kind() != reflect.Struct {
		return nil
	}
	if v, ok := fillPlanCache.Load(t); ok {
		return v.(*fillPlan)
	}
	building := map[reflect.Type]*fillPlan{}
	plan := fillPlanOf(t, building)
	for bt, bp := range building {
		fillPlanCache.LoadOrStore(bt, bp)
	}
	return plan
}

// fillPlanOf answers the plan for one struct type DURING a build: the
// published one when it exists, the in-progress one on a cycle, a freshly
// built one (recorded in building, never in the cache) otherwise.
func fillPlanOf(t reflect.Type, building map[reflect.Type]*fillPlan) *fillPlan {
	if v, ok := fillPlanCache.Load(t); ok {
		return v.(*fillPlan)
	}
	if p, ok := building[t]; ok {
		return p
	}
	plan := &fillPlan{byName: map[string]int{}, byFold: map[string]int{}}
	building[t] = plan
	buildFillPlan(plan, t, nil, building)
	return plan
}

func buildFillPlan(plan *fillPlan, t reflect.Type, basePath []int, building map[reflect.Type]*fillPlan) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		path := append(append([]int{}, basePath...), i)

		// Anonymous struct embedded — promote its fields (encoding/json
		// convention), before the IsExported check so unexported anonymous
		// structs still surface their exported fields.
		if f.Anonymous {
			ft := f.Type
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				buildFillPlan(plan, ft, path, building)
				continue
			}
		}
		if !f.IsExported() {
			continue
		}

		idx := len(plan.fields)
		plan.fields = append(plan.fields, fillField{
			name:   f.Name,
			index:  path,
			target: fillTargetFor(f.Type, building),
		})
		if _, dup := plan.byName[f.Name]; !dup {
			plan.byName[f.Name] = idx
		}
		fold := strings.ToLower(f.Name)
		if _, dup := plan.byFold[fold]; !dup {
			plan.byFold[fold] = idx
		}
	}
}

// fillTargetFor classifies the (deref'd) type a value will be written into.
func fillTargetFor(t reflect.Type, building map[reflect.Type]*fillPlan) fillTarget {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	ft := fillTarget{typ: t, kind: fkJSONFallback}
	switch {
	case t == fillTimeType:
		ft.kind = fkTime
		return ft
	case t == fillDomainIDType:
		ft.kind = fkDomainID
		return ft
	case reflect.PointerTo(t).Implements(fillUnmarshalerType):
		// A custom decoder owns its semantics — the per-field JSON fallback
		// runs it exactly as the whole-doc round-trip did.
		return ft
	}
	switch t.Kind() {
	case reflect.String:
		ft.kind = fkString
	case reflect.Bool:
		ft.kind = fkBool
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		ft.kind = fkInt
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		ft.kind = fkUint
	case reflect.Float32, reflect.Float64:
		ft.kind = fkFloat
	case reflect.Struct:
		ft.kind = fkStruct
		ft.nested = fillPlanOf(t, building)
	case reflect.Slice:
		ft.kind = fkSlice
		elem := fillTargetFor(t.Elem(), building)
		ft.elem = &elem
	}
	return ft
}

// fillStructFromDoc writes doc's entries into dst per plan. dst must be an
// addressable struct value.
func fillStructFromDoc(dst reflect.Value, doc map[string]any, plan *fillPlan) {
	for k, v := range doc {
		idx, ok := plan.byName[k]
		if !ok {
			idx, ok = plan.byFold[strings.ToLower(k)]
			if !ok {
				continue
			}
		}
		f := &plan.fields[idx]
		field := fieldByIndexAlloc(dst, f.index)
		if !field.IsValid() || !field.CanSet() {
			continue
		}
		setFilledValue(field, v, &f.target)
	}
}

// fieldByIndexAlloc walks an index path like reflect's FieldByIndex but
// allocates nil anonymous pointer embeds on the way (encoding/json does the
// same when decoding into a promoted field). Returns the zero Value when an
// intermediate pointer cannot be allocated.
func fieldByIndexAlloc(v reflect.Value, index []int) reflect.Value {
	for i, x := range index {
		if i > 0 {
			for v.Kind() == reflect.Pointer {
				if v.IsNil() {
					if !v.CanSet() {
						return reflect.Value{}
					}
					v.Set(reflect.New(v.Type().Elem()))
				}
				v = v.Elem()
			}
		}
		v = v.Field(x)
	}
	return v
}

// setFilledValue writes one doc value into dst. A nil value is a no-op (JSON
// null leaves the field alone — nil pointer stays nil, value stays zero).
// Pointer targets allocate through to the deref'd type. Anything the fast
// paths cannot coerce goes through the per-field JSON fallback, which
// reproduces the whole-doc round-trip's behavior for exactly that value:
// same coercions, same custom unmarshalers, same silent zero on mismatch.
func setFilledValue(dst reflect.Value, v any, target *fillTarget) {
	if v == nil {
		return
	}
	// A TYPED nil pointer is the same absence an untyped nil is. A document
	// assembled from a LOADED ENTITY rather than from a store driver carries
	// them: a nullable column reads back as (*string)(nil) inside the any, and a
	// left join with no counterpart is exactly that by construction. Without this
	// the pointer loop below would ALLOCATE the destination and settle it at its
	// zero value — turning "there is no counterpart" into an empty string on the
	// wire, which is the confusion the LeftJoin nullability rule exists to
	// prevent. The JSON round-trip this fill must match renders a nil pointer as
	// null and leaves the field alone; do the same.
	if rv := reflect.ValueOf(v); rv.Kind() == reflect.Pointer && rv.IsNil() {
		return
	}
	for dst.Kind() == reflect.Pointer {
		if dst.IsNil() {
			dst.Set(reflect.New(dst.Type().Elem()))
		}
		dst = dst.Elem()
	}
	switch target.kind {
	case fkString:
		switch s := v.(type) {
		case string:
			dst.SetString(s)
		case time.Time:
			// The round-trip renders a time value as its RFC 3339 form — the
			// exact bytes time.Time.MarshalJSON emits.
			dst.SetString(s.Format(time.RFC3339Nano))
		default:
			fillViaJSON(dst, v)
		}
	case fkBool:
		if b, ok := v.(bool); ok {
			dst.SetBool(b)
		} else {
			fillViaJSON(dst, v)
		}
	case fkInt:
		if i, ok := fillAsInt64(v); ok {
			if !dst.OverflowInt(i) {
				dst.SetInt(i)
			}
			// overflow: the round-trip left the field zero — same here.
		} else {
			fillViaJSON(dst, v)
		}
	case fkUint:
		if i, ok := fillAsInt64(v); ok && i >= 0 {
			u := uint64(i)
			if !dst.OverflowUint(u) {
				dst.SetUint(u)
			}
		} else if !ok {
			fillViaJSON(dst, v)
		}
	case fkFloat:
		if f, ok := fillAsFloat64(v); ok {
			dst.SetFloat(f)
		} else {
			fillViaJSON(dst, v)
		}
	case fkTime:
		if t, ok := v.(time.Time); ok {
			dst.Set(reflect.ValueOf(t))
		} else {
			fillViaJSON(dst, v)
		}
	case fkDomainID:
		switch id := v.(type) {
		case string:
			dst.Set(reflect.ValueOf(domain.NewID(id)))
		case domain.ID:
			dst.Set(reflect.ValueOf(id))
		default:
			fillViaJSON(dst, v)
		}
	case fkStruct:
		if m, ok := v.(map[string]any); ok && target.nested != nil {
			fillStructFromDoc(dst, m, target.nested)
		} else {
			fillViaJSON(dst, v)
		}
	case fkSlice:
		src := reflect.ValueOf(v)
		if src.Kind() != reflect.Slice {
			fillViaJSON(dst, v)
			return
		}
		n := src.Len()
		out := reflect.MakeSlice(dst.Type(), n, n)
		for i := 0; i < n; i++ {
			item := src.Index(i).Interface()
			setFilledValue(out.Index(i), item, target.elem)
		}
		dst.Set(out)
	default:
		fillViaJSON(dst, v)
	}
}

// fillAsInt64 normalizes the integer shapes a view document carries (BSON
// int32/int64, Go ints, whole floats, json.Number) into an int64. A float
// with a fractional part answers !ok is-a-mismatch — the round-trip erred and
// left the field zero, and the caller preserves that by not setting.
func fillAsInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int8:
		return int64(n), true
	case int16:
		return int64(n), true
	case int32:
		return int64(n), true
	case int64:
		return n, true
	case uint:
		return int64(n), true
	case uint8:
		return int64(n), true
	case uint16:
		return int64(n), true
	case uint32:
		return int64(n), true
	case uint64:
		if n > 1<<63-1 {
			return 0, false
		}
		return int64(n), true
	case float32:
		if float32(int64(n)) == n {
			return int64(n), true
		}
		return 0, true // fractional: mismatch, leave zero (round-trip parity)
	case float64:
		if float64(int64(n)) == n {
			return int64(n), true
		}
		return 0, true // fractional: mismatch, leave zero (round-trip parity)
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return i, true
		}
		return 0, true
	default:
		return 0, false
	}
}

// fillAsFloat64 normalizes any numeric document value into a float64. A
// float32 widens through its SHORTEST DECIMAL form — the JSON round-trip
// renders "3.14159" and parses that into the float64, not the raw binary
// widening (3.1415901184…) — so parity holds bit for bit.
func fillAsFloat64(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		f, _ := strconv.ParseFloat(strconv.FormatFloat(float64(n), 'g', -1, 32), 64)
		return f, true
	case int:
		return float64(n), true
	case int8:
		return float64(n), true
	case int16:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint:
		return float64(n), true
	case uint8:
		return float64(n), true
	case uint16:
		return float64(n), true
	case uint32:
		return float64(n), true
	case uint64:
		return float64(n), true
	case json.Number:
		if f, err := n.Float64(); err == nil {
			return f, true
		}
		return 0, true
	default:
		return 0, false
	}
}

// fillViaJSON is the per-field escape hatch: marshal the single value and
// unmarshal it into the field, which reproduces the retired whole-doc
// round-trip for exactly this (value, field) pair — custom unmarshalers,
// scalar coercions and the silent zero-on-mismatch alike. Errors are
// swallowed like the whole-doc trip swallowed them.
func fillViaJSON(dst reflect.Value, v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		return
	}
	_ = json.Unmarshal(raw, dst.Addr().Interface())
}
