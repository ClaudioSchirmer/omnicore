package responses

import (
	"fmt"
	"reflect"
	"strings"
	"sync"

	"github.com/ClaudioSchirmer/omnicore/internal/fieldcopy"
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

// fieldCopier is the shared copier type (internal/fieldcopy).
type fieldCopier = fieldcopy.Copier

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

// buildValueCopier delegates to the shared engine (internal/fieldcopy), which
// owns the "can this value be assigned to that field" question for BOTH Auto
// seats. Only the struct walk differs between them — here the RESPONSE drives,
// on the request side the REQUEST does — so the nested builder is ours.
func buildValueCopier(srcT, dstT reflect.Type, inProgress map[pairKey]bool) (fieldCopier, string) {
	return fieldcopy.ValueCopier(srcT, dstT, func(s, d reflect.Type) (fieldcopy.Copier, string) {
		return buildStructCopier(s, d, inProgress)
	})
}
