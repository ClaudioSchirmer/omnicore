package responses

import (
	"encoding"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// The field-by-field copier behind [AutoFromResult]: one compiled plan per
// (Result, Response) pair, built on first use and cached. No serialization is
// involved — the values travel by assignment.
//
// A pair whose shape cannot be copied exactly is not silently degraded: the
// reason is reported (see [AutoFromResultReason]) and the route constructors
// turn it into a boot panic, because a Response that embedded [Auto] declared
// that this travel works.
//
// Destination fields come from the typePlan (json:"-" skipped, embedded fields
// promoted) and each reads the same-named source field (exact name first, then
// case-insensitive — the encoding/json key folding). A Response field with no
// source stays zero, and source fields the Response does not declare are never
// read: the Response may expose a SUBSET of the Result. The post-copy
// normalizations (nil-slice → empty, enum converge) run unchanged.

// fieldCopier copies one source value into one settable destination value.
type fieldCopier func(src, dst reflect.Value)

type pairKey struct{ src, dst reflect.Type }

// pairEntry caches one (src, dst) verdict: the compiled copier, or a nil
// copier plus the reason the pair cannot be auto-mapped.
type pairEntry struct {
	copier fieldCopier
	reason string
}

var pairCache sync.Map // pairKey → *pairEntry

// pairEntryFor compiles (and memoizes) the verdict for a struct pair.
func pairEntryFor(srcT, dstT reflect.Type) *pairEntry {
	key := pairKey{srcT, dstT}
	if v, ok := pairCache.Load(key); ok {
		return v.(*pairEntry)
	}
	copier, reason := buildStructCopier(srcT, dstT, map[pairKey]bool{})
	entry := &pairEntry{reason: reason}
	if reason == "" {
		entry.copier = copier
	}
	pairCache.Store(key, entry)
	return entry
}

// pairCopierFor returns the compiled struct copier for (srcT, dstT), or nil
// when the pair cannot be copied. Both must be plain struct types.
func pairCopierFor(srcT, dstT reflect.Type) fieldCopier {
	if srcT == nil || dstT == nil || srcT.Kind() != reflect.Struct || dstT.Kind() != reflect.Struct {
		return nil
	}
	return pairEntryFor(srcT, dstT).copier
}

// AutoFromResultReason reports why [AutoFromResult] cannot serve the
// Result→Response travel with its compiled copier, or "" when it can. It is the
// diagnostic seat the route constructors consult at Mount: a Response that
// declared [Auto] and cannot honor the declaration is a boot panic naming the
// offending field, never a silent null or a silent per-request slowdown.
//
// The verdict is a pure function of the two types — the same one
// [AutoFromResult] itself caches — so calling it at boot also pre-warms the cache: the first request
// no longer pays the compilation.
//
// Only the fields the RESPONSE declares are examined, so a Result may carry
// anything beyond them (the Response is free to expose a subset).
//
// Types are dereferenced through pointers. A non-struct on either side (a map
// Result, an `any` Response) can never take the direct path and reports so.
func AutoFromResultReason(resultType, respType reflect.Type) string {
	src, srcOK := derefStructType(resultType)
	dst, dstOK := derefStructType(respType)
	if !srcOK {
		return fmt.Sprintf("the Result type %s is not a struct", typeName(resultType))
	}
	if !dstOK {
		return fmt.Sprintf("the Response type %s is not a struct", typeName(respType))
	}
	return pairEntryFor(src, dst).reason
}

// derefStructType walks pointers to the underlying struct type.
func derefStructType(t reflect.Type) (reflect.Type, bool) {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil || t.Kind() != reflect.Struct {
		return nil, false
	}
	return t, true
}

// typeName renders a possibly-nil reflect.Type for a diagnostic message.
func typeName(t reflect.Type) string {
	if t == nil {
		return "<nil>"
	}
	return t.String()
}

// srcFields is the promoted, exported field index of a source struct type:
// exact Go name → index path, plus the case-insensitive fold.
type srcFields struct {
	byName map[string][]int
	byFold map[string][]int
}

var srcFieldsCache sync.Map // reflect.Type → *srcFields

func srcFieldsOf(t reflect.Type) *srcFields {
	if v, ok := srcFieldsCache.Load(t); ok {
		return v.(*srcFields)
	}
	sf := &srcFields{byName: map[string][]int{}, byFold: map[string][]int{}}
	collectSrcFields(sf, t, nil)
	srcFieldsCache.Store(t, sf)
	return sf
}

func collectSrcFields(sf *srcFields, t reflect.Type, basePath []int) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		path := append(append([]int{}, basePath...), i)
		if f.Anonymous {
			ft := f.Type
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				collectSrcFields(sf, ft, path)
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		if _, dup := sf.byName[f.Name]; !dup {
			sf.byName[f.Name] = path
		}
		fold := strings.ToLower(f.Name)
		if _, dup := sf.byFold[fold]; !dup {
			sf.byFold[fold] = path
		}
	}
}

// buildStructCopier compiles the copier for one struct pair, or reports the
// reason the pair cannot take the direct path ("" = supported). inProgress
// breaks recursion cycles (self-referential shapes): a pair already being
// compiled is answered with a lazy lookup resolved at copy time.
func buildStructCopier(srcT, dstT reflect.Type, inProgress map[pairKey]bool) (fieldCopier, string) {
	key := pairKey{srcT, dstT}
	if inProgress[key] {
		// Lazy self-reference: resolved from the cache when actually invoked.
		return func(src, dst reflect.Value) {
			if cp := pairCopierFor(srcT, dstT); cp != nil {
				cp(src, dst)
			}
		}, ""
	}
	inProgress[key] = true
	defer delete(inProgress, key)

	dstPlan := planFor(dstT)
	if dstPlan == nil {
		return nil, fmt.Sprintf("%s carries no mappable fields", dstT)
	}
	src := srcFieldsOf(srcT)

	type slot struct {
		srcIndex []int
		dstIndex []int
		copy     fieldCopier
	}
	var slots []slot
	for _, f := range dstPlan.fields {
		srcIndex, ok := src.byName[f.sourceKey]
		if !ok {
			srcIndex, ok = src.byFold[strings.ToLower(f.sourceKey)]
			if !ok {
				continue // no source field: destination stays zero
			}
		}
		sfT := srcT.FieldByIndex(srcIndex)
		dfT := dstT.FieldByIndex(f.fieldIndex)
		cp, reason := buildValueCopier(sfT.Type, dfT.Type, inProgress)
		if reason != "" {
			// Name the offending field on the Response — the fix site — and
			// carry the inner reason up, so a nested miss reads as a path
			// ("User.DeletedAt: …") rather than a bare type complaint.
			return nil, fmt.Sprintf("field %s (%s → %s): %s", dfT.Name, sfT.Type, dfT.Type, reason)
		}
		slots = append(slots, slot{srcIndex: srcIndex, dstIndex: f.fieldIndex, copy: cp})
	}

	return func(srcV, dstV reflect.Value) {
		for i := range slots {
			s := &slots[i]
			sv, err := srcV.FieldByIndexErr(s.srcIndex)
			if err != nil || !sv.IsValid() {
				continue // nil embedded pointer on the source: field absent
			}
			dv := dstFieldAlloc(dstV, s.dstIndex)
			if !dv.IsValid() || !dv.CanSet() {
				continue
			}
			s.copy(sv, dv)
		}
	}, ""
}

// dstFieldAlloc walks a destination index path allocating nil anonymous
// pointer embeds on the way, mirroring encoding/json's decode behavior.
func dstFieldAlloc(v reflect.Value, index []int) reflect.Value {
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

var (
	jsonMarshalerType   = reflect.TypeOf((*json.Marshaler)(nil)).Elem()
	jsonUnmarshalerType = reflect.TypeOf((*json.Unmarshaler)(nil)).Elem()
	textMarshalerType   = reflect.TypeOf((*encoding.TextMarshaler)(nil)).Elem()
	textUnmarshalerType = reflect.TypeOf((*encoding.TextUnmarshaler)(nil)).Elem()
	domainIDType        = reflect.TypeOf(domain.ID{})
)

// hasCustomCodec reports whether t (or *t) declares its own JSON or text
// codec — the signal that a marshal/unmarshal trip runs type-owned logic a
// structural copy cannot claim to reproduce across DIFFERENT types.
func hasCustomCodec(t reflect.Type) bool {
	pt := reflect.PointerTo(t)
	return t.Implements(jsonMarshalerType) || pt.Implements(jsonMarshalerType) ||
		t.Implements(jsonUnmarshalerType) || pt.Implements(jsonUnmarshalerType) ||
		t.Implements(textMarshalerType) || pt.Implements(textMarshalerType) ||
		t.Implements(textUnmarshalerType) || pt.Implements(textUnmarshalerType)
}

// buildValueCopier compiles the copier for one (source, destination) value
// shape, or reports the pair unsupported. The supported matrix is exactly the
// set of shapes whose JSON round-trip equals a structural copy:
//
//   - identical types (any kind — a marshal/unmarshal of a well-formed value
//     of the SAME type is the identity on the wire);
//   - pointer wrapping/unwrapping on either side (JSON null ↔ nil);
//   - same-family scalar conversions with the round-trip's exact overflow /
//     fraction behavior (mismatch leaves the field zero, never truncates);
//   - domain.ID ↔ string (the ID's declared JSON form IS its string value);
//   - struct → struct via a nested pair plan; slice → slice element-wise.
//
// Everything else — custom codecs across different types, interface sources,
// maps of different types, narrowing shapes — answers unsupported, and the
// whole pair keeps the legacy JSON path.
func buildValueCopier(srcT, dstT reflect.Type, inProgress map[pairKey]bool) (fieldCopier, string) {
	// Identical types: structural copy is wire-identical.
	if srcT == dstT {
		return func(src, dst reflect.Value) { dst.Set(src) }, ""
	}

	// Source pointer: nil renders as JSON null (destination untouched);
	// non-nil recurses on the pointee.
	if srcT.Kind() == reflect.Pointer {
		inner, reason := buildValueCopier(srcT.Elem(), dstT, inProgress)
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

	// Destination pointer: a present source value allocates through.
	if dstT.Kind() == reflect.Pointer {
		inner, reason := buildValueCopier(srcT, dstT.Elem(), inProgress)
		if reason != "" {
			return nil, reason
		}
		return func(src, dst reflect.Value) {
			p := reflect.New(dst.Type().Elem())
			inner(src, p.Elem())
			dst.Set(p)
		}, ""
	}

	// domain.ID ↔ string: the ID's declared JSON form is its plain string
	// value, so the conversion is exact in both directions.
	if srcT == domainIDType && dstT.Kind() == reflect.String && !hasCustomCodec(dstT) {
		return func(src, dst reflect.Value) {
			dst.SetString(src.Interface().(domain.ID).Value())
		}, ""
	}
	if dstT == domainIDType && srcT.Kind() == reflect.String && !hasCustomCodec(srcT) {
		return func(src, dst reflect.Value) {
			dst.Set(reflect.ValueOf(domain.NewID(src.String())))
		}, ""
	}

	// Different types with a type-owned codec on either side: the trip runs
	// logic a structural copy cannot claim to reproduce.
	if hasCustomCodec(srcT) || hasCustomCodec(dstT) {
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
			// float32 → float64 must widen through the SHORTEST DECIMAL — the
			// JSON trip renders the float32 as "3.14159" and parses that into
			// the float64, not the raw binary widening (3.1415901184…).
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
			return buildStructCopier(srcT, dstT, inProgress)
		}
	case reflect.Slice:
		if sk == reflect.Slice {
			elemCp, reason := buildValueCopier(srcT.Elem(), dstT.Elem(), inProgress)
			if reason != "" {
				return nil, reason
			}
			dt := dstT
			return func(src, dst reflect.Value) {
				if src.IsNil() {
					return // JSON null: leave nil (normalizeSlices empties it after)
				}
				n := src.Len()
				out := reflect.MakeSlice(dt, n, n)
				for i := 0; i < n; i++ {
					elemCp(src.Index(i), out.Index(i))
				}
				dst.Set(out)
			}, ""
		}
	}
	return nil, fmt.Sprintf("no direct conversion from %s to %s", srcT, dstT)
}
