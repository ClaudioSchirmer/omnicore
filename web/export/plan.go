package export

import (
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// The tabular-export column plan, derived from the wire Response DTO — the
// same single wire authority every other read surface consumes. A field
// absent from the Response exports nowhere; `?fields=` speaks the same json
// wire tokens as the JSON listing; headers come from the Response's
// `exportLabelKey` tag (translated) falling back to the json wire name.
//
// The tree mirrors the Response's nesting: the root node holds the root
// scalar columns, each struct / slice-of-struct field a child node, nested
// to the DTO's full depth. A renderer walks the tree depth-first; the
// node's depth is the column offset (root at column 0, child at column 1,
// grandchild at column 2…).

// Plan is the format-neutral description of how a Response DTO renders as a
// flat tabular export (CSV/XLSX). Built once per type via PlanFor.
type Plan struct {
	Root *Node
}

// Node is one level of the export tree.
type Node struct {
	// GoSegment is the parent-side Go field name the renderer descends into —
	// identical to the Result field and the canonical document key. Empty on
	// the root.
	GoSegment string
	// WireSegment is the parent-side json wire token used to prefix this
	// node's columns in a `?fields=` path. Empty on the root.
	WireSegment string
	// Columns are this level's leaf columns, in DTO declaration order.
	Columns []Column
	// Children are the nested segments under this node (recursive).
	Children []*Node

	// fieldIndex locates the segment field on the PARENT struct.
	fieldIndex []int
}

// Column is one leaf column at a node level.
type Column struct {
	// GoField is the Go field name at this level — the Go-path leaf used to
	// prune against the read projection echo.
	GoField string
	// WireLeaf is the json wire token for this column at its level — the
	// `?fields=` leaf, shared verbatim with the JSON listing.
	WireLeaf string
	// LabelKey is the header catalog key from the field's `exportLabelKey`
	// tag. Empty when absent — the renderer falls back to WireLeaf. Reusing
	// the entity's labelKey value converges the header on the same
	// translation the write side uses.
	LabelKey string

	// fieldIndex locates the column field on its node's struct.
	fieldIndex []int
}

var planCache sync.Map // map[reflect.Type]*Plan

// PlanFor builds (and memoizes) the export plan for the Response type t.
// Pointer types are dereferenced; a non-struct type yields an empty plan.
func PlanFor(t reflect.Type) *Plan {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil || t.Kind() != reflect.Struct {
		return &Plan{Root: &Node{}}
	}
	if v, ok := planCache.Load(t); ok {
		return v.(*Plan)
	}
	plan := &Plan{Root: buildNode(t, "", "", nil)}
	planCache.Store(t, plan)
	return plan
}

var (
	timeType          = reflect.TypeOf(time.Time{})
	jsonMarshalerType = reflect.TypeOf((*json.Marshaler)(nil)).Elem()
	textMarshalerType = reflect.TypeOf((*interface{ MarshalText() ([]byte, error) })(nil)).Elem()
)

// scalarStruct reports whether a struct type renders as a single cell
// rather than a nested segment: time.Time, self-marshaling types
// (json.Marshaler / encoding.TextMarshaler — e.g. domain.ID, value
// objects) and structs with no exported fields.
func scalarStruct(t reflect.Type) bool {
	if t == timeType {
		return true
	}
	if t.Implements(jsonMarshalerType) || reflect.PointerTo(t).Implements(jsonMarshalerType) {
		return true
	}
	if t.Implements(textMarshalerType) || reflect.PointerTo(t).Implements(textMarshalerType) {
		return true
	}
	for i := 0; i < t.NumField(); i++ {
		if t.Field(i).IsExported() {
			return false
		}
	}
	return true
}

func buildNode(t reflect.Type, goSegment, wireSegment string, fieldIndex []int) *Node {
	node := &Node{GoSegment: goSegment, WireSegment: wireSegment, fieldIndex: fieldIndex}
	appendFields(node, t, nil)
	return node
}

func appendFields(node *Node, t reflect.Type, basePath []int) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		path := append(append([]int{}, basePath...), i)

		// Anonymous struct promotion — fields surface at this level,
		// matching encoding/json and the projection schema.
		if f.Anonymous {
			ft := f.Type
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				appendFields(node, ft, path)
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		wire, _, _ := strings.Cut(tag, ",")
		if wire == "" {
			wire = f.Name
		}
		labelKey := f.Tag.Get("exportLabelKey")

		ft := f.Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		switch {
		case ft.Kind() == reflect.Slice:
			elem := ft.Elem()
			for elem.Kind() == reflect.Pointer {
				elem = elem.Elem()
			}
			if elem.Kind() == reflect.Struct && !scalarStruct(elem) {
				node.Children = append(node.Children, buildNode(elem, f.Name, wire, path))
				continue
			}
			node.Columns = append(node.Columns, Column{GoField: f.Name, WireLeaf: wire, LabelKey: labelKey, fieldIndex: path})
		case ft.Kind() == reflect.Struct && !scalarStruct(ft):
			node.Children = append(node.Children, buildNode(ft, f.Name, wire, path))
		default:
			node.Columns = append(node.Columns, Column{GoField: f.Name, WireLeaf: wire, LabelKey: labelKey, fieldIndex: path})
		}
	}
}

// PruneToProjection returns a copy of the plan restricted to the effective
// read projection (Go-field-path → 1 include / 0 exclude — the
// PageOf.Projection echo, post-ToCriteria), so a field a Query removed from
// the criteria (e.g. via ReadCriteria.Restrict) disappears from the tabular
// columns — header included — not just from the JSON. This keeps ToCriteria
// the single source of truth for which fields surface in every format.
//
//   - empty projection        → whole doc → every column survives
//   - inclusion mode (any 1)  → a column survives iff its Go-path, or an
//     ancestor segment path, is flagged 1
//   - exclusion mode (only 0) → every column survives except those flagged
//     0, or under an ancestor flagged 0
//
// alsoKeep names Go field paths that must survive the prune even though they
// are absent from proj — the COMPUTED fields the consumer selected. A computed
// field has no column, so `?fields=display` pushes its SOURCES to the store and
// the projection echo never mentions `Display`; without this the pruner would
// drop the very column the consumer asked for and keep its sources instead.
func (p *Plan) PruneToProjection(proj map[string]int, alsoKeep ...string) *Plan {
	if p == nil || p.Root == nil || !projectionNarrows(proj) {
		return p
	}
	include := projectionIncludes(proj)
	keep := make(map[string]bool, len(alsoKeep))
	for _, k := range alsoKeep {
		keep[k] = true
	}
	root, _ := pruneNodeByProjection(p.Root, "", proj, keep, include, !include)
	if root == nil {
		root = &Node{}
	}
	return &Plan{Root: root}
}

// projectionNarrows reports whether proj restricts anything. A nil/empty map — or
// one carrying only the `_id:0` auto-exclusion (no real column) — is whole-doc.
func projectionNarrows(proj map[string]int) bool {
	for k := range proj {
		if k != "_id" {
			return true
		}
	}
	return false
}

// projectionIncludes reports inclusion mode: any real column flagged 1.
func projectionIncludes(proj map[string]int) bool {
	for k, v := range proj {
		if v == 1 && k != "_id" {
			return true
		}
	}
	return false
}

func pruneNodeByProjection(n *Node, goPrefix string, proj map[string]int, alsoKeep map[string]bool, include, keepAll bool) (*Node, bool) {
	var cols []Column
	for _, col := range n.Columns {
		gp := joinPath(goPrefix, col.GoField)
		if alsoKeep[gp] {
			// A selected computed column: no projection entry backs it, but the
			// consumer asked for it explicitly.
			cols = append(cols, col)
			continue
		}
		var keep bool
		if include {
			keep = keepAll || proj[gp] == 1
		} else {
			// Exclusion mode: keep unless this path is EXPLICITLY flagged 0. An
			// absent key is a zero value, not an exclusion — check presence.
			v, present := proj[gp]
			keep = keepAll && (!present || v != 0)
		}
		if keep {
			cols = append(cols, col)
		}
	}
	var children []*Node
	for _, child := range n.Children {
		cg := joinPath(goPrefix, child.GoSegment)
		childKeepAll := keepAll
		if include {
			if proj[cg] == 1 {
				childKeepAll = true // an included segment path keeps the whole segment
			}
		} else if v, present := proj[cg]; present && v == 0 {
			childKeepAll = false // an explicitly-excluded segment path drops the whole segment
		}
		pc, kept := pruneNodeByProjection(child, cg, proj, alsoKeep, include, childKeepAll)
		if kept {
			children = append(children, pc)
		}
	}
	out := &Node{
		GoSegment:   n.GoSegment,
		WireSegment: n.WireSegment,
		Columns:     cols,
		Children:    children,
		fieldIndex:  n.fieldIndex,
	}
	return out, len(cols) > 0 || len(children) > 0
}

func joinPath(prefix, segment string) string {
	if prefix == "" {
		return segment
	}
	if segment == "" {
		return prefix
	}
	return prefix + "." + segment
}

// fieldValue walks idx from v, dereferencing pointers; a nil hop yields an
// invalid Value.
func fieldValue(v reflect.Value, idx []int) reflect.Value {
	for _, i := range idx {
		for v.Kind() == reflect.Pointer {
			if v.IsNil() {
				return reflect.Value{}
			}
			v = v.Elem()
		}
		if v.Kind() != reflect.Struct {
			return reflect.Value{}
		}
		v = v.Field(i)
	}
	return v
}

// cellValue extracts a column's cell value from one item: the field value
// with value objects unwrapped to their underlying scalar (parity with what
// the JSON surface renders and with the persisted form). Pointers stay —
// the encoders dereference them (nil → empty cell).
func cellValue(item reflect.Value, col Column) any {
	fv := fieldValue(item, col.fieldIndex)
	if !fv.IsValid() {
		return nil
	}
	v := fv
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return nil
		}
		v = v.Elem()
	}
	if !v.CanInterface() {
		return nil
	}
	if raw, ok := domain.ValueObjectValue(v.Interface()); ok {
		return raw
	}
	return v.Interface()
}

// childItems extracts a segment's items from one parent item: a slice field
// yields its elements, a 1:1 struct field yields a single element, a nil
// pointer yields none.
func childItems(item reflect.Value, node *Node) []reflect.Value {
	fv := fieldValue(item, node.fieldIndex)
	if !fv.IsValid() {
		return nil
	}
	for fv.Kind() == reflect.Pointer {
		if fv.IsNil() {
			return nil
		}
		fv = fv.Elem()
	}
	switch fv.Kind() {
	case reflect.Slice:
		out := make([]reflect.Value, 0, fv.Len())
		for i := 0; i < fv.Len(); i++ {
			out = append(out, fv.Index(i))
		}
		return out
	case reflect.Struct:
		return []reflect.Value{fv}
	default:
		return nil
	}
}
