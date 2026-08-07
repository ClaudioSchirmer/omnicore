package query

import (
	"reflect"

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
// non-ID, non-managed fields (the business columns) in declaration order; the
// header label is resolved per field via the same precedence the audit path
// uses (external-schema inline labelKey, else the type-anchored struct tag).
// Embeds recurse to the view's full depth, carrying the parent-side Go segment
// (so the renderer descends the Go-keyed document) and the embed doc-field name
// (the `?fields=` wire segment).
//
// The ID and managed columns (created_at/updated_at/deleted_at) are
// intentionally excluded — the export carries the labeled business columns; the
// surrogate id and framework timestamps are not part of the human-facing sheet.
func (v *ViewDefinition) ExportPlan() *queries.ExportPlan {
	// A SharedBaseView is rooted at the TYPE-LESS base, whose columns carry their
	// `labelKey` tags on the ROLE structs that hold them flat — hand every role as
	// a label anchor (empty for a plain view, whose root schema is type-anchored).
	root := buildExportNode(v.schema, v.embeds, "", "", v.roleLabelAnchors()...)
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
		var branch *queries.ExportNode
		if ce.leg.view != nil {
			branch = ce.leg.view.ExportPlan().Root
			branch.GoSegment = goSeg
			branch.WireSegment = ce.Field()
			branch = restrictExportBranch(branch, ce.leg)
		} else {
			branch = buildExportNode(ce.leg.schema, nil, goSeg, ce.Field())
		}
		for _, cn := range root.Children {
			if cn.GoSegment == seg {
				cn.Children = append(cn.Children, branch)
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

// roleLabelAnchors returns the Go types of the declared roles, in declaration
// order — the label anchors for the type-less shared base a SharedBaseView is
// rooted at. Empty for a plain view (no roles), which labels its root columns
// from its own type-anchored schema.
func (v *ViewDefinition) roleLabelAnchors() []reflect.Type {
	if len(v.roles) == 0 {
		return nil
	}
	out := make([]reflect.Type, 0, len(v.roles))
	for _, r := range v.roles {
		if t := r.schema.GoType(); t != nil {
			out = append(out, t)
		}
	}
	return out
}

// buildExportNode assembles one level of the plan. labelAnchors are the Go types
// whose `labelKey` struct tags label a TYPE-LESS root schema's columns (a
// SharedBaseView's roles); a type-anchored root passes none and labels itself.
func buildExportNode(schema *core.TableSchema, embeds []embedDef, goSegment, wireSegment string, labelAnchors ...reflect.Type) *queries.ExportNode {
	node := &queries.ExportNode{GoSegment: goSegment, WireSegment: wireSegment}
	if schema != nil {
		// This level's FLAT business columns — the read document merges them all at
		// the owner's level, so they are columns of THIS node: the schema's own
		// fields, then each sibling's, then the SharedBase's.
		appendSchemaColumns(node, schema, labelAnchors...)
		for _, sib := range schema.Siblings() {
			appendSchemaColumns(node, sib)
		}
		if base, _, ok := schema.SharedBaseRef(); ok {
			// The base is type-less: its flattened columns are labeled by the role
			// struct that carries them (mirroring the audit timeline's composition).
			appendSchemaColumns(node, base, schema.GoType())
		}
	}
	// Embeds nest as children. An EXTERNAL source contributes only its own
	// schema-derived closure (a mirror declares no embeds of its own). A VIEW
	// source (JoinView) contributes its FULL export tree — its children, roles
	// and its own embeds — re-rooted under this embed's segments, exactly as a
	// composed view's internal leg does (ComposedViewDefinition.ExportPlan).
	for _, e := range embeds {
		if e.leg == nil {
			continue
		}
		var branch *queries.ExportNode
		if e.leg.view != nil {
			branch = e.leg.view.ExportPlan().Root
			branch.GoSegment = resolveGoSegment(e)
			branch.WireSegment = e.Field()
			// A Fields allowlist narrows the STORED segment, so the export tree
			// must not advertise columns/segments that are never materialized.
			branch = restrictExportBranch(branch, e.leg)
		} else {
			branch = buildExportNode(
				e.leg.schema, nil,
				resolveGoSegment(e), // parent-side Go field (renderer descends doc[goSegment])
				e.Field(),           // embed doc field = ?fields wire segment
			)
		}
		node.Children = append(node.Children, branch)
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

// restrictExportBranch narrows a JoinView-leg export branch to the leg's
// declared Fields (a no-op without one): only allowlisted leaf columns stay,
// and only admitted top-level segments keep their child branches — mirroring
// what the trimmed document actually stores.
func restrictExportBranch(branch *queries.ExportNode, leg *Leg) *queries.ExportNode {
	if len(leg.fields) == 0 {
		return branch
	}
	allow := make(map[string]struct{}, len(leg.fields))
	for _, f := range leg.fields {
		allow[f] = struct{}{}
	}
	cols := make([]queries.ExportColumn, 0, len(branch.Columns))
	for _, c := range branch.Columns {
		if _, ok := allow[c.GoField]; ok {
			cols = append(cols, c)
		}
	}
	children := make([]*queries.ExportNode, 0, len(branch.Children))
	for _, ch := range branch.Children {
		if _, ok := allow[ch.GoSegment]; ok {
			children = append(children, ch)
		}
	}
	branch.Columns = cols
	branch.Children = children
	return branch
}

// appendSchemaColumns adds one ExportColumn per declared business field of s
// (ID + managed columns excluded by GoFields), labeled via s's own label source
// (external inline labelKey, else the type-anchored struct tag) — or, for a
// type-less schema, via the labelKey tags of the anchor types that carry its
// fields flat.
func appendSchemaColumns(node *queries.ExportNode, s *core.TableSchema, labelAnchors ...reflect.Type) {
	labels := s.LabelKeysByGoFieldAnchoredOn(labelAnchors...)
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
