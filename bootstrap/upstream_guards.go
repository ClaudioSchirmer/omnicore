package bootstrap

import (
	"fmt"
	"strings"

	"github.com/ClaudioSchirmer/omnicore/infra"
)

// resolveUpstreamSubscriptions concatenates the YAML-declared slice
// (cfg.UpstreamSubscriptions) and the Wiring-declared slice
// (wiring.UpstreamSubscriptions). Topic collisions across the two sources
// are rejected so the operator picks one declaration site per subscription.
//
// Returns the merged slice (caller responsibility to mutate further) or an
// error describing the collision. Both inputs may be nil/empty; an empty
// merge result is valid and yields no boot work.
func resolveUpstreamSubscriptions(cfg *Config, wiring Wiring) ([]UpstreamSubscription, error) {
	var out []UpstreamSubscription
	out = append(out, cfg.UpstreamSubscriptions...)
	if len(wiring.UpstreamSubscriptions) == 0 {
		return out, nil
	}
	seen := map[string]string{} // topic → declaration source
	for _, s := range out {
		seen[s.Topic] = "yaml"
	}
	for _, s := range wiring.UpstreamSubscriptions {
		if where, dup := seen[s.Topic]; dup {
			return nil, fmt.Errorf(
				"upstreamSubscriptions: topic %q declared in both %s and %s — "+
					"keep one declaration site (yaml is canonical; Wiring is for manual lifecycle / tests)",
				s.Topic, where, "wiring",
			)
		}
		seen[s.Topic] = "wiring"
		out = append(out, s)
	}
	return out, nil
}

// applyUpstreamSubscriptionDefaults walks subs in place, filling per-entry
// defaults (ConsumerGroup naming uses cfg.Service). Returns the same slice
// for fluency; modifies the passed slice's elements through the address-
// taken loop variable.
func applyUpstreamSubscriptionDefaults(subs []UpstreamSubscription, service string) []UpstreamSubscription {
	for i := range subs {
		subs[i].applyDefaults(service)
	}
	return subs
}

// validateUpstreamSubscriptions runs the four boot guards from §8 of
// mongo_cross_service_composition_final.md and the per-entry shape guard
// (§5 yaml validation that survives manual Wiring entries). All checks
// accumulate findings into a single error so the operator sees every
// violation in one boot attempt — the pattern matches the framework's
// other multi-finding diagnostics (validateWiring, MongoSpec validation).
//
// Inputs:
//   - subs: the resolved slice (cfg + wiring, defaults applied)
//   - views: the views collected from Wiring.Features
//   - profile: cfg.Profile, threaded for the per-entry shape check
//
// Each guard is implemented in its own helper for readability and tests:
//
//   - §8.1 — Mandatory index on join field (every FromMongo embed)
//   - §8.2 — Collection name collision (sub↔sub and sub↔local view)
//   - §8.3 — Mongo embed must have a materializing source
//   - §8.4 — Anonymize policy requires AnonymizeFields
//
// Per-entry shape validation (§5) runs first because a structurally
// invalid entry would mislead the multi-entry guards.
func validateUpstreamSubscriptions(
	subs []UpstreamSubscription,
	views []*infra.ViewDefinition,
	profile string,
) error {
	var violations []string
	for i, s := range subs {
		if err := s.validateShape(profile); err != nil {
			violations = append(violations, fmt.Sprintf("entry[%d]: %v", i, err))
		}
	}
	if errs := guardJoinFieldIndex(views); len(errs) > 0 {
		violations = append(violations, errs...)
	}
	if errs := guardCollectionCollision(subs, views); len(errs) > 0 {
		violations = append(violations, errs...)
	}
	if errs := guardMaterializingSource(subs, views); len(errs) > 0 {
		violations = append(violations, errs...)
	}
	if errs := guardAnonymizePolicy(subs); len(errs) > 0 {
		violations = append(violations, errs...)
	}
	if len(violations) == 0 {
		return nil
	}
	return fmt.Errorf(
		"upstreamSubscriptions: %d violation(s) — boot aborted before any subscriber goroutine starts:\n  - %s",
		len(violations), strings.Join(violations, "\n  - "),
	)
}

// guardJoinFieldIndex implements §8.1. Walks every view and every embed
// whose source is Mongo-kind; rejects the boot when a (viewName, joinField)
// pair has no covering index declared on the view (single-field with the
// joinField OR compound where joinField is the FIRST key).
//
// "Covering" matches the recompose-ripple's query shape:
//
//	mongo.Find({joinField: aggregate_id}, {_id: 1})
//
// Suffix-only coverage in a compound index does not satisfy Mongo's
// equality-prefix rule, so the guard accepts only first-position matches.
func guardJoinFieldIndex(views []*infra.ViewDefinition) []string {
	var out []string
	for _, v := range views {
		for _, e := range v.Embeds() {
			if !e.Source().IsMongo() {
				continue
			}
			joinField := e.Source().JoinKey()
			if joinField == "" {
				out = append(out, fmt.Sprintf(
					"§8.1 view %q embeds upstream Mongo collection %q with no join field declared "+
						"(call .On(\"<field>\") on the FromMongo source)",
					v.Name(), e.Source().Collection(),
				))
				continue
			}
			if !viewIndexesCover(v, joinField) {
				out = append(out, fmt.Sprintf(
					"§8.1 view %q embeds upstream Mongo collection %q on join field %q "+
						"but no covering index is declared (need fwinfra.Index(%q) or "+
						"fwinfra.Compound(%q, ...) with %q as the FIRST key)",
					v.Name(), e.Source().Collection(), joinField, joinField, joinField, joinField,
				))
			}
		}
	}
	return out
}

// viewIndexesCover scans v.IndexSpecs() for an index whose key set begins
// with joinField. Used by §8.1.
func viewIndexesCover(v *infra.ViewDefinition, joinField string) bool {
	for _, idx := range v.IndexSpecs() {
		keys := idx.KeyNames()
		if len(keys) == 0 {
			continue
		}
		if keys[0] == joinField {
			return true
		}
	}
	return false
}

// guardCollectionCollision implements §8.2 with two sub-checks under the
// same diagnostic surface:
//
//  1. subscription ↔ subscription: two entries with the same Collection
//     would race on the same Mongo collection. Reject regardless of source
//     (yaml-declared or wiring-declared; the resolveUpstreamSubscriptions
//     step already rejected same-topic duplicates).
//
//  2. subscription ↔ local view: a subscription's Collection matches a
//     local ViewDefinition.Name() — same Mongo collection would receive
//     writes from both the UpstreamSubscriber AND the SyncEngine.
//     Reject; the operator either renames the subscription or removes the
//     local view if the data is meant to be entirely upstream-projected.
func guardCollectionCollision(subs []UpstreamSubscription, views []*infra.ViewDefinition) []string {
	var out []string
	localViews := make(map[string]bool, len(views))
	for _, v := range views {
		localViews[v.Name()] = true
	}
	seenSub := make(map[string]string, len(subs)) // collection → first topic claiming it
	for _, s := range subs {
		if first, dup := seenSub[s.Collection]; dup {
			out = append(out, fmt.Sprintf(
				"§8.2 two subscriptions declare collection=%q (topics: %q, %q) — "+
					"two writers on the same local Mongo collection would race. Rename one.",
				s.Collection, first, s.Topic,
			))
			continue
		}
		seenSub[s.Collection] = s.Topic
		if localViews[s.Collection] {
			out = append(out, fmt.Sprintf(
				"§8.2 subscription topic=%q collection=%q collides with a local view of the same name — "+
					"two writers on the same Mongo collection would create ambiguity of ownership. "+
					"Either rename the subscription's collection (e.g. %q, %q) or remove the local view "+
					"if %q is meant to be entirely upstream-projected.",
				s.Topic, s.Collection, "external_"+s.Collection, "peer_"+s.Collection, s.Collection,
			))
		}
	}
	return out
}

// guardMaterializingSource implements §8.3. For every embed whose source
// is Mongo-kind, the named collection MUST resolve at boot to an
// UpstreamSubscription.Collection — otherwise the embed would silently
// resolve to an empty slice in production.
//
// View-on-view via FromMongo (FromMongo targeting another local
// ViewDefinition.Name()) is explicitly NOT supported: the recompose
// ripple is one-hop (see UpstreamSubscriber.ripple consulting
// viewIndex.byMongoColl, populated from subscription collections only),
// so a change in the upstream of view Y would recompose Y but never
// trigger view X that embeds Y. Drift would accumulate silently. The
// guard rejects this shape at boot so consumers never reach the trap;
// the diagnostic suggests the supported alternatives.
func guardMaterializingSource(subs []UpstreamSubscription, views []*infra.ViewDefinition) []string {
	var out []string
	subCollections := make(map[string]bool, len(subs))
	for _, s := range subs {
		subCollections[s.Collection] = true
	}
	localViews := make(map[string]bool, len(views))
	for _, v := range views {
		localViews[v.Name()] = true
	}
	for _, v := range views {
		for _, e := range v.Embeds() {
			if !e.Source().IsMongo() {
				continue
			}
			coll := e.Source().Collection()
			if subCollections[coll] {
				continue
			}
			if localViews[coll] {
				out = append(out, fmt.Sprintf(
					"§8.3 view %q embeds Mongo collection %q via FromMongo, but %q is the name of a local "+
						"ViewDefinition — view-on-view composition via FromMongo is NOT supported. The "+
						"recompose ripple is one-hop: an upstream change recomposes %q but never re-ripples "+
						"to %q, so %q would drift silently. Either embed the upstream collection directly "+
						"with FromMongo(\"<upstream_collection>\").On(...), or model the JOIN at the "+
						"Postgres root via From(%q) if %q is a regular table.",
					v.Name(), coll, coll, coll, v.Name(), v.Name(), coll, coll,
				))
				continue
			}
			out = append(out, fmt.Sprintf(
				"§8.3 view %q embeds Mongo collection %q via FromMongo but no UpstreamSubscription "+
					"declares collection=%q — the embed would always resolve to empty. Either declare "+
					"an UpstreamSubscription{Topic, Collection: %q, ...} or replace FromMongo(%q) with "+
					"From(%q) for Postgres.",
				v.Name(), coll, coll, coll, coll, coll,
			))
		}
	}
	return out
}

// guardAnonymizePolicy implements §8.4. The anonymize policy needs the
// explicit field set to blank; an empty set under anonymize is almost
// always a typo (e.g. omitted the wrong key, or forgot to copy from
// schema). cascade and keep do not consume AnonymizeFields — an empty
// slice there is fine.
func guardAnonymizePolicy(subs []UpstreamSubscription) []string {
	var out []string
	for _, s := range subs {
		if s.OnUpstreamDelete != UpstreamDeleteAnonymize {
			continue
		}
		if len(s.AnonymizeFields) == 0 {
			out = append(out, fmt.Sprintf(
				"§8.4 subscription topic=%q collection=%q declares onUpstreamDelete: anonymize "+
					"but anonymizeFields is empty %s — anonymize requires the explicit set of fields to "+
					"blank when the upstream entity is deleted (e.g. anonymizeFields: [name, email, phone])",
				s.Topic, s.Collection, describeFilter(s.AnonymizeFields),
			))
		}
	}
	return out
}
