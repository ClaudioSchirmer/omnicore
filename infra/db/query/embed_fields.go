package query

import (
	"fmt"
	"strings"
)

// idGoFieldName is the fixed Go-side name of every identity (the Entity/AVO
// contract locks it — see core's idGoField). A Fields allowlist never declares
// it: the identity is always materialized and always translatable.
const idGoFieldName = "ID"

// Fields trim machinery — the storage side of Leg.Fields (JoinView-only).
//
// The allowlist is declared in GO names (leg.go); the stored document is
// column-keyed, so every writer of a segment trims through a PHYSICAL allowset
// derived here. One derivation, every funnel: the composer (per-row and batch),
// the surgical ripple, the 1:1 repair and the EmbedInChild $map all call
// trimToFields with the same set, so a trimmed segment is byte-identical no
// matter which writer produced it.

// embedTrimSet derives the physical allowset for one root embed. Nil when the
// leg declares no Fields (no trim — the historical whole-document shape).
// Forced members the consumer never declares: the source view's physical ID
// column (identity is always materialized — `_id` survives via the reserved-
// prefix rule, the physical column is added here so reads keyed on it stay
// symmetric), the leg-side join column of an EmbedMany (the segment's link back
// to the parent) and a declared OrderBy column (every writer sorts by it).
func embedTrimSet(e embedDef) map[string]struct{} {
	set := legFieldsColumnSet(e.leg)
	if set == nil {
		return nil
	}
	if e.many && e.joinCol != "" {
		set[e.joinCol] = struct{}{}
	}
	if e.orderBy != "" {
		set[e.orderBy] = struct{}{}
	}
	return set
}

// childEmbedTrimSet derives the physical allowset for one EmbedInChild
// enrichment. The element's ParentID column lives on the CHILD element, not in
// the enrichment segment, so only the leg translation + the source ID column
// apply. Nil when the leg declares no Fields.
func childEmbedTrimSet(ce childEmbedDef) map[string]struct{} {
	return legFieldsColumnSet(ce.leg)
}

// legFieldsColumnSet translates a JoinView leg's Go-name allowlist into the
// physical keys of the source view's document: a root field resolves through
// the source schema's read translator, a top-level segment (child / embed /
// role of the source) contributes its doc field. Unresolvable entries are
// skipped here — boot validation (appendLegFieldsProblems) already rejected
// them, so a running service never reaches this branch with one. The source
// view's physical ID column is always included.
func legFieldsColumnSet(leg *Leg) map[string]struct{} {
	if leg == nil || leg.view == nil || len(leg.fields) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(leg.fields)+1)
	var node *ViewNode
	for _, f := range leg.fields {
		if r, ok := leg.view.schema.Resolve(f); ok {
			set[r.Column] = struct{}{}
			continue
		}
		if node == nil {
			node = leg.view.BuildViewNode()
		}
		if emb, ok := node.embeds[f]; ok {
			set[emb.docField] = struct{}{}
		}
	}
	if pk := leg.view.schema.IDColumn(); pk != "" {
		set[pk] = struct{}{}
	}
	return set
}

// trimToFields returns doc narrowed to the allowset. A nil set (no Fields
// declared) returns the document untouched. Reserved `_`-prefixed fields
// (_id, _revision, _base_revision) ALWAYS survive: element identity and the
// surgical watermark guard depend on them. Returns a fresh map; the input is
// never mutated (mirror/view documents are shared with other consumers).
func trimToFields(doc Document, allow map[string]struct{}) Document {
	if allow == nil || doc == nil {
		return doc
	}
	out := make(Document, len(allow)+3)
	for k, v := range doc {
		if strings.HasPrefix(k, "_") {
			out[k] = v
			continue
		}
		if _, ok := allow[k]; ok {
			out[k] = v
		}
	}
	return out
}

// trimDocsToFields maps trimToFields over a 1:N result set. Nil set returns
// the slice untouched.
func trimDocsToFields(docs []Document, allow map[string]struct{}) []Document {
	if allow == nil || len(docs) == 0 {
		return docs
	}
	out := make([]Document, len(docs))
	for i, d := range docs {
		out[i] = trimToFields(d, allow)
	}
	return out
}

// appendLegFieldsProblems boot-validates a leg's declared Fields at its use
// site in the materialized Embed family. what describes the declaration site
// for the diagnostic. Rules:
//   - Fields is JOINVIEW-ONLY: an external (JoinUpstream) leg carrying one is
//     rejected — its NewExternalSchema is already the consumer's own
//     declaration of what it reads;
//   - every entry must resolve on the SOURCE view, in GO vocabulary: a root
//     field by Go name (managed slots by their fixed names — "DeletedAt",
//     "CreatedAt", "ParentID") or a top-level segment by its Go segment name.
//
// Emptiness, duplicates and reserved `_` entries already panicked at the
// declaration itself (Leg.Fields).
func appendLegFieldsProblems(acc []string, viewName, what string, leg *Leg) []string {
	if leg == nil || len(leg.fields) == 0 {
		return acc
	}
	if leg.view == nil {
		return append(acc, fmt.Sprintf(
			"view %q: %s declares Fields(...) on a JoinUpstream leg — Fields is available only on a "+
				"query.JoinView leg (its schema is the WRITE side's, not narrowable). An external leg needs no "+
				"narrowing device: the NewExternalSchema already declares which mirror columns this consumer "+
				"reads, and the subscription yaml `fields:` is the storage cut of the mirror itself.",
			viewName, what))
	}
	if leg.view.schema == nil {
		return acc // missing-schema problem reported elsewhere
	}
	var node *ViewNode
	for _, f := range leg.fields {
		if _, ok := leg.view.schema.Resolve(f); ok {
			continue
		}
		if node == nil {
			node = leg.view.BuildViewNode()
		}
		if _, ok := node.embeds[f]; ok {
			continue
		}
		acc = append(acc, fmt.Sprintf(
			"view %q: %s declares Fields entry %q, which is neither a Go field of the source view %q nor one "+
				"of its top-level segments — entries are GO names (business fields by their declared Go name, "+
				"managed slots by their fixed names: \"DeletedAt\", \"CreatedAt\", \"UpdatedAt\", \"ParentID\"), "+
				"and a segment name admits or cuts that segment whole.",
			viewName, what, f, leg.view.Name()))
	}
	return acc
}
