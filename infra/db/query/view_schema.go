package query

import (
	"reflect"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// ViewNode is the per-view, per-doc-path translator between Go field paths
// (PascalCase, what criteria/Response speak) and physical column paths (what
// the Mongo document stores — a mirror of PG). It is a tree: the root node maps
// the view's root leaf fields and points to one child node per embed, keyed by
// the embed's Go segment (for Go→column) and by its doc field (for column→Go).
//
// Built once from the ViewDefinition's core.TableSchema tree; consumed by the
// MongoViewReader to translate Filter/Sort/Projection on the way in and the
// returned document on the way out. The translation is lossless because each
// TableSchema is a complete bijection.
type ViewNode struct {
	schema      *core.TableSchema
	embeds      map[string]*viewEmbed // goSegment → embed
	embedsByDoc map[string]*viewEmbed // docField → embed
}

type viewEmbed struct {
	goSegment string
	docField  string
	node      *ViewNode
	// isChild marks a derived aggregate-child collection (a shared base's
	// native child or a schema's own child) as opposed to an explicitly
	// declared embed. Only child collections carry the aggregate's soft-delete
	// lifecycle, so only they are subject to the reader's archived-entry strip.
	isChild bool
	// isRole marks a SharedBaseView role segment: a SINGLE optional
	// sub-document (not a collection) carrying the role's own lifecycle — an
	// archived role's segment is hidden on default reads, and the role's own
	// child collections strip recursively.
	isRole bool
	// isViewLeg marks a segment materialized from a LOCAL view (query.JoinView):
	// the stored sub-document is a copy of that view's own document, so the
	// reader must treat its INSIDES exactly as a direct read of the source view
	// would — the archived-entry strip and the soft-delete auto-include descend
	// into it. An EXTERNAL (mirror) leg does not: a cross-service source's
	// lifecycle belongs to its upstream, and the mirror is stored as received.
	isViewLeg bool
}

// BuildViewNode assembles the translator tree for a view. On a SharedBaseView
// it additionally registers one embed per declared role — a single-map segment
// whose node translates the role's own fields (plus its siblings, merged flat
// by the composer) and its own children. The role node deliberately suppresses
// base-children registration: the base's native collections (e.g. Addresses)
// project at the ROOT of the person document, never inside a role segment.
// (The role schema's ColumnForRead also resolves the base's shared fields —
// harmless here: a `role.sharedField` path translates but matches nothing,
// because the composer lands shared fields at the root only.)
func (v *ViewDefinition) BuildViewNode() *ViewNode {
	n := newViewNode(v.schema, v.embeds)
	for _, r := range v.roles {
		ve := &viewEmbed{goSegment: r.segment, docField: r.segment, node: newRoleViewNode(r.schema), isRole: true}
		n.embeds[r.segment] = ve
		n.embedsByDoc[r.segment] = ve
	}
	// EmbedInChild: register the enriched sub-document INSIDE the native child's
	// node, so ToGoDoc translates the nested segment (column→Go) on read and
	// ColumnPath resolves a filter/sort/?fields= path into it
	// (e.g. "catalogLines.item.label"). The native child node was registered by
	// registerOwnChildren / the SharedBaseRef branch and is keyed by the child's
	// doc segment. Registered as a plain embed (NOT isChild): the enrichment is
	// external data with no aggregate lifecycle, so the archived-entry strip must
	// not touch it.
	for _, ce := range v.childEmbeds {
		childVE, ok := n.embedsByDoc[ce.ChildSegment()]
		if !ok || childVE.node == nil {
			continue // boot-validated to be a native child; defensive
		}
		seg := ce.leg.goSegment
		ive := &viewEmbed{goSegment: seg, docField: ce.Field(), node: legViewNode(ce.leg), isViewLeg: ce.leg.view != nil}
		childVE.node.embeds[seg] = ive
		childVE.node.embedsByDoc[ce.Field()] = ive
	}
	return n
}

// legViewNode builds the translator node for one embed leg: a VIEW leg
// (query.JoinView) resolves through the source view's own BuildViewNode — the
// materialized segment mirrors a direct read of that view, embeds and children
// included, and the recursion terminates because the embed graph is acyclic
// (appendEmbedCycles). An EXTERNAL leg (query.JoinUpstream) is a flat mirror
// schema with no closure of its own.
func legViewNode(leg *Leg) *ViewNode {
	if leg.view != nil {
		return leg.view.BuildViewNode()
	}
	return newViewNode(leg.schema, nil)
}

// newRoleViewNode builds the translator node for one role segment: the role's
// own children register (they nest inside the segment), base-children do NOT
// (they live at the person document's root).
func newRoleViewNode(schema *core.TableSchema) *ViewNode {
	n := &ViewNode{
		schema:      schema,
		embeds:      map[string]*viewEmbed{},
		embedsByDoc: map[string]*viewEmbed{},
	}
	registerOwnChildren(n, schema)
	return n
}

func newViewNode(schema *core.TableSchema, embeds []embedDef) *ViewNode {
	n := &ViewNode{
		schema:      schema,
		embeds:      map[string]*viewEmbed{},
		embedsByDoc: map[string]*viewEmbed{},
	}
	for _, e := range embeds {
		if e.leg == nil {
			continue
		}
		// Parent-side Go segment: the mandatory goName declared on the leg
		// constructor (JoinUpstream/JoinView).
		seg := resolveGoSegment(e)
		ve := &viewEmbed{
			goSegment: seg,
			docField:  e.Field(),
			// A VIEW leg translates with the source view's FULL node (its children,
			// roles and own embeds) — the materialized document carries exactly what
			// a direct read of that view would, so the translator must too. This is
			// the same node a composed view's internal leg uses (ComposedLink.Node).
			// An EXTERNAL leg has only its flat mirror schema.
			node:      legViewNode(e.leg),
			isViewLeg: e.leg.view != nil,
		}
		n.embeds[seg] = ve
		n.embedsByDoc[e.Field()] = ve
	}
	// A role's shared base may own NATIVE children (base-children) that are not
	// declared as view embeds — they are derived from the base schema. Register
	// them as embeds so ToGoDoc translates the nested collection and ColumnPath
	// resolves a base-child sub-field. The doc field == the Go segment (the
	// composer's mergeSharedBaseChildren nests under the same derived name).
	if base, _, ok := schema.SharedBaseRef(); ok {
		for _, bc := range base.ChildSchemas() {
			seg := childDocSegment(bc)
			ve := &viewEmbed{goSegment: seg, docField: seg, node: newViewNode(bc, nil), isChild: true}
			n.embeds[seg] = ve
			n.embedsByDoc[seg] = ve
		}
	}
	// A schema's OWN aggregate children (root.Child(...)) are projected the same
	// way — derived from the schema, not declared as view embeds. See
	// registerOwnChildren.
	registerOwnChildren(n, schema)
	return n
}

// registerOwnChildren registers the schema's OWN aggregate children
// (root.Child(...)) on the node so ToGoDoc translates the nested collection and
// ColumnPath resolves an own-child sub-field. Doc field == Go segment (the
// composer's mergeOwnChildren nests under the same derived name). Runs at every
// schema level (root, embed sources and role nodes); children are leaves
// (depth 1, boot-enforced), and a child's own siblings resolve FLAT via the
// child node's ColumnForRead. A segment clash with an explicit embed or a
// base-child is rejected upstream by ValidateViewSchemas, so a plain overwrite
// here never fires for a valid view.
func registerOwnChildren(n *ViewNode, schema *core.TableSchema) {
	for _, child := range schema.ChildSchemas() {
		seg := childDocSegment(child)
		ve := &viewEmbed{goSegment: seg, docField: seg, node: newViewNode(child, nil), isChild: true}
		n.embeds[seg] = ve
		n.embedsByDoc[seg] = ve
	}
}

// ChildSoftDeletePaths returns, for every DERIVED lifecycle-carrying segment
// that declares a soft-delete column, the doc-field path → soft-delete column
// pair. The reader consults it to auto-include the segment's soft-delete
// column when a consumer projection narrows the subfields —
// StripArchivedChildren can only hide what the projected entries still carry.
// Covered segments: aggregate-child collections (base-children + own children)
// at this level, plus — on a SharedBaseView root — each role segment (a
// single-map path, e.g. "User") and, DOTTED, each role's own child collection
// (e.g. "User.Dependents"). Regular views never produce dotted paths.
func (n *ViewNode) ChildSoftDeletePaths() map[string]string {
	if !n.hasSchema() {
		return nil
	}
	out := map[string]string{}
	for docField, emb := range n.embedsByDoc {
		// A materialized VIEW segment contributes only what lives INSIDE it: its
		// own lifecycle segments, dotted under this one. Its ROOT soft-delete
		// column is deliberately not auto-included — an archived source document
		// stays embedded (the segment mirrors the stored state, like a mirror
		// segment does), so there is nothing at this level to hide.
		if emb.isViewLeg {
			for sub, sd := range emb.node.ChildSoftDeletePaths() {
				out[docField+"."+sub] = sd
			}
			continue
		}
		if !emb.isChild && !emb.isRole {
			continue
		}
		if sdCol, ok := emb.node.SoftDeleteColumn(); ok {
			out[docField] = sdCol
		}
		if emb.isRole {
			for sub, sd := range emb.node.ChildSoftDeletePaths() {
				out[docField+"."+sub] = sd
			}
		}
	}
	return out
}

// StripArchivedChildren removes ARCHIVED entries from the doc's nested
// aggregate-child collections — the read-time counterpart, one level down, of
// the root-level soft-delete gate. The stored document deliberately mirrors
// the relational store (archived children INCLUDED, each carrying its
// soft-delete timestamp, so an ?includeArchived read can surface them); a
// default read must hide them exactly like the write-side loader hydrates only
// active children. Operates on the PHYSICAL (column-keyed) doc, before
// ToGoDoc: each entry's soft-delete column comes from the child's own schema.
// Scope: derived child collections only (base-children + own children);
// explicitly declared embeds are untouched — a cross-service source's
// lifecycle belongs to its upstream. Mutates doc in place.
func (n *ViewNode) StripArchivedChildren(doc map[string]any) {
	if !n.hasSchema() || doc == nil {
		return
	}
	for docField, emb := range n.embedsByDoc {
		// A SharedBaseView role segment is a SINGLE optional sub-document with
		// the role's own lifecycle: an archived role (its soft-delete column
		// populated — the composer's remnant pick) is hidden on a default read
		// by nulling the whole segment; an active role recurses so the role's
		// own child collections strip like any aggregate children.
		if emb.isRole {
			m, isMap := asStringMap(doc[docField])
			if !isMap {
				continue // absent or explicit null segment
			}
			if sdCol, ok := emb.node.SoftDeleteColumn(); ok {
				if v, present := m[sdCol]; present && v != nil {
					doc[docField] = nil
					continue
				}
			}
			emb.node.StripArchivedChildren(m)
			// asStringMap may have copied (bson.M via reflection) — write back.
			doc[docField] = m
			continue
		}
		// A materialized VIEW segment carries a copy of the source view's own
		// document, so its INSIDES must read exactly as a direct read of that view
		// would: recurse so ITS child collections drop their archived entries.
		// Applies to both multiplicities — a 1:1 sub-document and every element of
		// a 1:N array. The segment itself is never dropped: an archived SOURCE
		// document stays embedded, mirroring the stored state exactly like an
		// external mirror segment does.
		if emb.isViewLeg {
			if items, ok := asAnySlice(doc[docField]); ok {
				for i, item := range items {
					if m, isMap := asStringMap(item); isMap {
						emb.node.StripArchivedChildren(m)
						items[i] = m
					}
				}
				doc[docField] = items
				continue
			}
			if m, isMap := asStringMap(doc[docField]); isMap {
				emb.node.StripArchivedChildren(m)
				doc[docField] = m
			}
			continue
		}
		if !emb.isChild {
			continue
		}
		sdCol, ok := emb.node.SoftDeleteColumn()
		if !ok {
			continue
		}
		items, ok := asAnySlice(doc[docField])
		if !ok {
			continue
		}
		kept := make([]any, 0, len(items))
		for _, item := range items {
			if m, isMap := asStringMap(item); isMap {
				if v, present := m[sdCol]; present && v != nil {
					continue
				}
			}
			kept = append(kept, item)
		}
		doc[docField] = kept
	}
}

// childDocSegment is the derived parent-side Go segment (and doc field) of a
// nested child collection — the pluralized child type name, the same derivation
// an EmbedMany uses for a one-to-many local source. Shared by a shared base's
// native children (base-children) AND a schema's own aggregate children, so both
// project under a name the ViewNode and composer agree on and the collection
// round-trips.
func childDocSegment(child *core.TableSchema) string {
	return domain.PluralizeWord(child.TypeName())
}

// hasSchema reports whether this node carries a core.TableSchema. A registered view
// ALWAYS does (schema is mandatory, enforced at boot); this stays false only for
// the defensive empty node the reader returns for a view name it has no
// definition for — there is no schema-optional mode for a real view.
func (n *ViewNode) hasSchema() bool { return n != nil && n.schema != nil }

// ColumnPath translates a Go field path (e.g. ["Addresses","ZipCode"]) into the
// physical doc/column path (e.g. ["addresses","zip"]). Returns ok=false for an
// unknown field. A node with no schema (an unregistered view name only) can't
// translate, so it passes the path through unchanged.
func (n *ViewNode) ColumnPath(goPath []string) ([]string, bool) {
	if n == nil || len(goPath) == 0 {
		return nil, false
	}
	if !n.hasSchema() {
		return goPath, true
	}
	if len(goPath) == 1 {
		col, ok := n.schema.ColumnForRead(goPath[0])
		if !ok {
			return nil, false
		}
		return []string{col}, true
	}
	emb, ok := n.embeds[goPath[0]]
	if !ok {
		return nil, false
	}
	rest, ok := emb.node.ColumnPath(goPath[1:])
	if !ok {
		return nil, false
	}
	return append([]string{emb.docField}, rest...), true
}

// SoftDeleteColumn returns the view root's soft-delete column (and whether
// enabled). Empty/false means no archived gate is applied. There is no invented
// "deleted_at" fallback — if the schema declares no soft-delete, the view has
// none; an unregistered (schema-less) node likewise yields no gate.
func (n *ViewNode) SoftDeleteColumn() (string, bool) {
	if !n.hasSchema() {
		return "", false
	}
	return n.schema.SoftDeleteColumn()
}

// ToGoDoc rewrites a physical (column-keyed) document into the Go-field
// vocabulary the application/web layers consume — recursively for embeds.
// `_id` passes through (the reader + AutoFromDoc rely on it). Columns not in
// the schema (and not a managed column) are dropped. With no schema the doc is
// returned unchanged.
func (n *ViewNode) ToGoDoc(doc map[string]any) map[string]any {
	if !n.hasSchema() || doc == nil {
		return doc
	}
	out := make(map[string]any, len(doc))
	for col, val := range doc {
		if col == "_id" {
			out["_id"] = val
			continue
		}
		if emb, ok := n.embedsByDoc[col]; ok {
			out[emb.goSegment] = emb.translateValue(val)
			continue
		}
		if goName, ok := n.schema.GoNameForRead(col); ok {
			out[goName] = val
		}
	}
	return out
}

func (e *viewEmbed) translateValue(val any) any {
	if items, ok := asAnySlice(val); ok {
		out := make([]any, len(items))
		for i, item := range items {
			if m, ok := asStringMap(item); ok {
				out[i] = e.node.ToGoDoc(m)
			} else {
				out[i] = item
			}
		}
		return out
	}
	if m, ok := asStringMap(val); ok {
		return e.node.ToGoDoc(m)
	}
	return val
}

// asStringMap normalizes any string-keyed map-like value (map[string]any,
// bson.M) into map[string]any via reflection (avoids importing bson here).
func asStringMap(v any) (map[string]any, bool) {
	if m, ok := v.(map[string]any); ok {
		return m, true
	}
	rv := reflect.ValueOf(v)
	if !rv.IsValid() || rv.Kind() != reflect.Map || rv.Type().Key().Kind() != reflect.String {
		return nil, false
	}
	out := make(map[string]any, rv.Len())
	iter := rv.MapRange()
	for iter.Next() {
		out[iter.Key().String()] = iter.Value().Interface()
	}
	return out, true
}

// asAnySlice normalizes any slice-like value ([]any, bson.A, []bson.M) into
// []any via reflection.
func asAnySlice(v any) ([]any, bool) {
	if s, ok := v.([]any); ok {
		return s, true
	}
	rv := reflect.ValueOf(v)
	if !rv.IsValid() || (rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array) {
		return nil, false
	}
	out := make([]any, rv.Len())
	for i := 0; i < rv.Len(); i++ {
		out[i] = rv.Index(i).Interface()
	}
	return out, true
}
