package mongo

import (
	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db"
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
// its db.TableSchema + embed tree. Each level's columns are the schema's declared
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
	return &queries.ExportPlan{Root: buildExportNode(v.schema, v.embeds, "", "")}
}

func buildExportNode(schema *db.TableSchema, embeds []embedDef, goSegment, wireSegment string) *queries.ExportNode {
	node := &queries.ExportNode{GoSegment: goSegment, WireSegment: wireSegment}
	if schema != nil {
		labels := schema.LabelKeysByGoField()
		for _, gf := range schema.GoFields() {
			node.Columns = append(node.Columns, queries.ExportColumn{
				GoField:  gf,
				WireLeaf: domain.ToLowerCamel(gf),
				LabelKey: labels[gf],
			})
		}
	}
	for _, e := range embeds {
		if e.source == nil {
			continue
		}
		node.Children = append(node.Children, buildExportNode(
			e.source.schema, e.source.embeds,
			resolveGoSegment(e), // parent-side Go field (renderer descends doc[goSegment])
			e.field,             // embed doc field = ?fields wire segment
		))
	}
	return node
}
