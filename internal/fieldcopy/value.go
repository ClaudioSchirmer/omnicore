// Package fieldcopy is the shared engine behind the framework's Auto mappings:
// given two field types, it answers whether one value can be copied into the
// other by plain assignment, and compiles the copier when it can.
//
// It deliberately owns only the VALUE level. Each Auto seat owns its own
// struct walk, because the two directions check opposite sides:
//
//   - Result → Response (web/responses): the RESPONSE drives — every wire
//     field must be backed by the Result, and Result fields the Response omits
//     are simply cut from the wire.
//   - Request → Command (web/requests): the REQUEST drives — every wire field
//     must land on the Command, and Command fields the Request does not supply
//     are filled elsewhere (a path id, an identity overlay, a default).
//
// One rule underneath both: the WIRE side is fully checked, so no wire field
// is ever silently disconnected; the APPLICATION side may carry more.
package fieldcopy

import (
	"encoding"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// Copier moves one source value into one settable destination value.
type Copier func(src, dst reflect.Value)

// NestedBuilder compiles the copier for a nested struct pair. Each seat passes
// its own, so the recursion keeps that seat's driver side and cycle detection.
// It returns ("" reason) when the pair copies cleanly.
type NestedBuilder func(srcT, dstT reflect.Type) (Copier, string)

var (
	jsonMarshalerType   = reflect.TypeOf((*json.Marshaler)(nil)).Elem()
	jsonUnmarshalerType = reflect.TypeOf((*json.Unmarshaler)(nil)).Elem()
	textMarshalerType   = reflect.TypeOf((*encoding.TextMarshaler)(nil)).Elem()
	textUnmarshalerType = reflect.TypeOf((*encoding.TextUnmarshaler)(nil)).Elem()
	domainIDType        = reflect.TypeOf(domain.ID{})
)

// HasCustomCodec reports whether t (or *t) declares its own JSON or text
// codec — the signal that a value of this type carries logic a structural
// copy cannot claim to reproduce when the other side is a DIFFERENT type.
func HasCustomCodec(t reflect.Type) bool {
	pt := reflect.PointerTo(t)
	return t.Implements(jsonMarshalerType) || pt.Implements(jsonMarshalerType) ||
		t.Implements(jsonUnmarshalerType) || pt.Implements(jsonUnmarshalerType) ||
		t.Implements(textMarshalerType) || pt.Implements(textMarshalerType) ||
		t.Implements(textUnmarshalerType) || pt.Implements(textUnmarshalerType)
}

// ValueCopier compiles the copier for one (source, destination) field shape,
// or reports the reason the pair cannot travel by assignment ("" = fine).
//
// The supported matrix is exactly the set of shapes whose copy is exact:
//
//   - identical types (any kind);
//   - pointer wrapping/unwrapping on either side (a nil source leaves the
//     destination zero, the way an absent value does);
//   - same-family scalar conversions, with out-of-range values leaving the
//     destination zero rather than truncating;
//   - domain.ID ↔ string (the ID's value IS its string form);
//   - struct → struct and slice → slice, resolved through nested.
//
// Everything else — a custom codec across different types, a narrowing whose
// rounding belongs to the codec, an interface source, a map whose value type
// changes — is refused, because guessing would be a silent behavior change.
func ValueCopier(srcT, dstT reflect.Type, nested NestedBuilder) (Copier, string) {
	if srcT == dstT {
		return func(src, dst reflect.Value) { dst.Set(src) }, ""
	}

	if srcT.Kind() == reflect.Pointer {
		inner, reason := ValueCopier(srcT.Elem(), dstT, nested)
		if reason != "" {
			return nil, reason
		}
		return func(src, dst reflect.Value) {
			if src.IsNil() {
				return
			}
			inner(src.Elem(), dst)
		}, ""
	}
	if dstT.Kind() == reflect.Pointer {
		inner, reason := ValueCopier(srcT, dstT.Elem(), nested)
		if reason != "" {
			return nil, reason
		}
		return func(src, dst reflect.Value) {
			p := reflect.New(dst.Type().Elem())
			inner(src, p.Elem())
			dst.Set(p)
		}, ""
	}

	if srcT == domainIDType && dstT.Kind() == reflect.String && !HasCustomCodec(dstT) {
		return func(src, dst reflect.Value) {
			dst.SetString(src.Interface().(domain.ID).Value())
		}, ""
	}
	if dstT == domainIDType && srcT.Kind() == reflect.String && !HasCustomCodec(srcT) {
		return func(src, dst reflect.Value) {
			dst.Set(reflect.ValueOf(domain.NewID(src.String())))
		}, ""
	}

	if HasCustomCodec(srcT) || HasCustomCodec(dstT) {
		return nil, "the types differ and one of them declares its own JSON/text codec, whose output a structural copy cannot reproduce"
	}

	sk, dk := srcT.Kind(), dstT.Kind()
	switch dk {
	case reflect.String:
		if sk == reflect.String {
			return func(src, dst reflect.Value) { dst.SetString(src.String()) }, ""
		}
	case reflect.Bool:
		if sk == reflect.Bool {
			return func(src, dst reflect.Value) { dst.SetBool(src.Bool()) }, ""
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		switch sk {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			return func(src, dst reflect.Value) {
				if i := src.Int(); !dst.OverflowInt(i) {
					dst.SetInt(i)
				}
			}, ""
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			return func(src, dst reflect.Value) {
				u := src.Uint()
				if u <= 1<<63-1 && !dst.OverflowInt(int64(u)) {
					dst.SetInt(int64(u))
				}
			}, ""
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		switch sk {
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			return func(src, dst reflect.Value) {
				if u := src.Uint(); !dst.OverflowUint(u) {
					dst.SetUint(u)
				}
			}, ""
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			return func(src, dst reflect.Value) {
				i := src.Int()
				if i >= 0 && !dst.OverflowUint(uint64(i)) {
					dst.SetUint(uint64(i))
				}
			}, ""
		}
	case reflect.Float32, reflect.Float64:
		switch sk {
		case reflect.Float32, reflect.Float64:
			// float32 → float64 widens through the SHORTEST DECIMAL: a value
			// rendered as "3.14159" must parse back to that, not to the raw
			// binary widening (3.1415901184…).
			widenViaDecimal := sk == reflect.Float32 && dk == reflect.Float64
			return func(src, dst reflect.Value) {
				f := src.Float()
				if widenViaDecimal {
					f, _ = strconv.ParseFloat(strconv.FormatFloat(f, 'g', -1, 32), 64)
				}
				if !dst.OverflowFloat(f) {
					dst.SetFloat(f)
				}
			}, ""
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			return func(src, dst reflect.Value) { dst.SetFloat(float64(src.Int())) }, ""
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			return func(src, dst reflect.Value) { dst.SetFloat(float64(src.Uint())) }, ""
		}
	case reflect.Struct:
		if sk == reflect.Struct {
			return nested(srcT, dstT)
		}
	case reflect.Slice:
		if sk == reflect.Slice {
			elemCp, reason := ValueCopier(srcT.Elem(), dstT.Elem(), nested)
			if reason != "" {
				return nil, reason
			}
			dt := dstT
			return func(src, dst reflect.Value) {
				if src.IsNil() {
					return
				}
				n := src.Len()
				out := reflect.MakeSlice(dt, n, n)
				for i := 0; i < n; i++ {
					elemCp(src.Index(i), out.Index(i))
				}
				dst.Set(out)
			}, ""
		}
	case reflect.Map:
		// Same key type, convertible values: an element-wise copy, exactly what
		// the wire form of a map is. A different KEY type is refused — the key
		// is what a consumer indexes by, so silently converting it would change
		// the shape the caller reads.
		if sk == reflect.Map && srcT.Key() == dstT.Key() {
			valCp, reason := ValueCopier(srcT.Elem(), dstT.Elem(), nested)
			if reason != "" {
				return nil, reason
			}
			dt := dstT
			return func(src, dst reflect.Value) {
				if src.IsNil() {
					return
				}
				out := reflect.MakeMapWithSize(dt, src.Len())
				iter := src.MapRange()
				for iter.Next() {
					v := reflect.New(dt.Elem()).Elem()
					valCp(iter.Value(), v)
					out.SetMapIndex(iter.Key(), v)
				}
				dst.Set(out)
			}, ""
		}
	}
	return nil, fmt.Sprintf("no direct conversion from %s to %s", srcT, dstT)
}

// ExportedFields indexes a struct type's exported fields by Go name, walking
// through embedded structs the way encoding/json promotes them. skipJSONDash
// drops fields tagged `json:"-"` — the wire side's "this never travels" mark.
func ExportedFields(t reflect.Type, skipJSONDash bool) map[string][]int {
	out := map[string][]int{}
	var walk func(reflect.Type, []int)
	walk = func(t reflect.Type, base []int) {
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		if t.Kind() != reflect.Struct {
			return
		}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			path := append(append([]int{}, base...), i)
			if f.Anonymous {
				ft := f.Type
				for ft.Kind() == reflect.Pointer {
					ft = ft.Elem()
				}
				if ft.Kind() == reflect.Struct {
					walk(ft, path)
					continue
				}
			}
			if !f.IsExported() {
				continue
			}
			if skipJSONDash && f.Tag.Get("json") == "-" {
				continue
			}
			if _, dup := out[f.Name]; !dup {
				out[f.Name] = path
			}
		}
	}
	walk(t, nil)
	return out
}

// FieldAlloc walks an index path allocating nil embedded pointers on the way,
// mirroring how encoding/json reaches a promoted field.
func FieldAlloc(v reflect.Value, index []int) reflect.Value {
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
