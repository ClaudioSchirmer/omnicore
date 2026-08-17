package queries

import (
	"encoding/json"
	"reflect"
	"sync"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/domain"
)

// The read side's Result anatomy, mirroring the write side: a query handler
// produces a typed application-layer Result (TResult — pure data, no wire
// tags, the twin of a command's Result struct), never a raw document. The
// framework fills TResult from the canonical Go-keyed view document
// (ResultFromDoc), the Query's FromQueryResult hook adjusts or passes it through,
// and the web layer maps it to the wire Response — so every transport
// surface consumes the same typed value and a field absent from TResult can
// never reach any wire.

// PageOf is the typed page the read handlers return — the application-layer
// twin of Page, carrying []TResult instead of raw documents. The envelope
// fields mirror Page one to one (see Page for the Relay framing contract,
// the edge-cursor emission rule, ItemCursors and the Projection echo).
type PageOf[TResult any] struct {
	Items           []TResult
	HasNextPage     bool
	HasPreviousPage bool
	StartCursor     string
	EndCursor       string
	TotalCount      int64
	OnlyTotal       bool

	// ItemCursors is positionally aligned with Items — see Page.ItemCursors.
	ItemCursors []string

	// Projection is the effective read projection echo — see Page.Projection.
	Projection map[string]int
}

// PageOfFrom converts a reader-level Page into the typed PageOf by filling
// one TResult per document (ResultFromDoc) and passing each through fill —
// the Query's FromQueryResult hook. The envelope (cursors, totals, projection
// echo, only-total mode) is copied verbatim. Used by the framework's
// FindByParamsQueryHandler and available to manual handlers that read a
// Page from the ViewReader themselves.
func PageOfFrom[TResult any](page Page, fill func(TResult) (TResult, error)) (PageOf[TResult], error) {
	out := PageOf[TResult]{
		HasNextPage:     page.HasNextPage,
		HasPreviousPage: page.HasPreviousPage,
		StartCursor:     page.StartCursor,
		EndCursor:       page.EndCursor,
		TotalCount:      page.TotalCount,
		OnlyTotal:       page.OnlyTotal,
		ItemCursors:     page.ItemCursors,
		Projection:      page.Projection,
	}
	if page.OnlyTotal {
		return out, nil
	}
	out.Items = make([]TResult, 0, len(page.Items))
	for _, doc := range page.Items {
		r := ResultFromDoc[TResult](doc)
		r, err := fill(r)
		if err != nil {
			return PageOf[TResult]{}, err
		}
		out.Items = append(out.Items, r)
	}
	return out, nil
}

// FromQueryResultFiller adapts a Query's FromQueryResult hook into the per-item fill
// function PageOfFrom consumes, binding the request-scoped AppContext.
func FromQueryResultFiller[TResult any](ctx *configuration.AppContext, q interface {
	FromQueryResult(ctx *configuration.AppContext, r TResult) (TResult, error)
}) func(TResult) (TResult, error) {
	return func(r TResult) (TResult, error) { return q.FromQueryResult(ctx, r) }
}

// ResultFromDoc fills a TResult from the canonical Go-keyed view document.
// Both storage backends (the Mongo projection and the relational
// RelationalSource twin) normalize into the same document shape —
// map[string]any keyed by Go field name — before any consumer sees it, so
// this fill is backend-agnostic by construction.
//
// TResult is an application-layer Result struct: exported fields named
// exactly like the view document's Go keys, NO wire tags (json tags belong
// to the web layer's Response DTOs — the three-name model). Matching is by
// field name, recursively through nested structs, slices of structs and
// pointer fields.
//
// Normalizations applied (the read-side semantic pass, shared by every
// transport surface because it runs before any of them):
//   - Top-level "ID" ← _id when "ID" is absent and "_id" is a string.
//   - Nil slice fields → empty typed slice at every level.
//   - EnumValueObject fields carrying an out-of-set value converge to the
//     Unknown sentinel — parity with the write-side entity reconstruction,
//     so a stale/tampered stored value never surfaces as a phantom member.
func ResultFromDoc[TResult any](doc map[string]any) TResult {
	var out TResult
	if raw, err := json.Marshal(applyIDFallback(doc)); err == nil {
		_ = json.Unmarshal(raw, &out)
	}
	plan := resultPlanFor(reflect.TypeOf(out))
	normalizeResultSlices(reflect.ValueOf(&out).Elem(), plan)
	convergeResultEnums(reflect.ValueOf(&out).Elem(), plan)
	return out
}

// applyIDFallback returns a doc with the Go ID field "ID" ← _id when "ID"
// is absent and "_id" is a string. The reader maps the ID column to the Go
// field "ID"; this covers external/mirror schemas where the doc carries only
// the store id. Top-level only. Does not mutate the input; allocates a
// shallow copy only when a rewrite is needed.
func applyIDFallback(doc map[string]any) map[string]any {
	if doc == nil {
		return doc
	}
	if _, hasID := doc["ID"]; hasID {
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
	patched["ID"] = s
	return patched
}

// resultFieldKind classifies a Result field for the normalization walkers.
type resultFieldKind int

const (
	rfkScalar resultFieldKind = iota
	rfkStruct
	rfkSlice
	rfkSliceOfStruct
)

// resultFieldEntry is one rule in a Result's normalization plan.
type resultFieldEntry struct {
	fieldIndex []int
	kind       resultFieldKind
	nested     *resultPlan
	// enumType is non-nil when the field is an EnumValueObject (deref'd of
	// any pointer) — convergeResultEnums maps an out-of-set value to Unknown.
	enumType reflect.Type
}

// resultPlan is the cached normalization plan for one TResult type.
type resultPlan struct {
	fields []resultFieldEntry
}

var resultPlanCache sync.Map // map[reflect.Type]*resultPlan

// resultPlanFor returns (and memoizes) the plan for t. Pointer types are
// dereferenced; non-struct types yield nil (normalization degrades to a
// no-op).
func resultPlanFor(t reflect.Type) *resultPlan {
	if t == nil {
		return nil
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}
	if v, ok := resultPlanCache.Load(t); ok {
		return v.(*resultPlan)
	}
	plan := &resultPlan{}
	// Store BEFORE recursing so self-referential types terminate.
	resultPlanCache.Store(t, plan)
	buildResultPlan(plan, t, nil)
	return plan
}

func buildResultPlan(plan *resultPlan, t reflect.Type, basePath []int) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		path := append(append([]int{}, basePath...), i)

		// Anonymous struct embedded — promote its fields up to this level
		// (encoding/json convention). Runs BEFORE the IsExported check so
		// unexported anonymous structs still surface their exported fields.
		if f.Anonymous {
			ft := f.Type
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				buildResultPlan(plan, ft, path)
				continue
			}
		}
		if !f.IsExported() {
			continue
		}

		entry := resultFieldEntry{fieldIndex: path, kind: rfkScalar}

		ft := f.Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if domain.IsEnumValueObject(reflect.Zero(ft).Interface()) {
			entry.enumType = ft
		}
		switch ft.Kind() {
		case reflect.Struct:
			entry.kind = rfkStruct
			entry.nested = resultPlanFor(ft)
		case reflect.Slice:
			elem := ft.Elem()
			for elem.Kind() == reflect.Pointer {
				elem = elem.Elem()
			}
			if elem.Kind() == reflect.Struct {
				entry.kind = rfkSliceOfStruct
				entry.nested = resultPlanFor(elem)
			} else {
				entry.kind = rfkSlice
			}
		}

		plan.fields = append(plan.fields, entry)
	}
}

// normalizeResultSlices walks v per plan and replaces nil slice fields with
// empty typed slices, recursively — output is consistent regardless of the
// slice's depth.
func normalizeResultSlices(v reflect.Value, plan *resultPlan) {
	if plan == nil || !v.IsValid() {
		return
	}
	for _, f := range plan.fields {
		field := v.FieldByIndex(f.fieldIndex)
		if !field.IsValid() {
			continue
		}
		switch f.kind {
		case rfkSlice:
			if field.Kind() == reflect.Slice && field.IsNil() && field.CanSet() {
				field.Set(reflect.MakeSlice(field.Type(), 0, 0))
			}
		case rfkSliceOfStruct:
			if field.Kind() == reflect.Slice {
				if field.IsNil() && field.CanSet() {
					field.Set(reflect.MakeSlice(field.Type(), 0, 0))
				}
				if f.nested != nil {
					for i := 0; i < field.Len(); i++ {
						elem := derefResultValue(field.Index(i))
						if elem.IsValid() && elem.Kind() == reflect.Struct {
							normalizeResultSlices(elem, f.nested)
						}
					}
				}
			}
		case rfkStruct:
			elem := derefResultValue(field)
			if elem.IsValid() && elem.Kind() == reflect.Struct && f.nested != nil {
				normalizeResultSlices(elem, f.nested)
			}
		}
	}
}

// convergeResultEnums walks v per plan and maps every EnumValueObject field
// whose populated value is out of its declared member set to the Unknown
// sentinel. Recurses into nested structs and slice-of-struct elements.
func convergeResultEnums(v reflect.Value, plan *resultPlan) {
	if plan == nil || !v.IsValid() {
		return
	}
	for _, f := range plan.fields {
		field := v.FieldByIndex(f.fieldIndex)
		if !field.IsValid() {
			continue
		}
		if f.enumType != nil {
			convergeResultEnumField(field)
			continue
		}
		switch f.kind {
		case rfkStruct:
			elem := derefResultValue(field)
			if elem.IsValid() && elem.Kind() == reflect.Struct && f.nested != nil {
				convergeResultEnums(elem, f.nested)
			}
		case rfkSliceOfStruct:
			if field.Kind() == reflect.Slice && f.nested != nil {
				for i := 0; i < field.Len(); i++ {
					elem := derefResultValue(field.Index(i))
					if elem.IsValid() && elem.Kind() == reflect.Struct {
						convergeResultEnums(elem, f.nested)
					}
				}
			}
		}
	}
}

// convergeResultEnumField converges one populated enum field (a nil pointer
// is left untouched — absence, not Unknown).
func convergeResultEnumField(field reflect.Value) {
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

// derefResultValue dereferences pointers, yielding an invalid Value on nil.
func derefResultValue(v reflect.Value) reflect.Value {
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return reflect.Value{}
		}
		v = v.Elem()
	}
	return v
}
