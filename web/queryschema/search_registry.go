package queryschema

import (
	"sort"
	"sync"
)

// SearchOptIn is one endpoint's declaration that it accepts `?search=`, paired
// with the view that endpoint reads.
//
// The two halves live in different construction sites and neither can see the
// other: the wrapper reflects the Request DTO at route registration, while the
// ViewDefinition — which owns the index declarations — is contributed by
// ReadableFeature.Views() and collected earlier in the boot. This registry is
// the seam that lets bootstrap put them side by side once both exist.
type SearchOptIn struct {
	// View is the materialized view name the endpoint reads.
	View string
	// Request names the Request DTO, for the diagnostic.
	Request string
}

// searchOptIns accumulates the declarations across route registration, keyed so
// the same endpoint mounted twice (the paged route and its export sibling, or a
// test mounting repeatedly) contributes one entry.
var searchOptIns sync.Map // map[SearchOptIn]struct{}

// RecordSearchOptIn notes that an endpoint reading view accepts `?search=`.
// Called by the read-side wrappers at mount time; bootstrap consumes the set
// after the Mount phase.
func RecordSearchOptIn(view, request string) {
	if view == "" {
		return
	}
	searchOptIns.Store(SearchOptIn{View: view, Request: request}, struct{}{})
}

// SearchOptIns returns every recorded declaration, ordered by (view, request)
// so a diagnostic listing several of them is stable across runs.
func SearchOptIns() []SearchOptIn {
	out := []SearchOptIn{}
	searchOptIns.Range(func(k, _ any) bool {
		out = append(out, k.(SearchOptIn))
		return true
	})
	sort.Slice(out, func(i, j int) bool {
		if out[i].View != out[j].View {
			return out[i].View < out[j].View
		}
		return out[i].Request < out[j].Request
	})
	return out
}

// ResetSearchOptIns clears the registry. Exists for tests that mount routes and
// then assert the boot check in isolation; a running service records once per
// route and never clears.
func ResetSearchOptIns() {
	searchOptIns.Range(func(k, _ any) bool {
		searchOptIns.Delete(k)
		return true
	})
}
