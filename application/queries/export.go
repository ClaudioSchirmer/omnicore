package queries

import "strings"

// ExportPlan is the transport-agnostic, format-neutral description of how a view
// is rendered as a flat tabular export (CSV today, XLSX later). It is built by
// infra from the view's TableSchema tree (ViewDefinition.ExportPlan) and consumed
// by web's tabular-export wrapper. It carries no format knowledge — the encoder
// (web/export.Encoder) decides how columns and the per-level offset materialize.
//
// The tree mirrors the view's embed structure: the root node holds the root
// table's columns, each child node an embed (one-to-one or one-to-many), nested
// to the view's full depth. A renderer walks the tree depth-first; the node's
// depth is the column offset ("espaçar uma coluna por nível" — root at column 0,
// child at column 1, grandchild at column 2…).
type ExportPlan struct {
	Root *ExportNode
}

// ExportNode is one level of the export tree.
type ExportNode struct {
	// GoSegment is the parent-side Go field the renderer descends into on the
	// (Go-keyed) document to reach this node's items — e.g. "Addresses" so the
	// renderer reads doc["Addresses"].([]map). Empty on the root.
	GoSegment string
	// WireSegment is the parent-side wire token used to prefix this node's
	// columns in a `?fields=` path — the embed's doc field name (e.g.
	// "addresses"). Empty on the root.
	WireSegment string
	// Columns are this level's leaf columns, in schema declaration order.
	Columns []ExportColumn
	// Children are the embeds nested under this node (recursive, view depth).
	Children []*ExportNode
}

// ExportColumn is one leaf column at a node level.
type ExportColumn struct {
	// GoField is the Go field name at this level. The renderer reads
	// doc[GoField] (the view reader returns Go-keyed documents); it is also the
	// Go-path leaf used to build the read projection.
	GoField string
	// WireLeaf is the wire token for this column at its level (the acronym-aware
	// lowerCamel of GoField, e.g. "zipCode") — the `?fields=` leaf.
	WireLeaf string
	// LabelKey is the header catalog key resolved from the schema (struct tag on
	// a type-anchored schema, or the external schema's inline label). Empty when
	// the column carries no label — the renderer then falls back to GoField.
	LabelKey string
}

// WireToGoPaths returns the full wire-path → Go-field-path map for the plan:
// every leaf column (e.g. "addresses.zipCode" → "Addresses.ZipCode") AND every
// embed subtree node (e.g. "addresses" → "Addresses", which selects the whole
// subtree). The wrapper uses it to validate and translate `?fields=` / `?sort=`
// tokens; the reader maps the Go path → physical column via the view's
// TableSchema, exactly like the JSON read path.
func (p *ExportPlan) WireToGoPaths() map[string]string {
	out := map[string]string{}
	if p == nil || p.Root == nil {
		return out
	}
	walkExportPaths(p.Root, "", "", out)
	return out
}

func walkExportPaths(n *ExportNode, wirePrefix, goPrefix string, out map[string]string) {
	for _, col := range n.Columns {
		out[joinExportPath(wirePrefix, col.WireLeaf)] = joinExportPath(goPrefix, col.GoField)
	}
	for _, child := range n.Children {
		cw := joinExportPath(wirePrefix, child.WireSegment)
		cg := joinExportPath(goPrefix, child.GoSegment)
		out[cw] = cg // subtree token selects the whole embed
		walkExportPaths(child, cw, cg, out)
	}
}

// Validate reports the first `?fields=` token that names no column/subtree of
// the plan. Returns ("", true) when every token resolves.
func (p *ExportPlan) Validate(tokens []string) (bad string, ok bool) {
	m := p.WireToGoPaths()
	for _, t := range tokens {
		if _, found := m[t]; !found {
			return t, false
		}
	}
	return "", true
}

// Projection turns a set of validated `?fields=` tokens into a ReadCriteria
// projection keyed by Go field path (value 1 = include). The reader translates
// each Go path to the physical Mongo column. Unknown tokens are skipped (call
// Validate first to reject them).
func (p *ExportPlan) Projection(tokens []string) map[string]int {
	m := p.WireToGoPaths()
	out := make(map[string]int, len(tokens))
	for _, t := range tokens {
		if gp, ok := m[t]; ok {
			out[gp] = 1
		}
	}
	return out
}

// Prune returns a copy of the plan restricted to the requested `?fields=`
// tokens — a column survives when its own wire path is requested or an ancestor
// subtree token covers it; a node survives when it retains a column or a
// surviving descendant. An empty token list returns the plan unchanged (no
// narrowing → every column exports). Used by the renderer so the emitted columns
// match what the consumer asked for.
func (p *ExportPlan) Prune(tokens []string) *ExportPlan {
	if p == nil || p.Root == nil || len(tokens) == 0 {
		return p
	}
	set := make(map[string]bool, len(tokens))
	for _, t := range tokens {
		set[t] = true
	}
	root, _ := pruneExportNode(p.Root, "", set, false)
	if root == nil {
		root = &ExportNode{}
	}
	return &ExportPlan{Root: root}
}

func pruneExportNode(n *ExportNode, wirePrefix string, set map[string]bool, keepAll bool) (*ExportNode, bool) {
	var cols []ExportColumn
	for _, col := range n.Columns {
		if keepAll || set[joinExportPath(wirePrefix, col.WireLeaf)] {
			cols = append(cols, col)
		}
	}
	var children []*ExportNode
	for _, child := range n.Children {
		cw := joinExportPath(wirePrefix, child.WireSegment)
		pc, kept := pruneExportNode(child, cw, set, keepAll || set[cw])
		if kept {
			children = append(children, pc)
		}
	}
	out := &ExportNode{
		GoSegment:   n.GoSegment,
		WireSegment: n.WireSegment,
		Columns:     cols,
		Children:    children,
	}
	return out, len(cols) > 0 || len(children) > 0
}

func joinExportPath(prefix, segment string) string {
	if prefix == "" {
		return segment
	}
	if segment == "" {
		return prefix
	}
	return prefix + "." + segment
}

// SplitFields splits a comma-separated `?fields=` value into trimmed,
// non-empty tokens. The tabular-export wrapper consumes it to drive
// Validate/Projection/Prune.
func SplitFields(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
