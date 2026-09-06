package domain

import (
	"fmt"
	"reflect"
	"sync"
)

// This file is the deterministic field-reference resolver behind
// Rules.AddNotification(&e.Field, ...): it maps a pointer INTO an entity
// struct back to the exported field it addresses — by offset plus pointer
// type, never by string — so the emitted message carries the field's Go name
// (rendered lowerCamel for the wire), its `notifyAs:"..."` wire override and its
// `labelKey:"..."` catalog key, all read from the StructField itself.
//
// Misuse cannot be caught at compile time (methods cannot be generic, so the
// parameter is `any`), therefore every wrong input panics IMMEDIATELY with a
// message that names the likely fix. A silent wrong field name is the failure
// mode this design exists to kill; a loud first-execution panic is the
// contract.

// fieldNode is one addressable field of an entity type: the Path segments from
// the root to it (anonymous embeds contribute no segment — promoted fields
// keep their promoted name) and the StructField whose tags apply.
type fieldNode struct {
	segs []PathSegment
	leaf reflect.StructField
}

type atlasKey struct {
	offset uintptr
	typ    reflect.Type
}

// fieldAtlas indexes every exported field reachable from a struct type without
// crossing a pointer, keyed by (offset, field type). Crossing a pointer is
// impossible by construction: what a pointer field points at lives outside the
// struct's memory block, so a reference through it fails the range check
// before ever consulting the atlas.
type fieldAtlas struct {
	size      uintptr
	nodes     map[atlasKey]fieldNode
	ambiguous map[atlasKey]bool
}

var fieldAtlasCache sync.Map // reflect.Type → *fieldAtlas

func atlasFor(t reflect.Type) *fieldAtlas {
	if cached, ok := fieldAtlasCache.Load(t); ok {
		return cached.(*fieldAtlas)
	}
	a := &fieldAtlas{
		size:      t.Size(),
		nodes:     map[atlasKey]fieldNode{},
		ambiguous: map[atlasKey]bool{},
	}
	a.walk(t, 0, nil)
	fieldAtlasCache.Store(t, a)
	return a
}

func (a *fieldAtlas) walk(t reflect.Type, baseOff uintptr, prefix []PathSegment) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() && !f.Anonymous {
			continue
		}
		off := baseOff + f.Offset
		segs := prefix
		if !f.Anonymous {
			seg := PathSegment{Name: f.Name, Wire: notifyAsTagOf(f)}
			segs = append(append([]PathSegment{}, prefix...), seg)
			a.register(off, f.Type, fieldNode{segs: segs, leaf: f})
		}
		// Recurse into struct kinds so nested and embedded fields resolve
		// (&e.Composite.Part, promoted Managed slots). Pointer fields are
		// leaves: their target lives outside this memory block.
		if f.Type.Kind() == reflect.Struct {
			a.walk(f.Type, off, segs)
		}
	}
}

func (a *fieldAtlas) register(off uintptr, t reflect.Type, node fieldNode) {
	key := atlasKey{offset: off, typ: t}
	if _, exists := a.nodes[key]; exists {
		// Two distinct fields sharing offset AND type (zero-size neighbors) —
		// resolution would be a guess, so the pair is marked and a lookup on
		// it panics instead of picking one.
		a.ambiguous[key] = true
		return
	}
	a.nodes[key] = node
}

func (a *fieldAtlas) locate(off uintptr, t reflect.Type) (fieldNode, bool) {
	key := atlasKey{offset: off, typ: t}
	if a.ambiguous[key] {
		panic(fmt.Sprintf(
			"domain: field reference at offset %d is ambiguous — two zero-size fields of type %s share it; "+
				"emit via AddNotificationNamed for these fields", off, t))
	}
	node, ok := a.nodes[key]
	return node, ok
}

// notifyAsTagOf reads the `notifyAs:"..."` wire-name override off a field. The
// mirror of the labelKey rules: "-" and "" are both "no declaration".
func notifyAsTagOf(f reflect.StructField) string {
	tag := f.Tag.Get("notifyAs")
	if tag == "-" {
		return ""
	}
	return tag
}

// labelKeyOf reads the `labelKey:"..."` catalog key off a field, honoring the
// same skip rules resolveLabelKey applies ("-" and "" are no declaration).
func labelKeyOf(f reflect.StructField) string {
	tag := f.Tag.Get("labelKey")
	if tag == "-" {
		return ""
	}
	return tag
}

// bindFieldBase records the entity instance whose fields this Rules resolves
// references against. base must be a non-nil pointer to a struct — the SAME
// allocation whose BuildRules is being invoked, which is how &e.Field lands
// inside the range the resolver checks.
func (r *Rules) bindFieldBase(base any) {
	if base == nil {
		return
	}
	v := reflect.ValueOf(base)
	if v.Kind() != reflect.Pointer || v.IsNil() {
		return
	}
	elem := v.Elem()
	if elem.Kind() != reflect.Struct {
		return
	}
	r.base = v
	r.basePtr = v.Pointer()
	r.atlas = atlasFor(elem.Type())
}

// clonePath copies a Path slice before it leaves the atlas cache: alias and
// override passes mutate segments in place, and a shared backing array would
// let one message's rewrite corrupt every later emission of the same field.
func clonePath(segs []PathSegment) []PathSegment {
	out := make([]PathSegment, len(segs))
	copy(out, segs)
	return out
}

// resolveFieldRef maps a &e.Field reference to its fieldNode, panicking with a
// pedagogic message on every misuse (see the file comment for why panics, not
// errors). Returns the node and the typed pointer for value extraction.
func (r *Rules) resolveFieldRef(field any) (fieldNode, reflect.Value) {
	if !r.base.IsValid() {
		panic("domain: this Rules is not bound to an entity instance — field-reference emissions " +
			"(r.AddNotification(&e.Field, ...)) work inside BuildRules, where the framework binds the receiver " +
			"(implement BuildRules on the POINTER receiver — a value receiver leaves the framework nothing to bind); " +
			"outside BuildRules construct with domain.NewRulesFor(mode, ctx, &e), and for synthetic or " +
			"cross-field names use r.AddNotificationNamed(\"name\", ...)")
	}
	rv := reflect.ValueOf(field)
	if !rv.IsValid() || rv.Kind() != reflect.Pointer || rv.IsNil() {
		panic(fmt.Sprintf(
			"domain: AddNotification takes a pointer to the entity's own field — r.AddNotification(&e.Field, ...); got %T", field))
	}
	p := rv.Pointer()
	if p < r.basePtr || p >= r.basePtr+r.atlas.size {
		panic(fmt.Sprintf(
			"domain: the reference (%s) points outside the %s instance this Rules validates. "+
				"Take it from the SAME receiver BuildRules received — not from a copy made by a helper — and "+
				"if the field is itself a pointer (an optional field), pass &e.Field, not e.Field",
			rv.Type(), r.base.Type().Elem()))
	}
	node, ok := r.atlas.locate(p-r.basePtr, rv.Type().Elem())
	if !ok {
		panic(fmt.Sprintf(
			"domain: offset %d in %s matches no exported field of type %s — pass a reference to the field itself "+
				"(&e.Field); references through a pointer field (&e.Ptr.Sub) cannot be resolved",
			p-r.basePtr, r.base.Type().Elem(), rv.Type().Elem()))
	}
	return node, rv
}
