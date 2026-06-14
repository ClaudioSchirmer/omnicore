package openapi

import (
	"strings"
	"sync"
)

// Operation is one row in the Registry: an HTTP route the framework will
// document. Method + Path are normalized (METHOD uppercased, Path verbatim
// after JoinPath resolved the group's prefix). Exactly one of Spec / Raw
// is populated per operation — Mount uses Spec + Doc; MountRaw uses Raw.
// Doc on a Raw operation is left zero (RawSpec carries its own prose).
//
// resolvedRequestExamples / resolvedResponseExamples carry the JSON bytes
// produced by Mount / MountRaw when the consumer declared examples on
// this route. Pre-computing here means the renderer never re-marshals on
// every Build call (the Spec caches anyway, but keeping the encode-once
// invariant lets `runtime/race` stay quiet under spec rebuilds in tests).
// Both maps are nil when the consumer declared no examples.
type Operation struct {
	Method string
	Path   string
	Spec   RouteSpec
	Doc    Doc
	Raw    *RawSpec

	resolvedRequestExamples  map[string]rawExample
	resolvedResponseExamples map[int]map[string]rawExample
}

// opKey is the dedup index inside the Registry — two routes that share
// method + path collapse to the last write. Distinct from the path
// duplicate-mount that Fiber itself rejects at boot.
type opKey struct {
	Method string
	Path   string
}

// Registry collects every Operation the consumer registered via Mount /
// MountRaw and owns the Components schema-dedup pool the spec assembler
// later folds into /openapi.json. A service that does not enable OpenAPI
// passes a nil registry to Mount; the wrapper short-circuits and Registry
// methods are never reached, so a nil receiver is fine in that path.
//
// All exported methods are safe for concurrent use — Mount may run from
// multiple Features in parallel during bootstrap (rare, but the
// guarantee keeps the contract simple).
type Registry struct {
	mu         sync.Mutex
	operations map[opKey]Operation
	order      []opKey
	components *Components
}

// NewRegistry constructs a Registry with an empty Components pool. Pass
// the same Registry to every Mount call across the service so each
// feature's routes land in the same document and each named struct lands
// once in `components/schemas`.
func NewRegistry() *Registry {
	return &Registry{
		operations: map[opKey]Operation{},
		components: NewComponents(),
	}
}

// Components exposes the dedup pool the spec assembler reads. The
// Schema generator that walks RequestType / ResponseType during Mount
// writes named-struct definitions here; consumers normally do not touch
// this directly.
func (r *Registry) Components() *Components { return r.components }

// Operations returns the registered routes in insertion order. The order
// is preserved so the generated /openapi.json mirrors the order the
// service wires its routes — useful for diff-friendly snapshots and for
// the Swagger UI's sidebar layout.
func (r *Registry) Operations() []Operation {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Operation, 0, len(r.order))
	for _, k := range r.order {
		out = append(out, r.operations[k])
	}
	return out
}

// add inserts (or overwrites) op under its (METHOD, Path) key. Last write
// wins — two Mount calls on the same route collapse to one entry. The
// duplicate is also a Fiber-side error at boot, so this branch in
// practice only fires when a service rewires its routes in a test.
func (r *Registry) add(op Operation) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := opKey{Method: strings.ToUpper(op.Method), Path: op.Path}
	if _, exists := r.operations[key]; !exists {
		r.order = append(r.order, key)
	}
	r.operations[key] = op
}
