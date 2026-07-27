package query

import (
	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// DefaultMaxExportRows is the framework fallback ceiling on the number of rows a
// tabular export (CSV/XLSX) streams, used when neither the view
// (MaxExportRows) nor the yaml (query.maxExportRows) declares one. Bounds the
// memory + wall-clock cost of an unfiltered export of a large collection.
const DefaultMaxExportRows int64 = 10_000

// MaxExportRows overrides the per-view ceiling on the number of rows a tabular
// export of this projection streams. Resolution at request time: this value
// (when > 0) wins; otherwise the yaml default query.maxExportRows; otherwise
// DefaultMaxExportRows. Operational state — NOT part of RebuildHash /
// ArtifactHash (no Mongo rebuild, no Version bump on change), mirroring
// MaxLimit.
func (v *ViewDefinition) MaxExportRows(n int64) *ViewDefinition {
	v.maxExportRows = n
	return v
}

// MaxExportRowsValue returns the declared per-view export cap, or 0 when unset.
func (v *ViewDefinition) MaxExportRowsValue() int64 { return v.maxExportRows }

// ResolveMaxExportRows resolves the effective export-row ceiling: the per-view
// override when declared, else the supplied yaml default (query.maxExportRows)
// when positive, else DefaultMaxExportRows. The consumer passes the yaml value
// (cfg.Query.MaxExportRows) so this stays free of any bootstrap import.
func (v *ViewDefinition) ResolveMaxExportRows(yamlDefault int64) int64 {
	if v.maxExportRows > 0 {
		return v.maxExportRows
	}
	if yamlDefault > 0 {
		return yamlDefault
	}
	return DefaultMaxExportRows
}

// ExportPlan builds the format-neutral tabular-export plan for this view from
// its core.TableSchema + embed tree. Each level's columns are the schema's declared
// non-PK, non-managed fields (the business columns) in declaration order; the
// header label is resolved per field via the same precedence the audit path
// uses (external-schema inline labelKey, else the type-anchored struct tag).
// Embeds recurse to the view's full depth, carrying the parent-side Go segment
// (so the renderer descends the Go-keyed document) and the embed doc-field name
// (the `?fields=` wire segment).
//
// The PK and managed columns (created_at/updated_at/deleted_at) are
// intentionally excluded — the export carries the labeled business columns; the
// surrogate id and framework timestamps are not part of the human-facing sheet.
func (v *ViewDefinition) ExportPlan() *queries.ExportPlan {
	root := buildExportNode(v.schema, v.embeds, "", "")
	// SharedBaseView roles nest as single-object branches (the renderer's
	// child extraction already handles a single map exactly like a one-item
	// collection). Ordered as declared.
	for _, r := range v.roles {
		root.Children = append(root.Children, roleExportNode(r))
	}
	// EmbedInChild: attach the enriched sub-document as a branch INSIDE the native
	// child's export node, so the tabular export walks the enrichment columns too
	// (e.g. catalogLines[].item.label). The native child node was appended above by
	// buildExportNode, keyed by the child's derived segment.
	for _, ce := range v.childEmbeds {
		if ce.leg == nil {
			continue
		}
		seg := ce.ChildSegment()
		goSeg := ce.leg.goSegment
		for _, cn := range root.Children {
			if cn.GoSegment == seg {
				cn.Children = append(cn.Children, buildExportNode(ce.leg.schema, nil, goSeg, ce.Field()))
				break
			}
		}
	}
	return &queries.ExportPlan{Root: root}
}

// roleExportNode builds the export branch for one SharedBaseView role segment:
// the role's own business columns plus its siblings FLAT (mirroring the
// composed sub-document), its own children nested. Deliberately NOT reusing
// buildExportNode: that would fold in the base's shared columns (they live at
// the person root, exported there) and the base-children (they nest at the
// root too).
func roleExportNode(r roleDef) *queries.ExportNode {
	node := &queries.ExportNode{GoSegment: r.segment, WireSegment: domain.ToLowerCamel(r.segment)}
	appendSchemaColumns(node, r.schema)
	for _, sib := range r.schema.Siblings() {
		appendSchemaColumns(node, sib)
	}
	for _, child := range r.schema.ChildSchemas() {
		node.Children = append(node.Children, childExportNode(child))
	}
	return node
}

func buildExportNode(schema *core.TableSchema, embeds []embedDef, goSegment, wireSegment string) *queries.ExportNode {
	node := &queries.ExportNode{GoSegment: goSegment, WireSegment: wireSegment}
	if schema != nil {
		// This level's FLAT business columns — the read document merges them all at
		// the owner's level, so they are columns of THIS node: the schema's own
		// fields, then each sibling's, then the SharedBase's.
		appendSchemaColumns(node, schema)
		for _, sib := range schema.Siblings() {
			appendSchemaColumns(node, sib)
		}
		if base, _, ok := schema.SharedBaseRef(); ok {
			appendSchemaColumns(node, base)
		}
	}
	// External embeds nest as children. Embeds are single-level, so a source
	// contributes no further embeds (nil) — only its own schema-derived closure.
	for _, e := range embeds {
		if e.leg == nil {
			continue
		}
		node.Children = append(node.Children, buildExportNode(
			e.leg.schema, nil,
			resolveGoSegment(e), // parent-side Go field (renderer descends doc[goSegment])
			e.Field(),           // embed doc field = ?fields wire segment
		))
	}
	if schema != nil {
		// Nested 1:N collections project under their derived segment (matching the
		// composer + reader): a shared base's native children, then the schema's own
		// children. Each recurses, so a child's own siblings fold in FLAT too.
		if base, _, ok := schema.SharedBaseRef(); ok {
			for _, bc := range base.ChildSchemas() {
				node.Children = append(node.Children, childExportNode(bc))
			}
		}
		for _, child := range schema.ChildSchemas() {
			node.Children = append(node.Children, childExportNode(child))
		}
	}
	return node
}

// appendSchemaColumns adds one ExportColumn per declared business field of s
// (PK + managed columns excluded by GoFields), labeled via s's own label source
// (external inline labelKey, else the type-anchored struct tag).
func appendSchemaColumns(node *queries.ExportNode, s *core.TableSchema) {
	labels := s.LabelKeysByGoField()
	for _, gf := range s.GoFields() {
		node.Columns = append(node.Columns, queries.ExportColumn{
			GoField:  gf,
			WireLeaf: domain.ToLowerCamel(gf),
			LabelKey: labels[gf],
		})
	}
}

// childExportNode builds the export node for a nested 1:N child collection under
// its derived segment — the Go segment the reader nests it under (PluralizeWord of
// the child type) and the lower-camel `?fields` wire token.
func childExportNode(child *core.TableSchema) *queries.ExportNode {
	seg := childDocSegment(child)
	return buildExportNode(child, nil, seg, domain.ToLowerCamel(seg))
}
