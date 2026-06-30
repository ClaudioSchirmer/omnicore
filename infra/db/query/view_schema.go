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
}

// BuildViewNode assembles the translator tree for a view.
func (v *ViewDefinition) BuildViewNode() *ViewNode {
	return newViewNode(v.schema, v.embeds)
}

func newViewNode(schema *core.TableSchema, embeds []embedDef) *ViewNode {
	n := &ViewNode{
		schema:      schema,
		embeds:      map[string]*viewEmbed{},
		embedsByDoc: map[string]*viewEmbed{},
	}
	for _, e := range embeds {
		if e.source == nil {
			continue
		}
		// Parent-side Go segment: explicit .As, else derived from the source's
		// Go type (local), else the doc field as a last-resort (the boot guard
		// ValidateViewSchemas rejects an external embed that reaches this with
		// no resolvable segment, so this fallback is defensive only).
		seg := resolveGoSegment(e)
		if seg == "" {
			seg = e.field
		}
		ve := &viewEmbed{
			goSegment: seg,
			docField:  e.field,
			node:      newViewNode(e.source.schema, e.source.embeds),
		}
		n.embeds[seg] = ve
		n.embedsByDoc[e.field] = ve
	}
	// A role's shared base may own NATIVE children (base-children) that are not
	// declared as view embeds — they are derived from the base schema. Register
	// them as embeds so ToGoDoc translates the nested collection and ColumnPath
	// resolves a base-child sub-field. The doc field == the Go segment (the
	// composer's mergeSharedBaseChildren nests under the same derived name).
	if base, _, ok := schema.SharedBaseRef(); ok {
		for _, bc := range base.ChildSchemas() {
			seg := sharedBaseChildSegment(bc)
			ve := &viewEmbed{goSegment: seg, docField: seg, node: newViewNode(bc, nil)}
			n.embeds[seg] = ve
			n.embedsByDoc[seg] = ve
		}
	}
	return n
}

// sharedBaseChildSegment is the derived parent-side Go segment (and doc field) of
// a shared base's native child collection — the pluralized child type name, the
// same derivation an EmbedMany uses for a one-to-many local source. Composer and
// ViewNode both key on it, so the nested collection round-trips.
func sharedBaseChildSegment(bc *core.TableSchema) string {
	return domain.PluralizeWord(bc.TypeName())
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
