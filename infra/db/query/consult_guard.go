package query

import "encoding/json"

// Revision-guarded writes for CONSULT-composed documents — the pipeline form
// every consult writer (SyncEngine recompose, shared-base fan-out by consult,
// the rebuild/verify backfill) uses instead of a full-document $set.
//
// A composed document already carries its watermarks: the composer remaps the
// physical revision column of the ROOT row into _revision and of the shared
// BASE row into _base_revision (remapRevision), and it reads the root/base row
// FIRST — every related read (siblings, children, role segments) happens
// after. That order is load-bearing: the document's data is always AT LEAST as
// fresh as the watermark claims (stamp-first), so applying the document behind
// a `stored < incoming` guard can never regress a fresher write, and a torn
// composition (a concurrent commit landing between the root read and a related
// read) is self-healing — the interfering write's own event recomposes at a
// HIGHER watermark and overwrites. Equal watermarks skip the write: with the
// base revision advancing on every identity-touching write, an equal revision
// means an identical closure state, so there is nothing to lose.
//
// Scopes:
//   - the aggregate's OWN data (root scalars, own children, siblings, the PK)
//     guards on _revision;
//   - shared-base data (base scalars + base-children segments) guards on
//     _base_revision — for a SharedBaseView document the composer stamps the
//     base row's revision (the base revision) into _revision, and the WHOLE
//     document (base scalars, base children, role segments) is one
//     revision-guarded scope;
//   - EMBED segments keep the recompose-ripple's ownership: written only when
//     this upsert CREATES the document (the fieldOwnershipStages rule) — they
//     are ordered by the mirror's own writes, not by the relational revision.
//
// The watermark itself only ever advances (guardedSetStage keeps the maximum),
// so a late consult can no longer lower the bar for the payload-direct guards.

// scopeShape tells the equal-revision fill how to descend into a scope's
// STRUCTURED fields — a new column can surface at any level of the document
// (root scalar, sibling, shared-base scalar, a child element's column, a role
// segment's scalar), so key-level fill alone would miss additions nested
// inside a value that already exists:
//
//   - arraySegs maps a child-collection segment to its element PK column: at
//     the equal revision each STORED element shallow-merges the composed
//     element with the same PK (stored keys win; composed supplies only the
//     keys the element lacks — the columns a previous-binary writer's schema
//     could not see);
//   - objectSegs names sub-document segments (a SharedBaseView's role
//     segments): the stored sub-document shallow-merges the composed one, same
//     stored-wins rule.
//
// One honest level limit: the merge is SHALLOW per structure — an array nested
// INSIDE an object segment (a role segment's own child collection on a person
// document) keeps the stored array whole; additions there converge on the next
// write of that role or on a rebuild.
type scopeShape struct {
	arraySegs  map[string]string
	objectSegs map[string]bool
}

// consultGuardedStages renders the guarded pipeline for one consult-composed
// document of view. A document composed without a watermark (defensive — every
// root schema declares Revision) falls back to an unguarded $set of that
// scope, preserving the historical full-overwrite semantics.
func consultGuardedStages(view *ViewDefinition, doc Document) []Document {
	ownRev := watermarkOf(doc[docRevisionField])
	baseRev := watermarkOf(doc[docBaseRevisionField])
	embedFields := embedFieldSet(view.embeds)
	pk := schemaPK(view.schema)

	baseCols := map[string]bool{}
	ownShape := scopeShape{arraySegs: map[string]string{}, objectSegs: map[string]bool{}}
	baseShape := scopeShape{arraySegs: map[string]string{}, objectSegs: map[string]bool{}}
	if view.isSharedBaseView {
		// One revision-guarded scope: base children merge per element, role
		// segments merge as sub-documents.
		for _, bc := range view.schema.ChildSchemas() {
			ownShape.arraySegs[childDocSegment(bc)] = bc.PKColumn()
		}
		for _, r := range view.roles {
			ownShape.objectSegs[r.segment] = true
		}
	} else {
		for _, c := range view.schema.SharedBaseBusinessColumns() {
			baseCols[c] = true
		}
		for _, ch := range view.schema.ChildSchemas() {
			ownShape.arraySegs[childDocSegment(ch)] = ch.PKColumn()
		}
		if base, _, ok := view.schema.SharedBaseRef(); ok {
			for _, bc := range base.ChildSchemas() {
				seg := childDocSegment(bc)
				baseCols[seg] = true
				baseShape.arraySegs[seg] = bc.PKColumn()
			}
		}
	}

	own := Document{}
	base := Document{}
	embeds := Document{}
	for k, v := range doc {
		switch {
		case k == "_id" || k == docRevisionField || k == docBaseRevisionField:
			// watermarks travel via their guard stages, never as data
		case embedFieldOf(embedFields, k):
			embeds[k] = v
		case baseCols[k]:
			base[k] = v
		default:
			own[k] = v
		}
	}

	// Stage ORDER is load-bearing: the embed-ownership stage probes document
	// EXISTENCE via the PK field, and the own stage right after it SETS the PK
	// — pipeline stages are sequential, so the probe must run first or a fresh
	// upsert-insert would see the PK its own pipeline just wrote and skip the
	// embeds it should materialize.
	stages := make([]Document, 0, 3)
	if len(embeds) > 0 {
		stages = append(stages, embedCreateStage(embeds, pk, embedOrders(view.embeds)))
	}
	if len(own) > 0 {
		stages = append(stages, scopeStage(docRevisionField, own, ownRev, ownShape))
	}
	if len(base) > 0 {
		stages = append(stages, scopeStage(docBaseRevisionField, base, baseRev, baseShape))
	}
	return stages
}

// scopeStage guards one scope behind its watermark; a scope composed without
// one (rev == 0) writes unguarded — the defensive legacy form.
//
// Unlike the payload-direct guard (which applies its full carried state at the
// equal revision — the payload is the emitting transaction's own truth), a
// CONSULT scope is a READ snapshot, so at the equal revision it only FILLS
// MISSING FIELDS — stored data wins: a field the fresh
// composition produced but the document lacks is written even when the stored
// watermark equals the incoming one. "Equal revision ⇒ identical data" holds
// only when every writer runs the same schema — during a rolling deploy a pod
// on the previous binary projects an event WITHOUT the columns its schema does
// not know, leaving a document at the current revision missing exactly those
// fields, and no later event ever rewrites them. The fill closes that window
// by construction for additive changes: the composition is a consistent
// snapshot of the same revision, so a key it carries and the document lacks
// can only be schema blindness (or a torn write) — never a legitimate removal,
// because removals drop the key from the COMPOSITION too (shape parity: the
// composer omits a missing sibling row / a vanished segment the same way the
// projector does). Present fields are never touched at the equal revision, and
// nothing at all applies below it — the fill is add-only, the guard stays
// monotone.
func scopeStage(watermarkField string, set Document, rev int64, shape scopeShape) Document {
	if rev <= 0 {
		unguarded := Document{}
		for col, v := range set {
			unguarded[col] = lit(v)
		}
		return Document{"$set": unguarded}
	}
	newer := Document{"$lt": []any{
		Document{"$ifNull": []any{"$" + watermarkField, int64(-1)}},
		rev,
	}}
	equal := Document{"$eq": []any{"$" + watermarkField, rev}}
	out := Document{}
	for col, v := range set {
		out[col] = Document{"$cond": []any{
			newer, lit(v),
			Document{"$cond": []any{equal, equalRevisionExpr(col, v, shape), "$" + col}},
		}}
	}
	out[watermarkField] = Document{"$cond": []any{
		newer, rev, Document{"$ifNull": []any{"$" + watermarkField, rev}},
	}}
	return Document{"$set": out}
}

// equalRevisionExpr renders one field's EQUAL-REVISION fill expression —
// stored data always wins; the fresh composition only supplies what is absent,
// at the granularity the field's shape allows:
//
//   - child-collection segment (shape.arraySegs): each stored element
//     shallow-merges the composed element with the same PK ($mergeObjects with
//     the stored element LAST, so its keys win and the composed one only adds
//     the missing columns). Elements only the stored array carries survive
//     untouched; the stored element SET is never changed. A stored value that
//     is not an array (or absent) takes the composed value only when missing.
//   - sub-document segment (shape.objectSegs): the stored sub-document
//     shallow-merges the composed one, same stored-wins rule. A stored
//     explicit null (a vanished role's segment) is NOT an object and stays.
//   - scalar (everything else): composed value only when the field is absent.
func equalRevisionExpr(col string, v any, shape scopeShape) Document {
	missing := Document{"$eq": []any{Document{"$type": "$" + col}, "missing"}}
	fillIfMissing := Document{"$cond": []any{missing, lit(v), "$" + col}}
	if pkCol, ok := shape.arraySegs[col]; ok {
		matched := Document{"$ifNull": []any{
			Document{"$arrayElemAt": []any{
				Document{"$filter": Document{
					"input": lit(v),
					"as":    "fresh",
					"cond":  Document{"$eq": []any{"$$fresh." + pkCol, "$$stored." + pkCol}},
				}},
				0,
			}},
			Document{},
		}}
		return Document{"$cond": []any{
			Document{"$eq": []any{Document{"$type": "$" + col}, "array"}},
			Document{"$map": Document{
				"input": "$" + col,
				"as":    "stored",
				"in":    Document{"$mergeObjects": []any{matched, "$$stored"}},
			}},
			fillIfMissing,
		}}
	}
	if shape.objectSegs[col] {
		return Document{"$cond": []any{
			Document{"$eq": []any{Document{"$type": "$" + col}, "object"}},
			Document{"$mergeObjects": []any{lit(v), "$" + col}},
			fillIfMissing,
		}}
	}
	return fillIfMissing
}

// embedCreateStage renders the recompose-ripple ownership rule as a pipeline
// stage: each embed segment keeps its stored value on an EXISTING document
// (the ripple owns it) and materializes the composed value only when this
// upsert is creating the document. Existence is probed via the root PK column
// — absent on a fresh upsert-insert, present on every materialized document.
func embedCreateStage(embeds Document, pkCol string, orders map[string]*embedOrder) Document {
	exists := Document{"$ne": []any{Document{"$type": "$" + pkCol}, "missing"}}
	set := Document{}
	for k, v := range embeds {
		set[k] = Document{"$cond": []any{exists, "$" + k, sortedSegment(lit(v), orders[k])}}
	}
	return Document{"$set": set}
}

// embedFieldOf reports whether k is a declared embed segment.
func embedFieldOf(embedFields map[string]struct{}, k string) bool {
	_, ok := embedFields[k]
	return ok
}

// watermarkOf normalizes a composed watermark value to int64 — the relational
// scan yields int64 on every engine, but a defensive spread keeps a decoded
// numeric form from silently degrading the guard to the legacy unguarded path.
func watermarkOf(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int32:
		return int64(n)
	case int:
		return int64(n)
	case float64:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	default:
		return 0
	}
}
