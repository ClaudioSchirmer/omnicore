package core

import (
	"fmt"
	"reflect"
)

// Scanning an OPTIONAL composite value object (*Address) is the one place where
// a column cannot be decided on its own. "Every part column NULL ⇒ the value
// object is absent" is a verdict over the WHOLE group, and it has to be reached
// after the row lands — so the parts of an optional composite scan into typed
// holders that tolerate NULL, and a finalizer then either drops the value object
// or fills it in.
//
// A MANDATORY composite (declared by value) has no group and no finalizer: each
// part follows its own Go type, exactly as a scalar value-object field does —
// a non-pointer part scanning NULL is the driver's loud error, a pointer part
// scanning NULL is nil.

// compositeScanState holds every optional-composite group of one row scan.
type compositeScanState struct {
	groups []*compositeScanGroup
}

// compositeScanGroup is one optional composite value object being scanned: the
// pointer field that holds it, and one holder per declared part.
type compositeScanGroup struct {
	owner reflect.Value // the *VO field on the entity (settable)
	voTyp reflect.Type  // the value object's own type
	parts []compositePart
}

type compositePart struct {
	name   string        // the part's Go field name, for the error message
	field  reflect.Value // the part's field inside the materialized value object
	holder reflect.Value // *(*T) — nil after the scan means the column was NULL
	isVO   bool
}

// group returns the group owning prefix, creating it on first use. owner is the
// pointer field the composite lives in.
func (st *compositeScanState) group(owner reflect.Value, voTyp reflect.Type) *compositeScanGroup {
	for _, g := range st.groups {
		// Same addressable field ⇒ same group. Comparing the addresses keeps this
		// correct when one entity carries several composites.
		if g.owner.CanAddr() && owner.CanAddr() && g.owner.Addr().Pointer() == owner.Addr().Pointer() {
			return g
		}
	}
	g := &compositeScanGroup{owner: owner, voTyp: voTyp}
	st.groups = append(st.groups, g)
	return g
}

// optionalCompositeTarget builds the scan target for one part of an OPTIONAL
// composite. The part scans into a **T holder — the nullable idiom both drivers
// resolve natively (database/sql's convertAssign walks pointer depth, pgx has a
// pointer-to-pointer scan plan), so nothing here re-implements driver
// conversion. A value-object part goes through the existing nullable VO proxy,
// which already yields nil on NULL.
func (g *compositeScanGroup) target(name string, field reflect.Value) any {
	ft := field.Type()
	base := ft
	if base.Kind() == reflect.Pointer {
		base = base.Elem()
	}
	if _, underlying, isVO := valueObjectField(base); isVO {
		holder := reflect.New(reflect.PointerTo(base)) // **VO
		g.parts = append(g.parts, compositePart{name: name, field: field, holder: holder, isVO: true})
		return &nullableVOScanTarget{dst: holder.Elem(), voType: base, underlying: underlying}
	}
	holder := reflect.New(reflect.PointerTo(base)) // **T
	g.parts = append(g.parts, compositePart{name: name, field: field, holder: holder})
	return holder.Interface()
}

// finalize applies every group's verdict once the row has been scanned. A group
// whose every part came back NULL is an ABSENT value object: the pointer field
// is reset to nil, undoing the eager materialization the scan path needed to
// address the parts at all. Otherwise the value object is present, and a NULL on
// a non-pointer part is a half-written row — a loud error, the same contract a
// non-nullable scalar value object has.
func (st *compositeScanState) finalize() error {
	for _, g := range st.groups {
		present := false
		for _, p := range g.parts {
			if !p.holder.Elem().IsNil() {
				present = true
				break
			}
		}
		if !present {
			g.owner.Set(reflect.Zero(g.owner.Type()))
			continue
		}
		for _, p := range g.parts {
			val := p.holder.Elem()
			if val.IsNil() {
				if p.field.Kind() == reflect.Pointer {
					p.field.Set(reflect.Zero(p.field.Type()))
					continue
				}
				return fmt.Errorf(
					"NULL scanned into %s.%s, a non-nullable part of a PRESENT composite value object "+
						"(the other parts carry values, so the row is half-written) — declare the part *%s "+
						"(and the column NULL-able), or fix the row",
					g.voTyp, p.name, p.field.Type())
			}
			if p.field.Kind() == reflect.Pointer {
				p.field.Set(val)
				continue
			}
			p.field.Set(val.Elem())
		}
	}
	return nil
}

// buildScanTargets resolves one scan plan into driver targets against dst.
// A column whose path reaches through a POINTER — a part of an optional
// composite value object — is routed through a group so the row's verdict on
// absence can be taken as a whole; every other column scans straight into its
// field, which is the pre-composite behavior unchanged.
func buildScanTargets(v reflect.Value, columns []string, byCol map[string]FieldPath, caller string) ([]any, *compositeScanState, error) {
	targets := make([]any, 0, len(columns))
	state := &compositeScanState{}
	for _, col := range columns {
		path, ok := byCol[col]
		if !ok {
			return nil, nil, fmt.Errorf("%s: column %q has no corresponding field in %s", caller, col, v.Type().Name())
		}
		if owner, voTyp, part, isOptional := optionalCompositePart(v, path); isOptional {
			g := state.group(owner, voTyp)
			name := voTyp.Field(path[len(path)-1]).Name
			targets = append(targets, g.target(name, part))
			continue
		}
		field := path.TargetIn(v)
		if !field.IsValid() {
			return nil, nil, fmt.Errorf("%s: column %q does not resolve to a field of %s", caller, col, v.Type().Name())
		}
		targets = append(targets, scanTargetFor(field))
	}
	return targets, state, nil
}

// optionalCompositePart reports whether path addresses a part of an OPTIONAL
// composite (the field holding the value object is a pointer), returning the
// pointer field, the value object's type and the part's field inside the
// materialized value object.
func optionalCompositePart(root reflect.Value, path FieldPath) (owner reflect.Value, voTyp reflect.Type, part reflect.Value, ok bool) {
	prefix := path.prefix()
	if !prefix.resolved() {
		return reflect.Value{}, nil, reflect.Value{}, false
	}
	owner = prefix.TargetIn(root)
	if !owner.IsValid() || owner.Kind() != reflect.Pointer {
		return reflect.Value{}, nil, reflect.Value{}, false
	}
	voTyp = owner.Type().Elem()
	// TargetIn materializes the value object so its parts are addressable; the
	// finalizer resets it to nil when the row proves it absent.
	part = path.TargetIn(root)
	if !part.IsValid() {
		return reflect.Value{}, nil, reflect.Value{}, false
	}
	return owner, voTyp, part, true
}
