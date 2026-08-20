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

// ViewNamer is the optional seat a query handler exposes so a surface can pair
// a Request-DTO declaration with the view that has to honor it. The canonical
// handlers implement it; a hand-written pipeline.Handler need not, and is
// simply not covered by the boot check.
type ViewNamer interface{ ViewName() string }

// RecordSearchDeclaration notes an endpoint that accepts `?search=` together
// with the view it reads, when both are knowable at registration. Every read
// surface calls it from its own registration site — REST's paged and export
// wrappers, the GraphQL connection field, the gRPC list procedure — because
// the mismatch it guards against belongs to the (DTO, view) pair, not to any
// one wire: a service that exposes an entity ONLY over gRPC has the same
// unserveable control as a REST one.
//
// requestType names the Request DTO for the diagnostic; h is the application
// handler, consulted for [ViewNamer].
func RecordSearchDeclaration(schema *RequestSchema, requestType string, h any) {
	if schema == nil || !schema.Reserved[KeySearch] {
		return
	}
	if namer, ok := h.(ViewNamer); ok {
		RecordSearchOptIn(namer.ViewName(), requestType)
	}
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

// ResetSearchOptIns clears the registry.
//
// The boot check calls it once it has consumed the set, which is what keeps a
// process-wide registry honest across composition roots: each boot verifies
// exactly the declarations recorded since the previous one, so a view name two
// apps happen to share cannot make one app fail the boot naming the other's
// Request DTO. Tests that mount routes and then assert the check in isolation
// call it directly.
func ResetSearchOptIns() {
	searchOptIns.Range(func(k, _ any) bool {
		searchOptIns.Delete(k)
		return true
	})
}
