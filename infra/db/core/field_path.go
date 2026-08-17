package core

import "reflect"

// FieldPath addresses one persisted Go field by its chain of struct field
// indices: {3} for a field declared at the entity's root, {3,0} for the first
// part of a composite value object whose own field sits at index 3. It replaces
// the single int the schema carried before decomposition existed — a root field
// is a one-element path, so depth 1 is the general form's simplest case rather
// than a branch every consumer has to spell out.
//
// A nil/empty path means "not a struct field at all": the type-less schemas
// (NewExternalSchema, and a shared base before a role resolves it) declare
// columns with no Go struct behind them.
type FieldPath []int

// resolved reports whether the path addresses a real struct field.
func (p FieldPath) resolved() bool { return len(p) > 0 }

// prefix is the path of the COMPOSITE that owns this part — the whole path
// minus its last hop. Empty for a root field. It is the grouping key: two parts
// of one value object share a prefix, which is what lets the once-rule guard
// detect a composite declared twice and an audit-driven undo recover which
// entries belong to the same value object.
func (p FieldPath) prefix() FieldPath {
	if len(p) < 2 {
		return nil
	}
	return p[:len(p)-1]
}

// equal reports whether two paths address the same field.
func (p FieldPath) equal(other FieldPath) bool {
	if len(p) != len(other) {
		return false
	}
	for i := range p {
		if p[i] != other[i] {
			return false
		}
	}
	return true
}

// ValueIn resolves p against root for READING, without allocating: ok=false
// when a nil pointer is met along the way. That is an ABSENT optional composite
// (a nil *Address), and the write and audit paths want exactly that answer —
// the parts contribute SQL NULL, not the zero value of a struct nobody created.
// root may be a pointer; it is dereferenced first.
func (p FieldPath) ValueIn(root reflect.Value) (reflect.Value, bool) {
	v := root
	for _, idx := range p {
		for v.Kind() == reflect.Pointer {
			if v.IsNil() {
				return reflect.Value{}, false
			}
			v = v.Elem()
		}
		if v.Kind() != reflect.Struct || idx >= v.NumField() {
			return reflect.Value{}, false
		}
		v = v.Field(idx)
	}
	return v, p.resolved()
}

// TargetIn resolves p against root for WRITING and returns the addressable
// field, ALLOCATING every nil pointer it meets on the way down. This is what
// the scan path needs: an optional composite has to be materialized before its
// parts can be addressed at all. reflect's own walkers cannot do it —
// Value.FieldByIndex panics on a nil pointer and FieldByIndexErr only reports
// it, neither allocates. An invalid Value comes back when the walk hits a nil
// pointer it cannot set.
//
// Allocating eagerly is safe because the scan finalizer undoes it: an optional
// composite whose every part column scanned NULL is reset to nil (see
// compositeScanGroup), so a materialized-but-absent value never survives a row.
func (p FieldPath) TargetIn(root reflect.Value) reflect.Value {
	v := root
	for _, idx := range p {
		for v.Kind() == reflect.Pointer {
			if v.IsNil() {
				if !v.CanSet() {
					return reflect.Value{}
				}
				v.Set(reflect.New(v.Type().Elem()))
			}
			v = v.Elem()
		}
		if v.Kind() != reflect.Struct || idx >= v.NumField() {
			return reflect.Value{}
		}
		v = v.Field(idx)
	}
	if !p.resolved() {
		return reflect.Value{}
	}
	return v
}

// StructFieldIn resolves p against a TYPE, returning the declared struct field
// — the seam every tag reader uses (`labelKey`, `json:"-"`). Pointer hops are
// dereferenced, so a part of an optional composite resolves like any other.
func (p FieldPath) StructFieldIn(t reflect.Type) (reflect.StructField, bool) {
	var out reflect.StructField
	if !p.resolved() || t == nil {
		return out, false
	}
	cur := t
	for _, idx := range p {
		for cur.Kind() == reflect.Pointer {
			cur = cur.Elem()
		}
		if cur.Kind() != reflect.Struct || idx >= cur.NumField() {
			return reflect.StructField{}, false
		}
		out = cur.Field(idx)
		cur = out.Type
	}
	return out, true
}

// TypeIn is StructFieldIn reduced to the field's declared type.
func (p FieldPath) TypeIn(t reflect.Type) (reflect.Type, bool) {
	f, ok := p.StructFieldIn(t)
	if !ok {
		return nil, false
	}
	return f.Type, true
}
