package export

import "github.com/ClaudioSchirmer/omnicore/application/queries"

// Generate walks the (already pruned) plan over the read's documents and emits
// the Row stream into sink. It is pure — no HTTP, no format knowledge, no
// per-request state beyond the label closure.
//
// label resolves a column header: it receives the column's catalog key and Go
// field name and returns the header text (the wrapper injects the Translator +
// request language and the fallback-to-field-name rule). items are the Go-keyed
// documents the view reader returned; child collections are descended via each
// node's GoSegment.
//
// Layout — depth-first, header once per node group (each group is one
// invocation under a parent item), data row per item, then each child group at
// depth+1:
//
//	[root header]            depth 0
//	[root item 1 data]
//	  [addresses header]     depth 1
//	  [addr 1 data]
//	  [addr 2 data]
//	[root item 2 data]
//	  [addresses header]
//	  [addr 1 data]
func Generate(plan *queries.ExportPlan, items []map[string]any, label func(labelKey, goField string) string, sink Sink) error {
	if plan == nil || plan.Root == nil {
		return nil
	}
	return renderNode(plan.Root, items, 0, label, sink)
}

func renderNode(node *queries.ExportNode, items []map[string]any, depth int, label func(string, string) string, sink Sink) error {
	hasCols := len(node.Columns) > 0
	if hasCols {
		if err := sink.Write(headerRow(node, depth, label)); err != nil {
			return err
		}
	}
	for _, item := range items {
		if hasCols {
			if err := sink.Write(dataRow(node, depth, item)); err != nil {
				return err
			}
		}
		for _, child := range node.Children {
			childItems := extractChildItems(item[child.GoSegment])
			if len(childItems) == 0 {
				continue
			}
			if err := renderNode(child, childItems, depth+1, label, sink); err != nil {
				return err
			}
		}
	}
	return nil
}

func headerRow(node *queries.ExportNode, depth int, label func(string, string) string) Row {
	cells := make([]Cell, len(node.Columns))
	for i, col := range node.Columns {
		cells[i] = Cell{Value: label(col.LabelKey, col.GoField)}
	}
	return Row{Depth: depth, Header: true, Cells: cells}
}

func dataRow(node *queries.ExportNode, depth int, item map[string]any) Row {
	cells := make([]Cell, len(node.Columns))
	for i, col := range node.Columns {
		cells[i] = Cell{Value: item[col.GoField]}
	}
	return Row{Depth: depth, Cells: cells}
}

// extractChildItems normalizes the value found under a node's GoSegment into a
// slice of documents. EmbedMany lands a slice; a one-to-one Embed lands a single
// map; anything else (absent / wrong shape) yields no rows.
func extractChildItems(v any) []map[string]any {
	switch t := v.(type) {
	case []map[string]any:
		return t
	case map[string]any:
		return []map[string]any{t}
	case []any:
		out := make([]map[string]any, 0, len(t))
		for _, e := range t {
			if m, ok := e.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	default:
		return nil
	}
}
