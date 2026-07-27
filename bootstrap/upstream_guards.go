package bootstrap

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/ClaudioSchirmer/omnicore/infra/db/query"
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
//   - §8.1 — Mandatory index on join field (every external JoinUpstream embed)
//   - §8.2 — Collection name collision (sub↔sub and sub↔local view)
//   - §8.3 — Mongo embed must have a materializing source
//   - §8.4 — Anonymize policy requires AnonymizeFields
//   - §8.5 — Soft-delete column must survive the filter (abort) + advisory
//     warning when no embedding schema declares one
//
// §8.5 is the only guard with a non-fatal branch: a mirror whose embed
// schema declares no soft-delete column yields an advisory (logged via
// logger, which may be nil in tests), not a boot-aborting violation.
//
// Per-entry shape validation (§5) runs first because a structurally
// invalid entry would mislead the multi-entry guards.
func validateUpstreamSubscriptions(
	subs []UpstreamSubscription,
	views []*query.ViewDefinition,
	profile string,
	logger *slog.Logger,
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
	// §8.5 — a declared soft-delete column that the subscription filter drops is a
	// silent-archive bug (abort); a mirror whose embed schema declares no soft-delete
	// column at all is an advisory (logged, not fatal).
	sdViolations, sdWarnings := guardSoftDeleteFilter(subs, views)
	violations = append(violations, sdViolations...)
	if logger != nil {
		for _, w := range sdWarnings {
			logger.Warn(w)
		}
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
func guardJoinFieldIndex(views []*query.ViewDefinition) []string {
	var out []string
	for _, v := range views {
		for _, e := range v.Embeds() {
			if !e.Source().IsMongo() {
				continue
			}
			joinField := e.JoinColumn()
			if joinField == "" {
				out = append(out, fmt.Sprintf(
					"§8.1 view %q embeds upstream Mongo collection %q with an empty join column — "+
						"name it via .On(\"<field>\") on the Embed/EmbedMany",
					v.Name(), e.Source().Collection(),
				))
				continue
			}
			// A one-to-many EmbedMany needs NO covering index on the embedding
			// view: its recompose-ripple resolves parents by the CHANGED upstream
			// doc's FK value → the parent _id (always indexed), never by a reverse
			// scan of the parent view on the child FK column — which the parent
			// document does not even carry at top level (it lives under the embed
			// segment, e.g. "items.account_id"). The covering-index requirement is
			// a one-to-one Embed concern ONLY: there the PARENT holds the FK column
			// the ripple scans by (FindIDsByField(view, parentFK, upstreamID)).
			if e.Many() {
				continue
			}
			if !viewIndexesCover(v, joinField) {
				out = append(out, fmt.Sprintf(
					"§8.1 view %q embeds upstream Mongo collection %q on join field %q "+
						"but no covering index is declared (need fwmongo.Index(%q) or "+
						"fwmongo.Compound(%q, ...) with %q as the FIRST key)",
					v.Name(), e.Source().Collection(), joinField, joinField, joinField, joinField,
				))
			}
		}
	}
	return out
}

// viewIndexesCover scans v.IndexSpecs() for an index whose key set begins
// with joinField. Used by §8.1.
func viewIndexesCover(v *query.ViewDefinition, joinField string) bool {
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
func guardCollectionCollision(subs []UpstreamSubscription, views []*query.ViewDefinition) []string {
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
// A JoinUpstream leg naming a LOCAL view's collection stays rejected — but for
// a declaration reason, not a propagation one: materializing a local view is
// declared with query.JoinView (which carries the view, so the SyncEngine
// signals every write to it and the embedding view is refreshed), never by
// pointing an external schema at the view's collection behind the framework's
// back (that leg has no view to signal on, and no subscription materializes it).
// The diagnostic names the supported form.
func guardMaterializingSource(subs []UpstreamSubscription, views []*query.ViewDefinition) []string {
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
					"§8.3 view %q embeds Mongo collection %q through a JoinUpstream leg, but %q is the name of a "+
						"local ViewDefinition — an external schema pointing at a local view's collection is not "+
						"the way to materialize it (nothing signals that leg on a write, and no UpstreamSubscription "+
						"materializes it). Materialize the view with its own leg: "+
						"Embed(query.JoinView(%sView(), \"Go\", \"doc\")).On(...) — the SyncEngine then refreshes %q on "+
						"every write to %q. To join without materializing, use query.ComposedView.",
					v.Name(), coll, coll, coll, v.Name(), coll,
				))
				continue
			}
			out = append(out, fmt.Sprintf(
				"§8.3 view %q embeds Mongo collection %q but no UpstreamSubscription declares collection=%q — "+
					"the embed would always resolve to empty. Declare an UpstreamSubscription{Topic, "+
					"Collection: %q, ...} that materializes it (materialize, then embed).",
				v.Name(), coll, coll, coll,
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

// guardSoftDeleteFilter implements §8.5 — the soft-delete column must survive a
// subscription's Filter, because the filter is a string allowlist over the raw
// upstream payload and does not consult any schema. An ARCHIVED upstream event
// carries the soft-delete column populated; if the Filter drops it, the local
// mirror can never reflect the archive (archived rows look active forever).
//
// The check pairs each subscription with the soft-delete column declared on the
// external schema of the view(s) that embed the subscription's Collection
// (reached via Embed().Source().SchemaDef().SoftDeleteColumn()), and splits into
// two branches:
//
//   - ABORT (violation): the embed schema DECLARES a soft-delete column but the
//     Filter is non-empty and OMITS it. This is an unambiguous silent-archive
//     misconfiguration — fail loud.
//   - ADVISORY (warning): NO embedding schema declares a soft-delete column on
//     the mirror. The consumer often cannot know whether the upstream
//     soft-deletes, so this is a reminder, not a defect. Returned separately so
//     the caller logs it at Warn rather than aborting the boot.
//
// An empty Filter mirrors the full payload, so the soft-delete column survives
// unconditionally — never a violation. A subscription whose Collection is
// embedded by no view is skipped here (§8.3 governs the never-embedded case).
func guardSoftDeleteFilter(subs []UpstreamSubscription, views []*query.ViewDefinition) (violations, warnings []string) {
	// Per embedded upstream Mongo collection: the soft-delete columns declared on
	// its external schema (with the view that declared each, for the diagnostic).
	// The same collection may be embedded by several views with independent
	// external schemas, so declarations accumulate.
	type declaration struct{ column, view string }
	declaredBy := map[string][]declaration{}
	embedded := map[string]bool{}
	for _, v := range views {
		for _, e := range v.Embeds() {
			src := e.Source()
			if !src.IsMongo() {
				continue
			}
			coll := src.Collection()
			embedded[coll] = true
			if sd, ok := src.SchemaDef().SoftDeleteColumn(); ok {
				declaredBy[coll] = append(declaredBy[coll], declaration{column: sd, view: v.Name()})
			}
		}
	}
	for _, s := range subs {
		if !embedded[s.Collection] {
			continue // never embedded — §8.3 owns that case; nothing to cross-check here
		}
		decls := declaredBy[s.Collection]
		if len(decls) == 0 {
			warnings = append(warnings, fmt.Sprintf(
				"§8.5 subscription topic=%q collection=%q: no view embedding this mirror declares a "+
					"soft-delete column on its external schema. If the upstream entity soft-deletes "+
					"(archive), declare .SoftDelete(\"<column>\") on the NewExternalSchema AND keep that "+
					"column in the subscription's filter — otherwise an archived upstream entity stays "+
					"looking active in the mirror forever. Advisory: harmless if the upstream never archives.",
				s.Topic, s.Collection,
			))
			continue
		}
		if len(s.Filter) == 0 {
			continue // an empty filter mirrors the full payload — the soft-delete column survives
		}
		inFilter := make(map[string]bool, len(s.Filter))
		for _, f := range s.Filter {
			inFilter[f] = true
		}
		// A declared soft-delete column the filter drops is the silent-archive bug.
		// Report once per distinct column, in deterministic order.
		firstView := map[string]string{}
		var dropped []string
		for _, d := range decls {
			if inFilter[d.column] {
				continue
			}
			if _, seen := firstView[d.column]; !seen {
				firstView[d.column] = d.view
				dropped = append(dropped, d.column)
			}
		}
		sort.Strings(dropped)
		for _, col := range dropped {
			violations = append(violations, fmt.Sprintf(
				"§8.5 subscription topic=%q collection=%q declares filter %s which OMITS the soft-delete "+
					"column %q that view %q's embed schema declares — an archived upstream entity carries "+
					"%q in its event, the filter would strip it, and the mirror could never reflect the "+
					"archive (archived rows would look active forever). Add %q to the filter, or clear the "+
					"filter to mirror the full payload.",
				s.Topic, s.Collection, describeFilter(s.Filter), col, firstView[col], col, col,
			))
		}
	}
	return violations, warnings
}
