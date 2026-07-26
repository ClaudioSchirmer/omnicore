package query

import (
	"context"
	"fmt"
	"log"

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

// surgicalEmbedStages builds the single $set stage for one upstream event
// (after == nil means the mirror doc was deleted). Returns nil when no embed
// contributes a $set (nothing to edit surgically), so the caller falls back to
// a full recompose. Embeds are single-level, so every embed edits in place —
// there is no nested content to force the fallback.
func surgicalEmbedStages(embeds []embedDef, upstreamID string, after Document) []Document {
	set := Document{}
	for _, e := range embeds {
		if e.many {
			set[e.field] = surgicalManyExpr(e, upstreamID, after)
			continue
		}
		set[e.field] = surgicalOneExpr(e, upstreamID, after)
	}
	if len(set) == 0 {
		return nil
	}
	return []Document{{"$set": set}}
}

// surgicalManyExpr edits a 1:N array: strip the element by its _id, then — on
// the parent the event's FK names — append the new element. Applied to any
// other parent (the old side of a move, a delete) the strip alone stands.
func surgicalManyExpr(e embedDef, upstreamID string, after Document) Document {
	strip := Document{"$filter": Document{
		"input": Document{"$ifNull": []any{"$" + e.field, []any{}}},
		"as":    "it",
		"cond":  Document{"$ne": []any{"$$it._id", lit(upstreamID)}},
	}}
	fkVal := docFieldString(after, e.Source().SchemaDef().FKColumn())
	if fkVal == "" {
		return strip
	}
	return Document{"$cond": []any{
		Document{"$eq": []any{"$_id", lit(fkVal)}},
		Document{"$concatArrays": []any{strip, []any{lit(surgicalElement(upstreamID, after))}}},
		strip,
	}}
}

// surgicalOneExpr edits a 1:1 sub-document on the parents whose FK column
// names the changed upstream id: the new element, or the explicit null the
// unresolved contract requires when the source was deleted. Other parents keep
// their stored value untouched.
func surgicalOneExpr(e embedDef, upstreamID string, after Document) Document {
	var val any
	if after == nil {
		val = lit(nil)
	} else {
		val = lit(surgicalElement(upstreamID, after))
	}
	return Document{"$cond": []any{
		Document{"$eq": []any{"$" + e.JoinColumn(), lit(upstreamID)}},
		val,
		"$" + e.field,
	}}
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
func repairDanglingOneToOne(ctx context.Context, mongo ReadModelStore, resolver *ViewResolver, eng core.RelationalEngine, view *ViewDefinition, id string, written Document) {
	for _, e := range view.embeds {
		if e.many || !e.source.isMongo {
			continue
		}
		joinCol := e.JoinColumn()
		fk, has := written[joinCol]
		if !has || fk == nil {
			continue
		}
		fkStr := fmt.Sprintf("%v", fk)
		var val any
		docs, err := mongo.FindManyByField(ctx, resolver.Active(e.source.table), "_id", fk)
		if err != nil {
			// Best-effort by design (the main write already succeeded), but never
			// silent: a systematic Mongo failure here would leave 1:1 segments
			// dangling with no trace to diagnose by.
			log.Printf("sync: WARNING — 1:1 embed repair read failed on view %q doc %q (segment %q): %v — segment stays repairable by the next event",
				view.name, id, e.field, err)
			continue
		}
		if len(docs) == 0 {
			val = lit(nil)
		} else {
			val = lit(docs[0])
		}
		stages := []Document{{"$set": Document{
			e.field: Document{"$cond": []any{
				Document{"$and": []any{
					Document{"$eq": []any{"$" + joinCol, lit(fkStr)}},
					Document{"$ne": []any{"$" + e.field + "._id", lit(fkStr)}},
				}},
				val,
				"$" + e.field,
			}},
		}}}
		if aerr := mongo.ApplyProjection(ctx, resolver.Active(view.name), id, stages, false); aerr != nil {
			log.Printf("sync: WARNING — 1:1 embed repair write failed on view %q doc %q (segment %q): %v — segment stays repairable by the next event",
				view.name, id, e.field, aerr)
			continue
		}
		if shadow, on := resolver.ShadowActive(view.name); on {
			dualApplyShadow(ctx, eng, resolver, view.name, func() error {
				return mongo.ApplyProjection(ctx, shadow, id, stages, false)
			})
		}
	}
}
