package query

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// embedRippler is the recompose-ripple engine shared by every writer of an
// embed SOURCE: it answers "the source document changed — refresh every view
// that embeds it". Extracted from UpstreamSubscriber so the same pass can be
// driven by any source kind: today the UpstreamSubscriber (source = a locally
// materialized upstream Mongo collection), next the SyncEngine (source = a
// local view embedded via query.JoinView).
//
// The type is a pure parameter pack — no lifecycle, no goroutines, no state
// beyond the wiring handles. topic is the failure-registry / metrics
// coordinate (the subscription topic for an upstream source); collection is
// the source's local Mongo collection (the subscription collection, or the
// embedded view's name); dependentViews are the views embedding that source.
//
// Failure isolation contract (§7.3), unchanged from the subscriber: every
// recompose error is logged + counted + persisted to
// the unified ledger (omnicore_projection_failures, kind=ripple) and SKIPPED —
// the caller's offset/event always advances; stale documents are drained by the
// parked-retry loop, or by the next source event.
type embedRippler struct {
	// eng is the relational engine the unified failure ledger
	// (omnicore_projection_failures, kind=ripple) is read/written through; the
	// recompose ripple itself is Mongo + the composer. Nil-safe (test
	// scaffolds).
	eng      core.RelationalEngine
	mongo    ReadModelStore
	composer *Composer
	// resolver maps the source collection and each dependent view name to the
	// physical collection it currently resolves to (active slot).
	resolver *ViewResolver
	// topic labels failures + metrics rows for this source (the subscription
	// topic today; a synthetic "view:<name>" label for a view source).
	topic string
	// group is the sync consumer group that OWNS the ledger rows this rippler
	// records — the parked-retry loop replays rows scoped to its group, so a
	// row recorded under any other name would never be swept. Plumbed from the
	// SyncEngine (view sources) or WithViewChaining (subscription sources);
	// empty only in wiring-less test scaffolds, where eng is nil too.
	group string
	// collection is the source's local Mongo collection the dependent views
	// embed (subscription.Collection, or the embedded view's name).
	collection string
	// sourceIsView distinguishes the two source kinds when matching a
	// dependent's embeds: a JoinView leg names a VIEW (leg.view), an external
	// leg names a MIRROR collection (leg.IsMongo). They live in separate
	// namespaces — the same string can be a view name and, in principle, a
	// collection name — so the match is by kind AND name, never by name alone.
	sourceIsView   bool
	dependentViews []*ViewDefinition
	logger         *slog.Logger
	metrics        *upstreamMetrics
	// onViewWritten, when set, is called after this ripple successfully writes a
	// DEPENDENT view's document — the next hop of the chain: that view may itself
	// be materialized into another one (query.JoinView), and its refreshed
	// document is exactly the signal the next embedder needs. Nil disables
	// chaining. Termination is guaranteed by the acyclic embed graph
	// (appendEmbedCycles); each hop pays its own ripple.
	onViewWritten func(ctx context.Context, viewName, localID string)
}

// legMatches reports whether a dependent's embed leg reads THIS source.
func (r *embedRippler) legMatches(leg *Leg) bool {
	if leg == nil {
		return false
	}
	if r.sourceIsView {
		return leg.view != nil && leg.view.Name() == r.collection
	}
	return leg.IsMongo() && leg.Collection() == r.collection
}

// collectEmbeds / collectChildEmbeds select the dependent's declarations fed by
// this source — the kind-aware counterparts of the package-level
// collectMongoEmbeds / collectChildMongoEmbeds.
func (r *embedRippler) collectEmbeds(v *ViewDefinition) []embedDef {
	var out []embedDef
	for _, e := range v.Embeds() {
		if r.legMatches(e.leg) {
			out = append(out, e)
		}
	}
	return out
}

func (r *embedRippler) collectChildEmbeds(v *ViewDefinition) []childEmbedDef {
	var out []childEmbedDef
	for _, ce := range v.ChildEmbeds() {
		if r.legMatches(ce.leg) {
			out = append(out, ce)
		}
	}
	return out
}

// chain fires the next hop after a successful write to viewName's document.
// The pre-write state is deliberately not captured for a chained hop: a ripple
// only rewrites EMBED segments, never the document's own ParentID columns, so the old
// and new 1:N parent of the chained document are the same and a nil `before`
// discovers the identical target set.
func (r *embedRippler) chain(ctx context.Context, viewName, localID string) {
	if r.onViewWritten == nil {
		return
	}
	r.onViewWritten(ctx, viewName, localID)
}

// ripple is the downstream recompose pass. For every dependent view it
// asks Mongo "which docs reference the changed upstream id?", recomposes
// each one through the Composer, and upserts the result. Failures are
// per-view isolated: a Composer bug or upsert error on view A does not
// block view B referencing the same upstream entity. The caller's offset
// still advances after this returns — the alternative (block on failure)
// turns a deterministic recompose bug into a poison pill across the whole
// consumer group.
//
// Every failure is persisted to the unified ledger alongside the
// in-memory metric so operators have a queryable record of stale docs
// surviving past the consumer group's offset. A view+upstream_id pair
// that completes the full recompose pass without errors resolves prior
// pending rows for the same coordinate — the ledger mirrors the live
// state, not a monotonically-growing log.
// srcRev is the source document's revision watermark (0 for an unwatermarked
// source such as an upstream mirror — see the srcRev commentary in
// embed_surgical.go).
func (r *embedRippler) ripple(ctx context.Context, upstreamID string, before, after Document, srcRev int64) {
	for _, v := range r.dependentViews {
		embeds := r.collectEmbeds(v)
		if len(embeds) == 0 {
			// No ROOT embed of this collection — the view may be a dependent only
			// via EmbedInChild, handled in the child-embed pass below. Skip the
			// root-embed handling.
			continue
		}
		// Discover the local parent docs to recompose — the UNION across every
		// embed of this collection on the view (a view may embed the same
		// collection both 1:1 and 1:N). See discoverRippleTargets.
		localIDs, discoverErr := r.discoverRippleTargets(ctx, v, embeds, upstreamID, before, after)
		if discoverErr {
			continue
		}
		// EXISTING parent docs take the surgical per-element edit (see
		// embed_surgical.go): no relational read, and edits for different
		// upstream ids commute, so concurrent ripples cannot erase each
		// other's elements. A parent with no document yet falls back to the
		// full recompose below, which materializes the complete composition
		// (racing a concurrent SyncEngine create is safe — both writers use
		// the field-ownership upsert). Non-upsert on the surgical write keeps
		// a ripple racing a concurrent document delete from resurrecting a
		// skeleton.
		failed := false
		var fallback []string
		stages := surgicalEmbedStages(embeds, upstreamID, after, srcRev)
		if stages == nil {
			fallback = localIDs
		} else {
			for _, localID := range localIDs {
				present, err := r.mongo.FindIDsByField(ctx, r.resolver.Active(v.Name()), "_id", localID)
				if err != nil {
					r.metrics.inc(r.topic, v.Name(), upstreamRecomposeStageUpsert)
					r.logger.Error("upstream.recompose.exists",
						"subscription", r.topic, "view", v.Name(),
						"upstreamID", upstreamID, "localID", localID, "err", err)
					r.recordFailure(ctx, v.Name(), upstreamID, localID, ProjectionFailureStageUpsert, err)
					failed = true
					continue
				}
				if len(present) == 0 {
					fallback = append(fallback, localID)
					continue
				}
				if r.rippleApplyOne(ctx, v, upstreamID, localID, stages, false) {
					failed = true
				}
			}
		}
		// Full-recompose fallback (parent doc absent, or an embed with nested
		// embeds — the element in hand cannot carry nested content). One
		// set-based ComposeBatch; on a batch error drop to per-id compose to
		// isolate the offender, exactly as before.
		if len(fallback) > 0 {
			composed, batchErr := r.composer.ComposeBatch(ctx, v, fallback)
			if batchErr != nil {
				for _, localID := range fallback {
					if r.rippleRecomposeOne(ctx, v, upstreamID, localID) {
						failed = true
					}
				}
			} else {
				pkCol := schemaPK(v.schema)
				byID := make(map[string]Document, len(composed))
				for _, doc := range composed {
					byID[fmt.Sprintf("%v", doc[pkCol])] = doc
				}
				for _, localID := range fallback {
					doc := byID[localID]
					if doc == nil {
						continue // the local root vanished between discover and compose — skip, as the nil compose did
					}
					if r.rippleUpsertOne(ctx, v, upstreamID, localID, doc) {
						failed = true
					}
				}
			}
		}
		if !failed {
			r.resolveFailures(ctx, v.Name(), upstreamID)
		}
	}
	// Child-embed pass (EmbedInChild): the enrichment lives INSIDE a native child
	// array (items[].product), not a root embed segment, so the surgical /
	// field-ownership write above cannot reach it. Each affected doc is FULLY
	// recomposed and written with the consult-guarded pipeline (own data behind
	// _revision, equal-revision fill landing the refreshed enrichment) — the same
	// write the SyncEngine uses, so a concurrent local write is never regressed.
	for _, v := range r.dependentViews {
		childEmbeds := r.collectChildEmbeds(v)
		if len(childEmbeds) == 0 {
			continue
		}
		r.rippleChildEmbeds(ctx, v, childEmbeds, upstreamID, after, srcRev)
	}
}

// rippleChildEmbeds surgically updates the enriched sub-document inside every
// matching child element, for each EmbedInChild of this collection — the
// per-element analog of the root embed's surgical ripple. The enrichment lives
// INSIDE the child array and is owned by THIS ripple (the source's writes),
// NOT by the parent's revision, so it must NOT go through the
// revision-guarded consult path: at the parent's unchanged revision a consult
// scope only fills MISSING fields (stored wins), which could never overwrite a
// stale enrichment. Instead a `$map` rewrites ONLY the matched elements'
// enrichment field — unguarded, and commuting with concurrent ripples for other
// items (each touches a disjoint element set). `after` is the changed upstream
// document (the new sub-doc value); nil on delete clears the enrichment to null.
func (r *embedRippler) rippleChildEmbeds(ctx context.Context, v *ViewDefinition, childEmbeds []childEmbedDef, upstreamID string, after Document, srcRev int64) {
	// The enrichment value is embedded as a $literal: it is DATA (the upstream
	// sub-document, or null on delete), not an aggregation expression. Without
	// $literal Mongo would evaluate the map as an expression object — the same
	// reason surgicalManyExpr wraps its element in lit(...).
	var itemVal any
	if after != nil {
		itemVal = map[string]any(after)
	} // else nil → clear the enrichment to null (source deleted)
	litVal := lit(itemVal)
	for _, ce := range childEmbeds {
		seg := ce.ChildSegment()
		fkCol := ce.ParentIDColumn()
		// $map over the child array (defensive $ifNull for a missing/absent
		// array): each element whose ParentID equals the changed upstream id gets its
		// enrichment field merged in; every other element is left untouched.
		matches := Document{"$eq": []any{"$$e." + fkCol, upstreamID}}
		if srcRev > 0 {
			// Watermarked source (a view): keep an enrichment already refreshed by
			// a newer write — see the srcRev commentary in embed_surgical.go.
			matches = Document{"$and": []any{matches, notNewerThan("$$e."+ce.Field(), srcRev)}}
		}
		stage := Document{"$set": Document{
			seg: Document{"$map": Document{
				"input": Document{"$ifNull": []any{"$" + seg, []any{}}},
				"as":    "e",
				"in": Document{"$cond": []any{
					matches,
					Document{"$mergeObjects": []any{"$$e", Document{ce.Field(): litVal}}},
					"$$e",
				}},
			}},
		}}
		ids, err := r.mongo.FindIDsByField(ctx, r.resolver.Active(v.Name()), seg+"."+fkCol, upstreamID)
		if err != nil {
			r.metrics.inc(r.topic, v.Name(), upstreamRecomposeStageDiscover)
			r.logger.Error("upstream.recompose.discover.child",
				"subscription", r.topic, "view", v.Name(),
				"upstreamID", upstreamID, "field", seg+"."+fkCol, "err", err)
			r.recordFailure(ctx, v.Name(), upstreamID, "", ProjectionFailureStageDiscover, err)
			continue
		}
		failed := false
		for _, id := range ids {
			if r.rippleApplyOne(ctx, v, upstreamID, id, []Document{stage}, false) {
				failed = true
			}
		}
		if !failed {
			r.resolveFailures(ctx, v.Name(), upstreamID)
		}
	}
}

// rippleRecomposeOne composes one local doc and upserts it, recording any
// compose- or upsert-stage failure to the registry. Returns true iff a failure
// was recorded (a vanished local root — nil compose — is NOT a failure). It is
// the per-id fallback the batch ripple drops to when a set-based ComposeBatch
// fails, so a single offending id is isolated exactly as the pre-batch path did.
func (r *embedRippler) rippleRecomposeOne(ctx context.Context, v *ViewDefinition, upstreamID, localID string) (failed bool) {
	doc, err := r.composer.Compose(ctx, v, localID)
	if err != nil {
		r.metrics.inc(r.topic, v.Name(), upstreamRecomposeStageCompose)
		r.logger.Error("upstream.recompose.compose",
			"subscription", r.topic,
			"collection", r.collection,
			"view", v.Name(),
			"upstreamID", upstreamID,
			"localID", localID,
			"err", err)
		r.recordFailure(ctx, v.Name(), upstreamID, localID, ProjectionFailureStageCompose, err)
		return true
	}
	if doc == nil {
		return false
	}
	return r.rippleUpsertOne(ctx, v, upstreamID, localID, doc)
}

// rippleUpsertOne upserts one recomposed local doc into the view's active slot,
// recording an upsert-stage failure to the registry on error. Returns true iff a
// failure was recorded. Shared by the batch happy path and the per-id fallback so
// upsert failures stay isolated per local id on both.
//
// The write claims ONLY the embed segments (plus the full document when this
// upsert is creating it): the non-embed fields composed here may be staler
// than a concurrent SyncEngine recompose of the same document, and a
// full-document Upsert would regress them — see fieldOwnershipStages.
func (r *embedRippler) rippleUpsertOne(ctx context.Context, v *ViewDefinition, upstreamID, localID string, doc Document) (failed bool) {
	stages := fieldOwnershipStages(doc, schemaPK(v.schema), embedFieldSet(v.embeds), embedOrders(v.embeds))
	if failed = r.rippleApplyOne(ctx, v, upstreamID, localID, stages, true); failed {
		return true
	}
	repairDanglingOneToOne(ctx, r.mongo, r.resolver, r.eng, v, localID, doc, r.onViewWritten)
	return false
}

// rippleApplyOne runs one pipeline write against the view's active slot and —
// during a rebuild window — against the shadow slot too, with the same
// dual-apply discipline as the SyncEngine (dualApplyShadow: bounded retry,
// then abort the rebuild rather than fail the live path). Without the shadow
// leg, a ripple landing mid-rebuild would reach only the retiring active slot
// and the flipped collection would silently miss it. Records an upsert-stage
// failure on error; returns true iff a failure was recorded.
func (r *embedRippler) rippleApplyOne(ctx context.Context, v *ViewDefinition, upstreamID, localID string, stages []Document, upsert bool) (failed bool) {
	if err := r.mongo.ApplyProjection(ctx, r.resolver.Active(v.Name()), localID, stages, upsert); err != nil {
		r.metrics.inc(r.topic, v.Name(), upstreamRecomposeStageUpsert)
		r.logger.Error("upstream.recompose.upsert",
			"subscription", r.topic,
			"collection", r.collection,
			"view", v.Name(),
			"upstreamID", upstreamID,
			"localID", localID,
			"err", err)
		r.recordFailure(ctx, v.Name(), upstreamID, localID, ProjectionFailureStageUpsert, err)
		return true
	}
	if shadow, on := r.resolver.ShadowActive(v.Name()); on {
		dualApplyShadow(ctx, r.eng, r.resolver, v.Name(), func() error {
			return r.mongo.ApplyProjection(ctx, shadow, localID, stages, upsert)
		})
	}
	// The write landed on a VIEW document — the next hop's signal.
	r.chain(ctx, v.Name(), localID)
	return false
}

// discoverRippleTargets computes the distinct local parent _ids to recompose for
// one dependent view, unioning every embed of the changed collection:
//
//   - one-to-one Embed: the PARENT holds the ParentID column, so scan the parent view
//     for docs whose join field == the changed upstream _id
//     (FindIDsByField(view, parentFK, upstreamID)).
//   - one-to-many EmbedMany: the CHILD holds the ParentID, and its value IS the parent
//     _id, so read it from the doc state BEFORE and AFTER the change — a moved or
//     deleted child must recompose both the old and the new parent, and neither
//     is reachable by scanning the parent view (the ParentID lives on the child, under
//     the embed segment). No reverse scan, no covering index: the target is the
//     parent primary key, always indexed.
//
// Returns (ids, discoverErr): discoverErr is true when a 1:1 reverse scan errored
// (already recorded), signalling the caller to skip this view for this pass.
func (r *embedRippler) discoverRippleTargets(
	ctx context.Context,
	v *ViewDefinition,
	embeds []embedDef,
	upstreamID string,
	before, after Document,
) ([]string, bool) {
	seen := map[string]struct{}{}
	var ordered []string
	add := func(id string) {
		if id == "" {
			return
		}
		if _, dup := seen[id]; dup {
			return
		}
		seen[id] = struct{}{}
		ordered = append(ordered, id)
	}
	for _, e := range embeds {
		if e.Many() {
			fkCol := e.JoinColumn()
			add(docFieldString(before, fkCol))
			add(docFieldString(after, fkCol))
			continue
		}
		ids, err := r.mongo.FindIDsByField(ctx, r.resolver.Active(v.Name()), e.JoinColumn(), upstreamID)
		if err != nil {
			r.metrics.inc(r.topic, v.Name(), upstreamRecomposeStageDiscover)
			r.logger.Error("upstream.recompose.discover",
				"subscription", r.topic,
				"collection", r.collection,
				"view", v.Name(),
				"upstreamID", upstreamID,
				"err", err)
			r.recordFailure(ctx, v.Name(), upstreamID, "", ProjectionFailureStageDiscover, err)
			return nil, true
		}
		for _, id := range ids {
			add(id)
		}
	}
	return ordered, false
}

// recordFailure persists one ripple failure row in the unified ledger
// (kind=ripple). Best-effort: any database error is logged at Warn and
// discarded — the failure isolation contract requires that the caller's offset
// advances regardless of side-channel writes. Skipped entirely when eng is nil
// (test scaffolds that drive ripple without a relational handle).
func (r *embedRippler) recordFailure(
	ctx context.Context,
	viewName, upstreamID, localID string,
	stage ProjectionFailureStage,
	cause error,
) {
	if r.eng == nil {
		return
	}
	msg := ""
	if cause != nil {
		msg = cause.Error()
	}
	rec := ProjectionFailureRecord{
		Kind:          ProjectionFailureKindRipple,
		ConsumerGroup: r.group,
		Topic:         r.topic,
		AggregateType: viewName,
		AggregateID:   upstreamID,
		LocalID:       localID,
		Stage:         stage,
		Error:         msg,
	}
	if err := RecordProjectionFailure(ctx, r.eng.Querier(), r.eng.Dialect(), rec); err != nil {
		r.logger.Warn("upstream.recompose.record_failure_failed",
			"subscription", r.topic,
			"view", viewName,
			"upstreamID", upstreamID,
			"localID", localID,
			"stage", stage,
			"err", err)
	}
}

// resolveFailures marks any pending ripple row for (source, view, upstream_id)
// as resolved. Best-effort + nil-safe like recordFailure.
func (r *embedRippler) resolveFailures(ctx context.Context, viewName, upstreamID string) {
	if r.eng == nil {
		return
	}
	if err := ResolveProjectionFailure(ctx, r.eng.Querier(), r.eng.Dialect(),
		r.group, ProjectionFailureKindRipple, r.topic, viewName, upstreamID); err != nil {
		r.logger.Warn("upstream.recompose.resolve_failures_failed",
			"subscription", r.topic,
			"view", viewName,
			"upstreamID", upstreamID,
			"err", err)
	}
}
