package graphql

import (
	"bytes"
	"encoding/json"
	"reflect"

	"github.com/ClaudioSchirmer/omnicore/application/queries"
)

// pageToConnection maps a typed queries.PageOf into the Relay connection
// value tree (plain maps/slices keyed by WIRE names, so the executor's
// selection trim is a pure key pick). Each item is projected through the
// SAME response seat every other surface uses — the Query's FromQueryResult ran in
// the application handler, project is the web-side TResult→R mapping — and
// the projected Response value renders to its wire form via its json tags.
// Each edge's cursor comes from PageOf.ItemCursors — the per-row keyset
// cursor the reader emits; the page edges feed pageInfo's start/end.
func pageToConnection[TResult any, R any](page queries.PageOf[TResult], project func(TResult) R) map[string]any {
	edges := make([]any, len(page.Items))
	for i, r := range page.Items {
		cursor := ""
		if i < len(page.ItemCursors) {
			cursor = page.ItemCursors[i]
		}
		edges[i] = map[string]any{
			"node":   wireValueOf(project(r)),
			"cursor": cursor,
		}
	}
	return map[string]any{
		"edges": edges,
		"pageInfo": map[string]any{
			"hasNextPage":     page.HasNextPage,
			"hasPreviousPage": page.HasPreviousPage,
			"startCursor":     emptyToNil(page.StartCursor),
			"endCursor":       emptyToNil(page.EndCursor),
		},
		"totalCount": page.TotalCount,
	}
}

func emptyToNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// wireValueOf renders a projected Response value as its wire-named nested
// map — the Response's json tags ARE the wire contract, so a plain JSON
// round-trip produces exactly the tree the executor trims by selection.
// Numbers decode as json.Number so 64-bit integers survive without float64
// truncation; sparse fields (nil + omitempty) are absent, which the
// executor resolves to null when selected.
func wireValueOf(v any) map[string]any {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var out map[string]any
	if err := dec.Decode(&out); err != nil {
		return nil
	}
	return out
}

// scalarStruct reports whether a struct type is one of the well-known leaf
// types (time.Time / uuid.UUID / domain.ID) that must NOT be walked as an
// object — its value is a scalar on the wire. Consumed by the SDL builder's
// type reflection.
func scalarStruct(t reflect.Type) bool {
	return t == timeType || t == uuidType || t == domainIDTyp
}
