package query

import (
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// The payload-direct projector: turns one decoded v2 event into the update
// PIPELINE the store applies atomically (ApplyProjection). No relational read
// happens on this path — the payload IS the state:
//
//   - own scalars (role/root ∪ siblings ∪ managed timestamps) $set
//     unconditionally — single writer per aggregate, ordered by the broker key;
//   - shared-base scalars $set behind the base-revision guard: they apply only
//     when the document's stored `_base_revision` is older than the event's —
//     the relational row lock serialized the writers, the guard replays that
//     order here, so the LAST writer into the base wins on every document no
//     matter how consumers interleave (maintainer requirement);
//   - child collections edit SURGICALLY (per-element by child PK), never by
//     whole-array replace: the document keeps archived child history the
//     payload does not carry. Base-children elements carry a per-element
//     `_rev` stamp and each op applies only over an older element — the same
//     last-writer-wins guarantee at element grain (two roles racing on one
//     shared child).
//
// A document written by this path is shaped exactly like the composer's (the
// equivalence gate of the phase); `_revision`, `_base_revision` and the
// elements' `_rev` are the framework-internal additions, inside the reserved
// `_` namespace. ACCEPTED divergence: consult-composed base-child elements
// carry no `_rev` (rows have no per-child revision to read) — the guard
// treats a missing `_rev` as older, and every `_`-prefixed field is excluded
// from doc-a-doc comparisons (verify's shape check included).

// docRevisionField is the document-level watermark of the aggregate's OWN
// data (root/sibling scalars + own children) — the event's _ids.revision must
// beat it or the whole own-data part of the pipeline is a no-op. This is the
// zombie-consumer defense: a slow pod finishing an in-flight event after a
// partition handoff carries an older revision and cannot regress the document.
const docRevisionField = "_revision"

// docBaseRevisionField is the document-level watermark of shared-base data.
const docBaseRevisionField = "_base_revision"

// elemRevisionField is the per-element watermark of a base-child entry.
const elemRevisionField = "_rev"

// lit wraps a decoded value as a pipeline literal — in an update pipeline every
// value is an aggregation EXPRESSION, so a raw "$..."-shaped string would read
// as a field path; $literal makes the value inert.
func lit(v any) Document { return Document{"$literal": v} }

// buildProjectionStages assembles the pipeline for one v2 event on an
// entity-rooted view (root = the aggregate's table; SharedBaseViews keep the
// consult path). eventType is one of the upsert verbs — DELETED never lands
// here (the sync engine deletes the document instead).
func buildProjectionStages(schema *core.TableSchema, ev *decodedEvent) []Document {
	baseCols := map[string]bool{}
	for _, c := range schema.SharedBaseBusinessColumns() {
		baseCols[c] = true
	}
	own := Document{}
	base := Document{}
	for col, v := range ev.Scalars {
		if baseCols[col] {
			base[col] = lit(v)
			continue
		}
		own[col] = lit(v)
	}
	// Stage ORDER is load-bearing: pipeline stages are sequential, so every
	// own-data stage guarded by the _revision watermark must run BEFORE the
	// stage that advances the watermark (the own-scalars set below).
	stages := make([]Document, 0, 6)
	stages = append(stages, ownGuardedChildStages(schema, ev)...)
	stages = append(stages, childStages(schema, ev.BaseChildren, ev.IDs.BaseRevision, true)...)
	if len(base) > 0 && ev.IDs.BaseID != "" {
		stages = append(stages, baseGuardedSetStage(base, ev.IDs.BaseRevision))
	}
	if len(own) > 0 {
		stages = append(stages, guardedSetStage(docRevisionField, own, ev.IDs.Revision))
	}
	return stages
}

// ownGuardedChildStages renders the OWN child edits, each array expression
// wrapped in the document-revision guard (reading the still-unchanged
// watermark — these stages precede the own-scalars stage that advances it).
// A pre-4b3 payload (revision 0) applies unguarded.
func ownGuardedChildStages(schema *core.TableSchema, ev *decodedEvent) []Document {
	stages := childStages(schema, ev.Children, 0, false)
	if ev.IDs.Revision <= 0 {
		return stages
	}
	newer := Document{"$lt": []any{
		Document{"$ifNull": []any{"$" + docRevisionField, int64(-1)}},
		ev.IDs.Revision,
	}}
	for _, st := range stages {
		set, _ := st["$set"].(Document)
		for seg, expr := range set {
			set[seg] = Document{"$cond": []any{newer, expr, "$" + seg}}
		}
	}
	return stages
}

// guardedSetStage renders one $set where every column applies only when the
// stored watermark (watermarkField) is older than the incoming revision — and
// the watermark itself advances monotonically. revision <= 0 (a pre-4b3
// payload) degrades to an unconditional set with no watermark write.
func guardedSetStage(watermarkField string, set Document, revision int64) Document {
	if revision <= 0 {
		return Document{"$set": set}
	}
	newer := Document{"$lt": []any{
		Document{"$ifNull": []any{"$" + watermarkField, int64(-1)}},
		revision,
	}}
	out := Document{}
	for col, v := range set {
		out[col] = Document{"$cond": []any{newer, v, "$" + col}}
	}
	out[watermarkField] = Document{"$cond": []any{
		newer, revision, Document{"$ifNull": []any{"$" + watermarkField, revision}},
	}}
	return Document{"$set": out}
}

// baseGuardedSetStage guards shared-base columns behind the base watermark.
func baseGuardedSetStage(base Document, revision int64) Document {
	return guardedSetStage(docBaseRevisionField, base, revision)
}

// childStages renders one $set stage per child operation — surgical edits on
// the child's document segment (childDocSegment), keyed by the child PK.
// guarded=true (base children) stamps each written element with the event's
// base revision and refuses to overwrite a NEWER element.
func childStages(schema *core.TableSchema, groups map[string][]childOp, revision int64, guarded bool) []Document {
	if len(groups) == 0 {
		return nil
	}
	var stages []Document
	for typeName, ops := range groups {
		child, _, ok := schema.ResolveAggregateChild(typeName)
		if !ok {
			continue
		}
		seg := childDocSegment(child)
		pk := child.PKColumn()
		for _, op := range ops {
			if op.Op == "noop" {
				continue
			}
			id, _ := op.Fields[pk].(string)
			if id == "" {
				continue // a surgical op without the element key cannot land
			}
			stages = append(stages, Document{"$set": Document{
				seg: childArrayExpr(seg, pk, id, op, revision, guarded, child),
			}})
		}
	}
	return stages
}

// childArrayExpr renders the aggregation expression producing the segment's
// new array for one op. `arr` below is the existing array (missing → []).
func childArrayExpr(seg, pk, id string, op childOp, revision int64, guarded bool, child *core.TableSchema) Document {
	arr := Document{"$ifNull": []any{"$" + seg, []any{}}}
	matches := func(ref string) Document {
		return Document{"$eq": []any{ref + "." + pk, lit(id)}}
	}
	others := Document{"$filter": Document{
		"input": arr, "cond": Document{"$not": []any{matches("$$this")}},
	}}
	switch op.Op {
	case "insert", "update":
		elem := Document{}
		for k, v := range op.Fields {
			elem[k] = v
		}
		if guarded {
			elem[elemRevisionField] = revision
			// Keep the EXISTING element instead when it is newer or equal —
			// the racing writer that entered the base later already landed.
			existing := Document{"$filter": Document{"input": arr, "cond": matches("$$this")}}
			newerExists := Document{"$gt": []any{Document{"$size": Document{"$filter": Document{
				"input": existing,
				"cond":  Document{"$gte": []any{"$$this." + elemRevisionField, revision}},
			}}}, 0}}
			return Document{"$cond": []any{
				newerExists,
				arr,
				Document{"$concatArrays": []any{others, []any{lit(elem)}}},
			}}
		}
		return Document{"$concatArrays": []any{others, []any{lit(elem)}}}
	case "archive":
		mutate := Document{"deleted_at": lit(op.Fields[softDeleteOf(child)])}
		if sd, ok := child.SoftDeleteColumn(); ok {
			mutate = Document{sd: lit(op.Fields[sd])}
		}
		if guarded {
			mutate[elemRevisionField] = revision
		}
		return Document{"$map": Document{
			"input": arr,
			"in": Document{"$cond": []any{
				matches("$$this"),
				Document{"$mergeObjects": []any{"$$this", mutate}},
				"$$this",
			}},
		}}
	case "delete":
		return others
	default:
		return arr
	}
}

// softDeleteOf returns the child's soft-delete column or "deleted_at" as the
// defensive fallback (an archive op only exists for soft-deletable children).
func softDeleteOf(child *core.TableSchema) string {
	if sd, ok := child.SoftDeleteColumn(); ok {
		return sd
	}
	return "deleted_at"
}

// buildFanOutStages renders the SHARED-IDENTITY-ONLY stages a fan-out applies
// to the OTHER roles' documents: guarded base scalars + guarded base-children
// edits. The event's role-own fields never leak into a foreign role's doc.
func buildFanOutStages(schema *core.TableSchema, ev *decodedEvent) []Document {
	base := Document{}
	for _, col := range schema.SharedBaseBusinessColumns() {
		if v, ok := ev.Scalars[col]; ok {
			base[col] = lit(v)
		}
	}
	var stages []Document
	if len(base) > 0 && ev.IDs.BaseID != "" {
		stages = append(stages, baseGuardedSetStage(base, ev.IDs.BaseRevision))
	}
	stages = append(stages, childStages(schema, ev.BaseChildren, ev.IDs.BaseRevision, true)...)
	return stages
}
