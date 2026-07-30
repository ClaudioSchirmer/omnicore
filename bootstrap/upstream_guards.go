package bootstrap

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
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
//   - §8.5 (generalized) — every declared external-schema column must survive
//     the subscription's `fields:` allowlist (abort) + advisory warning when
//     no consuming schema declares a DeletedAt column
//
// §8.5 is the only guard with a non-fatal branch: a mirror whose consuming
// schemas declare no DeletedAt column yield an advisory (logged via
// logger, which may be nil in tests), not a boot-aborting violation.
//
// Per-entry shape validation (§5) runs first because a structurally
// invalid entry would mislead the multi-entry guards.
func validateUpstreamSubscriptions(
	subs []UpstreamSubscription,
	views []*query.ViewDefinition,
	composed []*query.ComposedViewDefinition,
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
	// §8.5 (generalized) — every column a consumer's external schema declares
	// must survive the subscription's `fields:` allowlist (a dead declaration
	// aborts); a mirror whose consuming schemas declare no DeletedAt column at
	// all is an advisory (logged, not fatal).
	sdViolations, sdWarnings := guardSchemaFieldsSurvival(subs, views, composed)
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
			// doc's ParentID value → the parent _id (always indexed), never by a reverse
			// scan of the parent view on the child ParentID column — which the parent
			// document does not even carry at top level (it lives under the embed
			// segment, e.g. "items.account_id"). The covering-index requirement is
			// a one-to-one Embed concern ONLY: there the PARENT holds the ParentID column
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

// guardSchemaFieldsSurvival implements §8.5, generalized — EVERY column an
// external schema declares must survive the subscription's `fields:` allowlist,
// because the allowlist operates on the raw upstream payload and consults no
// schema (at ingestion time no class is materialized). A declared column the
// mirror can never carry is a dead declaration: reads translate it, exports
// advertise it, and it is forever absent. The DeletedAt column is the highest-
// stakes instance (an archived upstream entity would look active forever), so
// it keeps its dedicated diagnostic.
//
// Two branches:
//
//   - ABORT (violation): a consumer's external schema declares a column that a
//     non-empty `fields:` OMITS — generic message, archive-flavored when the
//     column is that schema's DeletedAt.
//   - ADVISORY (warning): NO consuming schema declares a DeletedAt column on
//     the mirror. The consumer often cannot know whether the upstream
//     archives, so this is a reminder, not a defect. Returned separately so
//     the caller logs it at Warn rather than aborting the boot.
//
// An empty `fields:` mirrors the full payload, so every column survives
// unconditionally — never a violation. A subscription whose Collection is
// embedded by no view is skipped here (§8.3 governs the never-embedded case).
// Consumers cross-checked: root embeds, EmbedInChild enrichments and a
// ComposedView's external legs — every reader of the mirror's columns.
func guardSchemaFieldsSurvival(subs []UpstreamSubscription, views []*query.ViewDefinition, composed []*query.ComposedViewDefinition) (violations, warnings []string) {
	// Per embedded upstream Mongo collection: every column each consumer's
	// external schema declares (minus the ID column — the mirror's identity
	// lives in _id, the payload carries no id column), tagged with whether it
	// is that schema's DeletedAt and who declared it, for the diagnostic. The
	// same collection may be consumed by several views with independent
	// external schemas, so declarations accumulate.
	type declaration struct {
		column, view string
		isDeletedAt  bool
	}
	declaredBy := map[string][]declaration{}
	embedded := map[string]bool{}
	sdDeclared := map[string]bool{}
	collect := func(schema *core.TableSchema, coll, consumer string) {
		embedded[coll] = true
		if schema == nil {
			return
		}
		sd, hasSD := schema.DeletedAtColumn()
		if hasSD {
			sdDeclared[coll] = true
		}
		pk := schema.IDColumn()
		for _, col := range schema.ReadColumns() {
			if col == pk {
				continue
			}
			declaredBy[coll] = append(declaredBy[coll], declaration{
				column: col, view: consumer, isDeletedAt: hasSD && col == sd,
			})
		}
	}
	for _, v := range views {
		for _, e := range v.Embeds() {
			if src := e.Source(); src.IsMongo() {
				collect(src.SchemaDef(), src.Collection(), v.Name())
			}
		}
		for _, ce := range v.ChildEmbeds() {
			if src := ce.Source(); src.IsMongo() {
				collect(src.SchemaDef(), src.Collection(), v.Name())
			}
		}
	}
	for _, c := range composed {
		for _, leg := range c.ExternalLegs() {
			collect(leg.SchemaDef(), leg.Collection(), "composed "+c.Name())
		}
	}
	for _, s := range subs {
		if !embedded[s.Collection] {
			continue // never embedded — §8.3 owns that case; nothing to cross-check here
		}
		if !sdDeclared[s.Collection] {
			warnings = append(warnings, fmt.Sprintf(
				"subscription topic=%q collection=%q: no view embedding or composing this mirror declares a "+
					"DeletedAt column on its external schema. If the upstream entity archives, declare "+
					".DeletedAt(\"<column>\") on the NewExternalSchema AND keep that column in the subscription's "+
					"`fields:` — otherwise an archived upstream entity stays looking active in the mirror forever. "+
					"Advisory: harmless if the upstream never archives.",
				s.Topic, s.Collection,
			))
		}
		if len(s.Fields) == 0 {
			continue // an empty `fields:` mirrors the full payload — every declared column survives
		}
		inFields := make(map[string]bool, len(s.Fields))
		for _, f := range s.Fields {
			inFields[f] = true
		}
		// A declared column the allowlist drops is a dead declaration — report
		// once per distinct column, in deterministic order.
		firstView := map[string]string{}
		isSD := map[string]bool{}
		var dropped []string
		for _, d := range declaredBy[s.Collection] {
			if inFields[d.column] {
				continue
			}
			if _, seen := firstView[d.column]; !seen {
				firstView[d.column] = d.view
				isSD[d.column] = d.isDeletedAt
				dropped = append(dropped, d.column)
			}
		}
		sort.Strings(dropped)
		for _, col := range dropped {
			if isSD[col] {
				violations = append(violations, fmt.Sprintf(
					"subscription topic=%q collection=%q declares fields %s which OMITS the DeletedAt "+
						"column %q that %q's leg schema declares — an archived upstream entity carries "+
						"%q in its event, the allowlist would strip it, and the mirror could never reflect the "+
						"archive (archived rows would look active forever). Add %q to `fields:`, or clear "+
						"`fields:` to mirror the full payload.",
					s.Topic, s.Collection, describeFilter(s.Fields), col, firstView[col], col, col,
				))
				continue
			}
			violations = append(violations, fmt.Sprintf(
				"subscription topic=%q collection=%q declares fields %s which OMITS column %q that %q's "+
					"external schema declares — the mirror can never carry it, so the declaration is dead "+
					"(reads translate it, exports advertise it, the value is forever absent). Add %q to "+
					"`fields:`, drop it from the schema, or clear `fields:` to mirror the full payload.",
				s.Topic, s.Collection, describeFilter(s.Fields), col, firstView[col], col,
			))
		}
	}
	return violations, warnings
}
