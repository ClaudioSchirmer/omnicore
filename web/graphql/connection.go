package graphql

import (
	"reflect"

	"github.com/ClaudioSchirmer/omnicore/application/queries"
)

// pageToConnection maps a queries.Page into the Relay connection value tree
// (plain maps/slices keyed by WIRE names, so the executor's selection trim is a
// pure key pick). Each edge's cursor comes from Page.ItemCursors — the per-row
// keyset cursor the reader emits; the page edges feed pageInfo's start/end.
func pageToConnection(page queries.Page, respType reflect.Type) map[string]any {
	edges := make([]any, len(page.Items))
	for i, doc := range page.Items {
		cursor := ""
		if i < len(page.ItemCursors) {
			cursor = page.ItemCursors[i]
		}
		edges[i] = map[string]any{
			"node":   translateToWire(doc, respType),
			"cursor": cursor,
		}
	}
	return map[string]any{
		"edges": edges,
		"pageInfo": map[string]any{
			"hasNextPage":     page.HasNext,
			"hasPreviousPage": page.HasPrev,
			"startCursor":     emptyToNil(page.PrevCursor),
			"endCursor":       emptyToNil(page.NextCursor),
		},
		"totalCount": page.Total,
	}
}

func emptyToNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// translateToWire reshapes a Go-field-keyed view document into a wire-named
// nested map, driven by the Response DTO type — the same wire↔Go contract the
// REST projector honors, expressed generically so the executor can trim it by
// selection. A field absent from the doc yields nil (sparse responses); nested
// structs and slices of structs recurse.
func translateToWire(doc map[string]any, respType reflect.Type) map[string]any {
	out := map[string]any{}
	for _, f := range exportedJSONFields(respType) {
		out[f.wire] = translateValue(doc[f.field.Name], f.field.Type)
	}
	return out
}

func translateValue(v any, t reflect.Type) any {
	if v == nil {
		return nil
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Struct:
		if scalarStruct(t) {
			return v // time.Time / uuid / domain.ID — leaf value, passthrough
		}
		if m, ok := v.(map[string]any); ok {
			return translateToWire(m, t)
		}
		return v
	case reflect.Slice, reflect.Array:
		elem := t.Elem()
		for elem.Kind() == reflect.Pointer {
			elem = elem.Elem()
		}
		if elem.Kind() != reflect.Struct || scalarStruct(elem) {
			return v // scalar / well-known element — passthrough
		}
		return translateSlice(v, elem)
	default:
		return v
	}
}

// translateSlice reshapes a slice of struct documents. Mongo decodes nested
// arrays as []map[string]any or []any of maps; both are handled.
func translateSlice(v any, elem reflect.Type) any {
	switch list := v.(type) {
	case []map[string]any:
		out := make([]any, len(list))
		for i, m := range list {
			out[i] = translateToWire(m, elem)
		}
		return out
	case []any:
		out := make([]any, len(list))
		for i, e := range list {
			if m, ok := e.(map[string]any); ok {
				out[i] = translateToWire(m, elem)
			} else {
				out[i] = e
			}
		}
		return out
	default:
		return v
	}
}

// scalarStruct reports whether a struct type is one of the well-known leaf
// types (time.Time / uuid.UUID / domain.ID) that must NOT be walked as an
// object — its value is a scalar on the wire.
func scalarStruct(t reflect.Type) bool {
	return t == timeType || t == uuidType || t == domainIDTyp
}
