package bootstrap

import (
	"fmt"

	"github.com/ClaudioSchirmer/omnicore/infra/db/query"
)

// RelationalReadableFeature is the opt-in for read models served straight from
// the relational backend. Bootstrap collects the RelationalViews() declared by
// every feature that satisfies it and registers them on the read seam, exactly
// as ReadableFeature's Views() are registered on the projection reader.
//
// Mirrors the role ComposingFeature plays for read-time composition: opt-in via
// type assertion, features that declare none pay zero cost. A relational read
// model is NOT a view like the Mongo ones — it is never materialized, never
// synced, never rebuilt: no collection, no Version, no schema-evolution entry, no
// SyncEngine involvement. The projection machinery takes *query.ViewDefinition
// concretely, so one cannot even be handed to it.
type RelationalReadableFeature interface {
	Feature
	RelationalViews() []*query.RelationalViewDefinition
}

// collectRelationalViews aggregates relational read models from every
// RelationalReadableFeature, rejecting name collisions BETWEEN features. The
// cross-family check — against Mongo views and composed views, which share the
// one read-side namespace — runs later in query.ValidateRelationalViews, once all
// three sets are resolved.
func collectRelationalViews(features []Feature) ([]*query.RelationalViewDefinition, error) {
	var out []*query.RelationalViewDefinition
	seen := map[string]string{} // view name -> first owner ("%T")
	for _, f := range features {
		rf, ok := f.(RelationalReadableFeature)
		if !ok {
			continue
		}
		for _, v := range rf.RelationalViews() {
			if v == nil {
				continue
			}
			owner := fmt.Sprintf("%T", f)
			if prev, dup := seen[v.Name()]; dup {
				return nil, fmt.Errorf(
					"bootstrap: relational view %q declared by both %s and %s — read-model names are service-unique",
					v.Name(), prev, owner)
			}
			seen[v.Name()] = owner
			out = append(out, v)
		}
	}
	return out, nil
}
