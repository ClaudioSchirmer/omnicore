package query

import (
	"context"
	"fmt"
	"log"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// Surgical embed edits — the recompose-ripple's write form for a parent
// document that already exists.
//
// The ripple used to $set each embed segment to a freshly-composed SNAPSHOT of
// the mirror. Two concurrent ripples for DIFFERENT upstream ids converging on
// the same parent (workers > 1 in one subscription, or partitions spread over
// pods) could then interleave: the older snapshot lands last and erases the
// newer one's element. Snapshot writes made cross-writer ordering matter;
// per-element edits make it irrelevant:
//
//   - an edit touches exactly the element keyed by the event's upstream id, so
//     edits for DIFFERENT ids commute — any interleaving converges to the same
//     array;
//   - events for the SAME id are already serialized end to end (broker
//     partitioning by aggregate id + the subscriber's hash-bucketed worker
//     dispatch), so the last edit for an element is the newest by construction.
//
// The element value is the mirror document just written (the decoded payload
// plus its _id) — byte-identical to what a full compose reads back from the
// mirror, so a rippled document and a rebuilt one carry the same shape. The
// ripple hot path therefore needs NO relational read at all; the full
// recompose remains only for materializing a parent whose document does not
// exist yet (and for rebuild/verify, which own their slots).
//
// One stage set serves every target parent of the event: each edit is
// conditioned on the DOCUMENT's own keys ($_id for the 1:N parent, the FK
// column for 1:1), so the same pipeline applied to the old and the new parent
// of a moved child removes the element from one and upserts it into the other.

// The srcRev watermark — why a SOURCE-side guard exists at all
//
// An upstream MIRROR has ONE writer (§8.2 forbids two subscriptions per
// collection) and its events are bucketed per aggregate id, so edits for the
// same source id are serialized end to end and the last one is the newest by
// construction — no guard needed (srcRev == 0 keeps the byte-identical stages
// that path always produced).
//
// A source VIEW (query.JoinView) has SEVERAL writers with DIFFERENT bucketing:
// the SyncEngine keys its workers by the view root's aggregate id, while a
// ripple refreshing that same view is keyed by ITS source's id. Two writes to
// one source document can therefore land out of order on the embedding
// document — the older one last, leaving a stale segment until the next event.
// So a view-sourced ripple carries the source document's watermark
// (_revision) and every edit is applied only when the STORED element is not
// newer, in the same revision-guarded style as the rest of the projection
// pipeline. srcRev <= 0 disables the guard entirely.

// embedOrder is a materialized 1:N segment's declared element order.
type embedOrder struct {
	column string
	desc   bool
}

// sortedSegment wraps a 1:N segment expression in $sortArray when the embed
// declares an order (nil = unordered, the historical shape).
//
// ONE sort implementation, and it lives HERE — in the pipeline. Every writer of
// a materialized segment already writes through a pipeline update (the consult
// projection, the rebuild's bulk apply, and the surgical ripple), so routing all
// of them through this helper is what makes them converge byte for byte. Sorting
// the composed array in Go instead would mean TWO implementations — Go's byte
// order versus the server's, which honors the view's declared collation — and
// the divergence would surface as an intermittent blue-green verify failure.
//
// The sort is TOTAL: the declared column, then `_id`. Neither the driver nor the
// server promises a stable sort on ties, so without the tiebreaker two writers
// could store different arrays for identical state. sortBy is a bson.D because
// key ORDER is semantic here — a map would marshal in random order and could
// silently make `_id` the primary key of the sort.
//
// Requires MongoDB 5.2+ ($sortArray).
func sortedSegment(expr Document, ord *embedOrder) Document {
	if ord == nil || ord.column == "" {
		return expr
	}
	dir := 1
	if ord.desc {
		dir = -1
	}
	return Document{"$sortArray": Document{
		"input":  expr,
		"sortBy": bson.D{{Key: ord.column, Value: dir}, {Key: "_id", Value: 1}},
	}}
}

// embedOrders maps each declared embed's document field to its element order
// (absent = unordered / 1:1). Handed to the stage builders so every writer of a
// segment applies the identical sort.
func embedOrders(embeds []embedDef) map[string]*embedOrder {
	out := map[string]*embedOrder{}
	for _, e := range embeds {
		if e.many && e.orderBy != "" {
			out[e.Field()] = &embedOrder{column: e.orderBy, desc: e.orderDesc}
		}
	}
	return out
}

// notNewerThan is the guard predicate: the stored element at path has no
// watermark, or a watermark not newer than srcRev.
func notNewerThan(path string, srcRev int64) Document {
	return Document{"$lte": []any{
		Document{"$ifNull": []any{path + "." + docRevisionField, lit(int64(-1))}},
		lit(srcRev),
	}}
}

// surgicalEmbedStages builds the single $set stage for one source event
// (after == nil means the source doc was deleted). srcRev is the source
// document's revision watermark (0 = unwatermarked source, e.g. an upstream
// mirror: no guard, byte-identical stages). Returns nil when no embed
// contributes a $set (nothing to edit surgically), so the caller falls back to
// a full recompose. Every embed edits in place — a nested source contributes
// its own materialized content, so there is no nested content to force the
// fallback.
func surgicalEmbedStages(embeds []embedDef, upstreamID string, after Document, srcRev int64) []Document {
	set := Document{}
	for _, e := range embeds {
		if e.many {
			set[e.Field()] = surgicalManyExpr(e, upstreamID, after, srcRev)
			continue
		}
		set[e.Field()] = surgicalOneExpr(e, upstreamID, after, srcRev)
	}
	if len(set) == 0 {
		return nil
	}
	return []Document{{"$set": set}}
}

// surgicalManyExpr edits a 1:N array: strip the element by its _id, then — on
// the parent the event's FK names — append the new element. Applied to any
// other parent (the old side of a move, a delete) the strip alone stands.
//
// Under a watermark (srcRev > 0) the whole edit is skipped for a parent whose
// STORED element for this id is newer: the strip keeps it and the append is
// suppressed, so a late-arriving older write cannot regress the array.
func surgicalManyExpr(e embedDef, upstreamID string, after Document, srcRev int64) Document {
	input := Document{"$ifNull": []any{"$" + e.Field(), []any{}}}
	stripCond := Document{"$ne": []any{"$$it._id", lit(upstreamID)}}
	var hasNewer Document
	if srcRev > 0 {
		// Is a NEWER element for this id already stored?
		hasNewer = Document{"$gt": []any{
			Document{"$size": Document{"$filter": Document{
				"input": input,
				"as":    "it",
				"cond": Document{"$and": []any{
					Document{"$eq": []any{"$$it._id", lit(upstreamID)}},
					Document{"$not": []any{notNewerThan("$$it", srcRev)}},
				}},
			}}},
			lit(int64(0)),
		}}
		// Keep every element when a newer one is stored (nothing is stripped).
		stripCond = Document{"$or": []any{stripCond, hasNewer}}
	}
	strip := Document{"$filter": Document{"input": input, "as": "it", "cond": stripCond}}
	fkVal := docFieldString(after, e.JoinColumn())
	if fkVal == "" {
		if e.orderBy != "" {
			return sortedSegment(strip, &embedOrder{column: e.orderBy, desc: e.orderDesc})
		}
		return strip
	}
	appendCond := Document{"$eq": []any{"$_id", lit(fkVal)}}
	if srcRev > 0 {
		appendCond = Document{"$and": []any{appendCond, Document{"$not": []any{hasNewer}}}}
	}
	var ord *embedOrder
	if e.orderBy != "" {
		ord = &embedOrder{column: e.orderBy, desc: e.orderDesc}
	}
	return sortedSegment(Document{"$cond": []any{
		appendCond,
		Document{"$concatArrays": []any{strip, []any{lit(surgicalElement(upstreamID, after))}}},
		strip,
	}}, ord)
}

// surgicalOneExpr edits a 1:1 sub-document on the parents whose FK column
// names the changed source id: the new element, or the explicit null the
// unresolved contract requires when the source was deleted. Other parents keep
// their stored value untouched. Under a watermark (srcRev > 0) a stored
// segment newer than this event is kept too.
func surgicalOneExpr(e embedDef, upstreamID string, after Document, srcRev int64) Document {
	var val any
	if after == nil {
		val = lit(nil)
	} else {
		val = lit(surgicalElement(upstreamID, after))
	}
	cond := Document{"$eq": []any{"$" + e.JoinColumn(), lit(upstreamID)}}
	if srcRev > 0 {
		cond = Document{"$and": []any{cond, notNewerThan("$"+e.Field(), srcRev)}}
	}
	return Document{"$cond": []any{cond, val, "$" + e.Field()}}
}

// surgicalElement is the array/sub-doc element as the mirror stores it — the
// filtered payload plus its _id, matching what FindManyByField returns to a
// full compose.
func surgicalElement(id string, after Document) Document {
	elem := make(Document, len(after)+1)
	for k, v := range after {
		elem[k] = v
	}
	elem["_id"] = id
	return elem
}

// repairDanglingOneToOne closes the LAST ordering window a 1:1 embed has: a
// document written with an FK whose segment does not (or no longer) match it.
// Two producers of that state exist —
//
//   - CREATE raced the mirror: the composing read ran before the referenced
//     mirror doc was written, so the document materialized with a null
//     segment, and the mirror doc's own insert ripple scanned the view before
//     this document existed (both sides missed);
//   - FK CHANGE under field ownership: a consult update rewrote the FK column
//     but, by design, never touches embed segments — the stored segment still
//     holds the previously-referenced element.
//
// The repair runs AFTER the document write and re-reads the mirror fresh:
// either the mirror doc is visible by then (repair heals it here), or it is
// not — in which case its insert ripple, which necessarily runs after the
// mirror write, finds this already-written document by its FK scan and heals
// it there. One of the two always fires; no timing assumption.
//
// The write carries a double guard — FK still matches AND the stored segment's
// _id is NOT already the FK — so a repair can never regress the segment
// written by the element's own ripple (per-id serialized, always fresher):
// same id → no-op. A missing mirror doc repairs the segment to the explicit
// null under the same guard (a stale element from a dead reference clears; a
// later mirror insert re-heals through its own ripple).
// chain, when non-nil, receives (viewName, id) after a repair actually wrote —
// the repair edits an embed segment of a VIEW document, so a view materializing
// THIS one must learn about it too.
func repairDanglingOneToOne(ctx context.Context, mongo ReadModelStore, resolver *ViewResolver, eng core.RelationalEngine, view *ViewDefinition, id string, written Document, chain func(context.Context, string, string)) {
	for _, e := range view.embeds {
		// 1:1 only (a 1:N segment has no single FK to dangle), and only a
		// materialized source — BOTH kinds: an upstream mirror AND a local view
		// (query.JoinView). A view leg needs this repair MORE than a mirror does,
		// because an FK change on the embedding document is otherwise permanent
		// staleness: field ownership keeps the consult write off the segment, and
		// the source view's own ripple never fires (nothing changed on ITS side).
		if e.many || e.leg == nil {
			continue
		}
		joinCol := e.JoinColumn()
		fk, has := written[joinCol]
		if !has || fk == nil {
			continue
		}
		fkStr := fmt.Sprintf("%v", fk)
		var val any
		docs, err := mongo.FindManyByField(ctx, resolver.Active(e.leg.Collection()), "_id", fk)
		if err != nil {
			// Best-effort by design (the main write already succeeded), but never
			// silent: a systematic Mongo failure here would leave 1:1 segments
			// dangling with no trace to diagnose by.
			log.Printf("sync: WARNING — 1:1 embed repair read failed on view %q doc %q (segment %q): %v — segment stays repairable by the next event",
				view.name, id, e.Field(), err)
			continue
		}
		if len(docs) == 0 {
			val = lit(nil)
		} else {
			val = lit(docs[0])
		}
		stages := []Document{{"$set": Document{
			e.Field(): Document{"$cond": []any{
				Document{"$and": []any{
					Document{"$eq": []any{"$" + joinCol, lit(fkStr)}},
					Document{"$ne": []any{"$" + e.Field() + "._id", lit(fkStr)}},
				}},
				val,
				"$" + e.Field(),
			}},
		}}}
		if aerr := mongo.ApplyProjection(ctx, resolver.Active(view.name), id, stages, false); aerr != nil {
			log.Printf("sync: WARNING — 1:1 embed repair write failed on view %q doc %q (segment %q): %v — segment stays repairable by the next event",
				view.name, id, e.Field(), aerr)
			continue
		}
		if shadow, on := resolver.ShadowActive(view.name); on {
			dualApplyShadow(ctx, eng, resolver, view.name, func() error {
				return mongo.ApplyProjection(ctx, shadow, id, stages, false)
			})
		}
		if chain != nil {
			chain(ctx, view.name, id)
		}
	}
}
