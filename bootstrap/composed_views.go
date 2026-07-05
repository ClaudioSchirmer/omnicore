package bootstrap

import (
	"fmt"

	"github.com/ClaudioSchirmer/omnicore/infra/db/query"
)

// ComposingFeature is the opt-in for read-time composition. Bootstrap
// collects the ComposedViews() declared by the ComposingFeatures, validates
// them (query.ValidateComposedViews — fatal boot on any invalid declaration)
// and wraps the framework ViewReader with the composed decorator, so the
// composed names resolve through the same queries.ViewReader port every
// handler, GraphQL field and export route already consumes.
//
// Mirrors the role ReadableFeature plays for materialized views: opt-in via
// type assertion; features that declare no composition pay zero cost. A
// composed view is NOT a view like the others — it is never materialized,
// never synced, never rebuilt: no collection, no Version, no schema-evolution
// entry, no SyncEngine involvement. It is a read-time orchestration over
// views that already exist.
type ComposingFeature interface {
	Feature
	ComposedViews() []*query.ComposedViewDefinition
}

// collectComposedViews aggregates composed views from every ComposingFeature,
// rejecting composed-name collisions between features (the structural
// validation against views/upstream collections runs later in
// query.ValidateComposedViews, once both are resolved).
func collectComposedViews(features []Feature) ([]*query.ComposedViewDefinition, error) {
	var out []*query.ComposedViewDefinition
	seen := map[string]string{}
	for _, f := range features {
		cf, ok := f.(ComposingFeature)
		if !ok {
			continue
		}
		for _, c := range cf.ComposedViews() {
			if c == nil {
				continue
			}
			owner := fmt.Sprintf("%T", f)
			if prev, dup := seen[c.Name()]; dup {
				return nil, fmt.Errorf(
					"bootstrap: composed view %q declared by both %s and %s — composed names are service-unique",
					c.Name(), prev, owner)
			}
			seen[c.Name()] = owner
			out = append(out, c)
		}
	}
	return out, nil
}

// upstreamCollectionSet indexes the resolved subscriptions by their local
// Mongo collection — the set an external composed-view leg must resolve into
// (a leg never reads another service's live storage; materialize first).
func upstreamCollectionSet(subs []UpstreamSubscription) map[string]bool {
	out := make(map[string]bool, len(subs))
	for _, s := range subs {
		out[s.Collection] = true
	}
	return out
}
