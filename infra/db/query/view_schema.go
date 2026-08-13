package query

import (
	"reflect"

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
	// fieldsGo / fieldsCol restrict a JoinView-leg node whose leg declares
	// Fields(...): fieldsGo holds the declared Go entries (plus the always-
	// materialized "ID"), fieldsCol the physical allowset the trim applies. Nil
	// on every unrestricted node (the default). A restricted node translates
	// ONLY allowlisted leaf fields (a capped field is unknown → the wire's 400
	// SchemaViolation, honest instead of a query matching nothing), reports no
	// DeletedAt gate when the column is capped (the segment then has no archived
	// rule, BY DECLARATION), and registers only the admitted nested segments.
	fieldsGo  map[string]struct{}
	fieldsCol map[string]struct{}
}

type viewEmbed struct {
	goSegment string
	docField  string
	node      *ViewNode
	// isChild marks a derived aggregate-child collection (a shared base's native
	// child or a schema's own child) as opposed to an explicitly declared embed.
	// It no longer gates the archived-entry strip — every segment follows one
	// rule now (hide when the SOURCE SCHEMA declares a DeletedAt column) — but
	// it still distinguishes the two shapes for the rest of the read path.
	isChild bool
	// isRole marks a SharedBaseView role segment: a SINGLE optional
	// sub-document (not a collection) carrying the role's own lifecycle — an
	// archived role's segment is hidden on default reads, and the role's own
	// child collections strip recursively.
	isRole bool
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
	// doc segment. Registered as a plain embed (NOT isChild): it is a SEGMENT, not
	// an aggregate child collection. Like every other segment it is hidden on a
	// default read when its own source schema declares a DeletedAt column.
	for _, ce := range v.childEmbeds {
		childVE, ok := n.embedsByDoc[ce.ChildSegment()]
		if !ok || childVE.node == nil {
			continue // boot-validated to be a native child; defensive
		}
		seg := ce.leg.goSegment
		ive := &viewEmbed{goSegment: seg, docField: ce.Field(), node: legViewNode(ce.leg)}
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
		node := leg.view.BuildViewNode()
		if len(leg.fields) > 0 {
			return restrictViewNode(node, leg)
		}
		return node
	}
	return newViewNode(leg.schema, nil)
}

// restrictViewNode narrows a JoinView-leg node to the leg's declared Fields:
// leaf translation is gated on the Go entries, the DeletedAt gate on the
// physical allowset, and only admitted top-level segments stay registered
// (a segment entry admits that segment WHOLE — its own node, rules included,
// recursively; an unlisted segment is cut with the data). "ID" is always
// admitted: identity is always materialized.
func restrictViewNode(node *ViewNode, leg *Leg) *ViewNode {
	goSet := make(map[string]struct{}, len(leg.fields)+1)
	for _, f := range leg.fields {
		goSet[f] = struct{}{}
	}
	goSet[idGoFieldName] = struct{}{}
	out := &ViewNode{
		schema:      node.schema,
		embeds:      map[string]*viewEmbed{},
		embedsByDoc: map[string]*viewEmbed{},
		fieldsGo:    goSet,
		fieldsCol:   legFieldsColumnSet(leg),
	}
	for seg, emb := range node.embeds {
		if _, ok := goSet[seg]; ok {
			out.embeds[seg] = emb
			out.embedsByDoc[emb.docField] = emb
		}
	}
	return out
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
			node: legViewNode(e.leg),
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

// ChildDeletedAtPaths returns, for EVERY segment whose source schema declares a
// DeletedAt column, the doc-field path → DeletedAt column pair. The reader
// consults it to auto-include that column when a consumer projection narrows the
// subfields — StripArchivedChildren can only hide what the projected entries
// still carry — and removes it again before responding, so the wire shape matches
// the request.
//
// Covered: every shape the strip covers, because the two must agree — aggregate
// child collections (base-children + own children), SharedBaseView roles,
// materialized embed segments (1:1 and 1:N, over a local view or an upstream
// mirror) and EmbedInChild enrichments. Nested content contributes DOTTED paths
// (a role's own children, "User.Dependents"; a materialized view's children,
// "product.ProductLines"; an enrichment inside a child element,
// "items.product"). A segment whose schema declares NO DeletedAt contributes
// nothing: it has no archived state to hide.
func (n *ViewNode) ChildDeletedAtPaths() map[string]string {
	if !n.hasSchema() {
		return nil
	}
	out := map[string]string{}
	for docField, emb := range n.embedsByDoc {
		// ONE rule for every segment kind: a segment whose source schema declares
		// a DeletedAt column carries a lifecycle the default read hides, so the
		// column must survive a narrowed projection for the strip to see it.
		if sdCol, ok := emb.node.DeletedAtColumn(); ok {
			out[docField] = sdCol
		}
		// …and whatever lives INSIDE it (a role's children, a materialized view's
		// children, an EmbedInChild enrichment inside a child element) contributes
		// its own dotted path.
		for sub, sd := range emb.node.ChildDeletedAtPaths() {
			out[docField+"."+sub] = sd
		}
	}
	return out
}

// StripArchivedChildren hides ARCHIVED content in EVERY segment of the document
// — the read-time counterpart of the root-level DeletedAt gate, applied one or
// more levels down. The stored document deliberately mirrors the relational
// store (archived entries INCLUDED, each carrying its DeletedAt timestamp, so
// an ?includeArchived read can surface them); a default read hides them exactly
// like the write-side loader hydrates only active children.
//
// ONE rule, every shape — a native child collection, a SharedBaseView role, a
// materialized embed (1:1 or 1:N, over a local view or an upstream mirror) and
// an EmbedInChild enrichment: a segment is filtered if, and ONLY IF, the schema
// behind it declares a DeletedAt column. That declaration is what says the
// source has an archived state and names the column carrying it; a source that
// declares none has no archived concept and is never touched. The behavior is a
// property of the DECLARATION, not of the verb, the leg kind, or whether the
// data was materialized.
//
// Hiding is content-level, never row-level (a segment is a LEFT join, so the
// document always survives): a 1:1 segment becomes the explicit null — the same
// value an unresolved reference carries — and a 1:N segment drops the archived
// elements, keeping the rest in their stored order. Operates on the PHYSICAL
// (column-keyed) doc, before ToGoDoc, and recurses so content nested inside a
// segment follows the same rule. Mutates doc in place.
//
// The caller skips this pass entirely under ?includeArchived, which is what
// makes that one flag reveal every level at once.
func (n *ViewNode) StripArchivedChildren(doc map[string]any) {
	if !n.hasSchema() || doc == nil {
		return
	}
	for docField, emb := range n.embedsByDoc {
		sdCol, hasSD := emb.node.DeletedAtColumn()

		// A SharedBaseView role segment is a SINGLE optional sub-document with
		// the role's own lifecycle: an archived role is hidden by nulling the
		// whole segment; an active one recurses.
		if emb.isRole {
			m, isMap := asStringMap(doc[docField])
			if !isMap {
				continue // absent or explicit null segment
			}
			if hasSD {
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

		// EVERY OTHER SEGMENT — a native child collection, a materialized embed
		// (1:1 or 1:N, over a local view or an upstream mirror), an EmbedInChild
		// enrichment — follows ONE rule: when its source schema declares a
		// DeletedAt column, an archived entry is hidden on a default read, and
		// `?includeArchived=true` (which skips this whole pass) brings it back.
		// A source that declares NO DeletedAt has no archived concept and is
		// never touched. Hiding is content-level, never row-level: a 1:1 segment
		// becomes the explicit null and a 1:N element leaves the array, but the
		// document itself always survives (the LEFT semantics of a segment).
		if items, ok := asAnySlice(doc[docField]); ok {
			kept := make([]any, 0, len(items))
			for _, item := range items {
				m, isMap := asStringMap(item)
				if !isMap {
					kept = append(kept, item)
					continue
				}
				if hasSD {
					if v, present := m[sdCol]; present && v != nil {
						continue // archived entry: hidden on a default read
					}
				}
				// Recurse so whatever lives inside the entry follows the same rule
				// (a materialized view's own children, an EmbedInChild enrichment).
				emb.node.StripArchivedChildren(m)
				kept = append(kept, m)
			}
			doc[docField] = kept
			continue
		}
		if m, isMap := asStringMap(doc[docField]); isMap {
			if hasSD {
				if v, present := m[sdCol]; present && v != nil {
					doc[docField] = nil
					continue
				}
			}
			emb.node.StripArchivedChildren(m)
			doc[docField] = m
		}
	}
}

// childDocSegment is the parent-side Go segment (and doc field) of a nested
// child collection — the name the child's domain type DECLARES via
// CollectionName, resolved once at boot when its owner registered it. Shared by
// a shared base's native children (base-children) AND a schema's own aggregate
// children, so both project under a name the ViewNode and composer agree on and
// the collection round-trips. The framework derives no name here: a collection
// is called what the domain calls it.
func childDocSegment(child *core.TableSchema) string {
	return child.CollectionSegment()
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
		// A Fields-restricted leg node translates only allowlisted leaves — a
		// capped field is UNKNOWN here (the wire answers 400), never a silent
		// query against a column the trim removed. "ID" is always admitted.
		if n.fieldsGo != nil && goPath[0] != idGoFieldName {
			if _, ok := n.fieldsGo[goPath[0]]; !ok {
				return nil, false
			}
		}
		col, ok := n.schema.ColumnForRead(goPath[0])
		if !ok {
			return nil, false
		}
		// A mirror (external) schema keeps its id in `_id` (no physical id column),
		// so a filter / sort / ?fields= on the id resolves there. Regular schemas
		// keep it in the physical PK column.
		if n.schema.IsExternal() {
			if pk := n.schema.IDColumn(); pk != "" && col == pk {
				return []string{"_id"}, true
			}
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

// DeletedAtColumn returns the view root's DeletedAt column (and whether
// enabled). Empty/false means no archived gate is applied. There is no invented
// "deleted_at" fallback — if the schema declares no DeletedAt, the view has
// none; an unregistered (schema-less) node likewise yields no gate.
func (n *ViewNode) DeletedAtColumn() (string, bool) {
	if !n.hasSchema() {
		return "", false
	}
	col, ok := n.schema.DeletedAtColumn()
	if !ok {
		return "", false
	}
	// A Fields-restricted leg node whose allowlist CAPS the DeletedAt column has
	// no archived rule, BY DECLARATION: the column is never materialized, so the
	// strip has nothing to gate on and reports none — the per-consumer archive
	// switch (see Leg.Fields).
	if n.fieldsCol != nil {
		if _, kept := n.fieldsCol[col]; !kept {
			return "", false
		}
	}
	return col, true
}

// StripJoinKeyID removes the `_id` a composed read kept on a leg segment ONLY to
// attach it (the consumer did not request the id). For an external MIRROR schema
// — whose id has no physical column and was therefore PROMOTED onto the ID Go
// field by ToGoDoc — the promoted field is dropped too, so the id obeys the
// projection exactly like any other field: kept only when requested. A regular
// schema exposes its id through the physical column (which the projection already
// excludes when unrequested), so it never carries a promoted field to drop here.
func (n *ViewNode) StripJoinKeyID(doc map[string]any) {
	delete(doc, "_id")
	if n.hasSchema() && n.schema.IsExternal() {
		if pk := n.schema.IDColumn(); pk != "" {
			if idGo, ok := n.schema.GoOf(pk); ok {
				delete(doc, idGo)
			}
		}
	}
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
			// An upstream MIRROR schema (external) carries its identity ONLY in `_id`
			// — the outbox payload has no physical id column — so promote it onto the
			// schema's ID Go field. Regular schemas expose the id through their
			// physical PK column (which is subject to the projection like any field),
			// so they are NOT promoted from the incidental `_id` here: doing so would
			// leak the id past a `?fields=` that excluded it. When a mirror segment's
			// id is kept only as a composition join key, the composed reader strips
			// both `_id` and this promoted field together.
			if n.schema.IsExternal() {
				if pk := n.schema.IDColumn(); pk != "" {
					if idGo, ok := n.schema.GoOf(pk); ok {
						if _, present := out[idGo]; !present {
							out[idGo] = val
						}
					}
				}
			}
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
