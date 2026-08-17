package queryschema

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// The Result↔Response alignment guards. The read side's single wire
// authority is the Response DTO, and the Response maps from the
// application-layer Result BY FIELD NAME (the framework's generic
// TResult→TResp mapper reads result.<Name> for every response field
// <Name>). These boot guards make the alignment impossible to ship broken:
//
//   - every Response field must have a same-named Result field (recursively
//     through nested structs and slices of structs);
//   - the Result must stay application-pure — no json wire tags (the
//     three-name model: wire names live only in web/);
//   - when the endpoint opts into `?fields=`, the Result must tolerate the
//     sparse fill (pointer/slice/map fields), mirroring the Response's
//     sparse-render contract.

// ValidateResultAlignment checks that respType (the wire Response) is fully
// backed by resultType (the application Result): every exported,
// non-`json:"-"` Response field — recursively — must have a same-named
// field on the Result, with struct/slice shapes recursed pairwise. It also
// rejects json tags on the Result itself (wire naming belongs to the
// Response). Returns human-readable violation lines; empty means aligned.
func ValidateResultAlignment(resultType, respType reflect.Type) []string {
	var errs []string
	resultType = derefType(resultType)
	respType = derefType(respType)
	if respType.Kind() != reflect.Struct {
		return errs
	}
	if resultType.Kind() != reflect.Struct {
		errs = append(errs, fmt.Sprintf("result type %s is not a struct", resultType.String()))
		return errs
	}
	walkResultTags(resultType, "", &errs, map[reflect.Type]bool{})
	walkAlignment(resultType, respType, "", &errs, map[reflect.Type]bool{})
	return errs
}

// FormatResultAlignmentGuard assembles the boot-panic diagnostic for a
// Result↔Response misalignment. Boot panic is the framework's posture for
// structural contract violations — fail loud at construction.
func FormatResultAlignmentGuard(resultType, respType reflect.Type, errs []string) string {
	sortedErrs := append([]string(nil), errs...)
	sort.Strings(sortedErrs)
	var b strings.Builder
	fmt.Fprintf(&b, "[result] %s is not backed by %s:\n", respType.String(), resultType.String())
	for _, line := range sortedErrs {
		fmt.Fprintf(&b, "  - %s\n", line)
	}
	b.WriteString("Every Response field maps from the same-named Result field (the generic TResult→TResp mapper is name-based), and the Result carries no wire tags — declare the field on the Result, or drop it from the Response.")
	return b.String()
}

// ValidateComputedSources checks every `computed:"A,B"` tag the Response
// declares: each named source must resolve to a real field on the Result at
// the same level, and must not itself be computed (a computed field carries
// no column, so it cannot feed the push-down of another one). The sources are
// deliberately NOT required on the Response — a source that only exists on
// the Result is read, feeds FromQueryResult, and never reaches the wire.
//
// Returns human-readable violation lines; empty means every tag resolves.
func ValidateComputedSources(resultType, respType reflect.Type) []string {
	var errs []string
	resultType = derefType(resultType)
	respType = derefType(respType)
	if respType.Kind() != reflect.Struct || resultType.Kind() != reflect.Struct {
		return errs
	}
	schema := ExtractProjectionSchema(respType)
	if len(schema.Computed) == 0 {
		return errs
	}
	// Sort so the diagnostic is stable across runs (map iteration order).
	wirePaths := make([]string, 0, len(schema.Computed))
	for wirePath := range schema.Computed {
		wirePaths = append(wirePaths, wirePath)
	}
	sort.Strings(wirePaths)
	for _, wirePath := range wirePaths {
		for _, src := range schema.Computed[wirePath] {
			f, found := resultFieldByPath(resultType, src)
			if !found {
				errs = append(errs, fmt.Sprintf("%s: computed source %q names no field on %s", wirePath, src, resultType.String()))
				continue
			}
			if _, tagged := f.Tag.Lookup(ComputedTag); tagged {
				errs = append(errs, fmt.Sprintf("%s: computed source %q is itself computed — a source must be a stored field", wirePath, src))
			}
		}
		// The computed field itself must exist on the Result: FromQueryResult
		// has nowhere to write the derived value otherwise. (The Response→Result
		// alignment guard already covers it; this keeps the diagnostic local.)
		if goPath, ok := schema.Paths[wirePath]; ok {
			if _, found := resultFieldByPath(resultType, goPath); !found {
				errs = append(errs, fmt.Sprintf("%s: the computed field itself names no field on %s", wirePath, resultType.String()))
			}
		}
	}
	return errs
}

// ValidateComputedFilters rejects a Request DTO that declares a FILTER over a
// computed field. Filtering happens in the store, and a computed field has no
// column there — the predicate could never be evaluated, so the declaration is
// a wiring error the consumer must fix, not a request the client can get
// wrong. Boot-failing here is the framework's posture for structural contract
// violations: it is impossible to ship a filter that would 400 on every call.
//
// The match is by Go field path: a filter leaf whose DocPath equals a computed
// field's Go path (or lives under one) is refused.
func ValidateComputedFilters(reqSchema *RequestSchema, respType reflect.Type) []string {
	var errs []string
	respType = derefType(respType)
	if reqSchema == nil || respType.Kind() != reflect.Struct {
		return errs
	}
	schema := ExtractProjectionSchema(respType)
	if len(schema.Computed) == 0 {
		return errs
	}
	// Go path of every computed field, for the leaf comparison.
	computedGoPaths := make(map[string]string, len(schema.Computed))
	for wirePath := range schema.Computed {
		if goPath, ok := schema.Paths[wirePath]; ok {
			computedGoPaths[goPath] = wirePath
		}
	}
	wirePaths := make([]string, 0, len(reqSchema.Filters))
	for wirePath := range reqSchema.Filters {
		wirePaths = append(wirePaths, wirePath)
	}
	sort.Strings(wirePaths)
	for _, wirePath := range wirePaths {
		docPath := reqSchema.Filters[wirePath].DocPath
		if computedWire, isComputed := computedGoPaths[docPath]; isComputed {
			errs = append(errs, fmt.Sprintf(
				"filter %q resolves to %q, which the Response declares as computed (%q) — a computed field has no column to filter on",
				wirePath, docPath, computedWire))
		}
	}
	return errs
}

// FormatComputedFiltersGuard assembles the boot-panic diagnostic for a filter
// declared over a computed field.
func FormatComputedFiltersGuard(reqType, respType reflect.Type, errs []string) string {
	sortedErrs := append([]string(nil), errs...)
	sort.Strings(sortedErrs)
	var b strings.Builder
	fmt.Fprintf(&b, "[computed] %s declares filters over computed fields of %s:\n", reqType.String(), respType.String())
	for _, line := range sortedErrs {
		fmt.Fprintf(&b, "  - %s\n", line)
	}
	b.WriteString("Filtering is evaluated in the store; a computed field is derived after the read by FromQueryResult. Filter one of its sources instead, or materialize the value as a real column on the view.")
	return b.String()
}

// FormatComputedSourcesGuard assembles the boot-panic diagnostic for a
// `computed:` tag whose sources do not resolve.
func FormatComputedSourcesGuard(resultType, respType reflect.Type, errs []string) string {
	sortedErrs := append([]string(nil), errs...)
	sort.Strings(sortedErrs)
	var b strings.Builder
	fmt.Fprintf(&b, "[computed] %s declares computed fields that %s cannot feed:\n", respType.String(), resultType.String())
	for _, line := range sortedErrs {
		fmt.Fprintf(&b, "  - %s\n", line)
	}
	b.WriteString("A `computed:\"A,B\"` tag names Result fields pushed down in place of the computed field (which has no column). Declare the sources on the Result — they need not appear on the Response.")
	return b.String()
}

// resultFieldByPath walks a dotted Go field path through the Result type,
// descending into structs and slice element types, and returns the leaf field.
func resultFieldByPath(t reflect.Type, path string) (reflect.StructField, bool) {
	segments := strings.Split(path, ".")
	cur := derefType(t)
	var leaf reflect.StructField
	for _, seg := range segments {
		if cur.Kind() == reflect.Slice {
			cur = derefType(cur.Elem())
		}
		if cur.Kind() != reflect.Struct {
			return reflect.StructField{}, false
		}
		f, found := cur.FieldByName(seg)
		if !found || !f.IsExported() {
			return reflect.StructField{}, false
		}
		leaf = f
		cur = derefType(f.Type)
	}
	return leaf, true
}

// ValidateFieldsResult enforces the Result-side sparse contract for
// endpoints that opt into `?fields=`: every exported field (recursively)
// must be a pointer, slice or map so an absent (projected-out) document key
// leaves a distinguishable zero. The Response-side twin is
// ValidateFieldsResponse (which additionally requires ,omitempty — a wire
// concern the tagless Result does not carry).
func ValidateFieldsResult(t reflect.Type) []string {
	var errs []string
	walkResultSparse(derefType(t), "", &errs, map[reflect.Type]bool{})
	return errs
}

// FormatFieldsResultGuard assembles the boot-panic diagnostic for a Result
// that opts into `?fields=` without tolerating the sparse fill.
func FormatFieldsResultGuard(t reflect.Type, errs []string) string {
	sortedErrs := append([]string(nil), errs...)
	sort.Strings(sortedErrs)
	var b strings.Builder
	fmt.Fprintf(&b, "[fields] the endpoint declares query:\"fields\" but the Result shape %s violates the sparse-fill contract:\n", t.String())
	for _, line := range sortedErrs {
		fmt.Fprintf(&b, "  - %s\n", line)
	}
	b.WriteString("Every exported Result field (recursively, including nested struct and slice element types) must be *T, a slice or a map — `?fields=` reduces what the reader returns, and the Result must distinguish absent from zero.")
	return b.String()
}

func derefType(t reflect.Type) reflect.Type {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

// walkResultTags rejects json tags anywhere on the Result tree — wire
// naming is the Response's job; a tagged Result is a layering violation.
func walkResultTags(t reflect.Type, path string, errs *[]string, seen map[reflect.Type]bool) {
	if t.Kind() != reflect.Struct || seen[t] {
		return
	}
	seen[t] = true
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.Anonymous {
			ft := derefType(f.Type)
			if ft.Kind() == reflect.Struct {
				walkResultTags(ft, path, errs, seen)
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		fieldPath := joinPath(path, f.Name)
		if _, tagged := f.Tag.Lookup("json"); tagged {
			*errs = append(*errs, fmt.Sprintf("%s: result field carries a json tag — wire naming belongs to the Response DTO", fieldPath))
		}
		ft := derefType(f.Type)
		switch ft.Kind() {
		case reflect.Struct:
			walkResultTags(ft, fieldPath, errs, seen)
		case reflect.Slice:
			elem := derefType(ft.Elem())
			if elem.Kind() == reflect.Struct {
				walkResultTags(elem, fieldPath, errs, seen)
			}
		}
	}
}

func walkAlignment(resultType, respType reflect.Type, path string, errs *[]string, seen map[reflect.Type]bool) {
	if seen[respType] {
		return
	}
	seen[respType] = true
	for i := 0; i < respType.NumField(); i++ {
		f := respType.Field(i)
		if f.Anonymous {
			ft := derefType(f.Type)
			if ft.Kind() == reflect.Struct {
				walkAlignment(resultType, ft, path, errs, seen)
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		if f.Tag.Get("json") == "-" {
			continue
		}
		fieldPath := joinPath(path, f.Name)
		rf, found := resultType.FieldByName(f.Name)
		if !found || !rf.IsExported() {
			*errs = append(*errs, fmt.Sprintf("%s: no same-named field on %s", fieldPath, resultType.String()))
			continue
		}
		respFT := derefType(f.Type)
		resFT := derefType(rf.Type)
		// Identical (deref'd) types are aligned by construction — covers
		// time.Time, value objects and shared leaf types without recursing
		// into their unexported internals.
		if respFT == resFT {
			continue
		}
		switch respFT.Kind() {
		case reflect.Struct:
			if resFT.Kind() != reflect.Struct {
				*errs = append(*errs, fmt.Sprintf("%s: response declares a struct but the result field is %s", fieldPath, resFT.Kind()))
				continue
			}
			walkAlignment(resFT, respFT, fieldPath, errs, seen)
		case reflect.Slice:
			if resFT.Kind() != reflect.Slice {
				*errs = append(*errs, fmt.Sprintf("%s: response declares a slice but the result field is %s", fieldPath, resFT.Kind()))
				continue
			}
			respElem := derefType(respFT.Elem())
			resElem := derefType(resFT.Elem())
			if respElem.Kind() == reflect.Struct && respElem != resElem {
				if resElem.Kind() != reflect.Struct {
					*errs = append(*errs, fmt.Sprintf("%s: response slice carries structs but the result slice carries %s", fieldPath, resElem.Kind()))
					continue
				}
				walkAlignment(resElem, respElem, fieldPath, errs, seen)
			}
		}
	}
}

func walkResultSparse(t reflect.Type, path string, errs *[]string, seen map[reflect.Type]bool) {
	if t.Kind() != reflect.Struct || seen[t] {
		return
	}
	seen[t] = true
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.Anonymous {
			ft := derefType(f.Type)
			if ft.Kind() == reflect.Struct {
				walkResultSparse(ft, path, errs, seen)
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		fieldPath := joinPath(path, f.Name)
		switch f.Type.Kind() {
		case reflect.Pointer:
			elem := derefType(f.Type)
			if elem.Kind() == reflect.Struct {
				walkResultSparse(elem, fieldPath, errs, seen)
			}
		case reflect.Slice:
			elem := derefType(f.Type.Elem())
			if elem.Kind() == reflect.Struct {
				walkResultSparse(elem, fieldPath, errs, seen)
			}
		case reflect.Map:
			// Absent map stays nil — distinguishable, accepted.
		default:
			*errs = append(*errs, fmt.Sprintf("%s: must be *%s, a slice or a map (got %s)", fieldPath, f.Type.String(), f.Type.Kind()))
		}
	}
}
