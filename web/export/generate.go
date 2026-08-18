package export

import "reflect"

// Generate walks the (already pruned) plan over the projected wire items and
// emits the Row stream into sink. It is pure — no HTTP, no format knowledge,
// no per-request state beyond the label closure.
//
// label resolves a column header: it receives the column's catalog key
// (`exportLabelKey` tag) and json wire name and returns the header text (the
// wrapper injects the Translator + request language and the
// fallback-to-wire-name rule). items is the []TResp slice of projected
// Response values — the SAME typed items the JSON surface serializes — passed
// as `any`; child segments are descended via each node's field, so the
// column set is the Response's by construction.
//
// Layout — depth-first, header once per node group (each group is one
// invocation under a parent item), data row per item, then each child group at
// depth+1, and a BLANK separator line after each item's cascade concludes so
// every aggregate (and sub-aggregate) is visually delimited:
//
//	[root header]            depth 0
//	[root item 1 data]
//	  [addresses header]     depth 1
//	  [addr 1 data]
//	  [addr 2 data]
//	<blank>                  ← root item 1's cascade concluded
//	[root item 2 data]
//	  [addresses header]
//	  [addr 1 data]
//	<blank>
//
// With deeper nesting the separator lands at each conclusion (after a
// grandchild group, then the child group) and consecutive blanks from several
// levels popping at once collapse into one — so a leaf cascade gets one blank,
// not a stack. A blank line is a zero-cell Row{}; the encoder realizes it (CSV:
// an empty record; XLSX: an empty worksheet row).
func Generate(plan *Plan, items any, label func(labelKey, wireLeaf string) string, sink Sink) error {
	if plan == nil || plan.Root == nil {
		return nil
	}
	rv := reflect.ValueOf(items)
	if !rv.IsValid() || rv.Kind() != reflect.Slice {
		return nil
	}
	rows := make([]reflect.Value, 0, rv.Len())
	for i := 0; i < rv.Len(); i++ {
		rows = append(rows, rv.Index(i))
	}
	e := &emitter{sink: sink, label: label}
	e.renderNode(plan.Root, rows, 0)
	return e.err
}

// emitter threads the sink + a "last row was a blank separator" flag through the
// recursive walk, so blanks land after each cascade and consecutive blanks (when
// multiple nesting levels conclude at the same boundary) collapse into one.
type emitter struct {
	sink  Sink
	label func(string, string) string
	err   error
	blank bool // the last row written was a blank separator
}

func (e *emitter) write(r Row) {
	if e.err != nil {
		return
	}
	e.err = e.sink.Write(r)
	e.blank = false
}

// separator emits one blank line, collapsing it when the previous row was
// already blank (so popping several levels at once yields a single blank).
func (e *emitter) separator() {
	if e.err != nil || e.blank {
		return
	}
	e.err = e.sink.Write(Row{}) // zero-cell row = blank separator
	e.blank = true
}

func (e *emitter) renderNode(node *Node, items []reflect.Value, depth int) {
	hasCols := len(node.Columns) > 0
	if hasCols {
		e.write(headerRow(node, depth, e.label))
	}
	for _, item := range items {
		if hasCols {
			e.write(dataRow(node, depth, item))
		}
		cascaded := false
		for _, child := range node.Children {
			ci := childItems(item, child)
			if len(ci) == 0 {
				continue
			}
			e.renderNode(child, ci, depth+1)
			cascaded = true
		}
		// Blank line once this item's full cascade concludes, before the walk
		// returns to the parent level — one separator per aggregate conclusion.
		if cascaded {
			e.separator()
		}
	}
}

func headerRow(node *Node, depth int, label func(string, string) string) Row {
	cells := make([]Cell, len(node.Columns))
	for i, col := range node.Columns {
		cells[i] = Cell{Value: label(col.LabelKey, col.WireLeaf)}
	}
	return Row{Depth: depth, Header: true, Cells: cells}
}

func dataRow(node *Node, depth int, item reflect.Value) Row {
	cells := make([]Cell, len(node.Columns))
	for i, col := range node.Columns {
		cells[i] = Cell{Value: cellValue(item, col)}
	}
	return Row{Depth: depth, Cells: cells}
}
