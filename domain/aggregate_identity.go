package domain

import "reflect"

// IsSameByBusinessFields is the opt-in structural implementation of
// IsSameBusinessIdentity: two items are the same when every EXPORTED field is
// deeply equal, EXCEPT the framework-managed carrier (domain.Managed), which
// never participates in business identity. Use it inside IsSameBusinessIdentity
// when the aggregate's notion of identity genuinely is "every business field":
//
//	func (l OrderLine) IsSameBusinessIdentity(o domain.AggregateValueObject) bool {
//	    return domain.IsSameByBusinessFields(l, o)
//	}
//
// It is the explicit, opt-in counterpart of the whole-struct reflect.DeepEqual
// the change tracker used to apply implicitly — same result MINUS the managed
// carrier, and only where the domain asks for it. A type mismatch is never the
// same identity. Prefer a hand-written identity (a natural key) when the
// aggregate has one; IsSameByBusinessFields is the escape hatch for children with no
// canonical business key beyond their full value.
func IsSameByBusinessFields(a, b AggregateValueObject) bool {
	va, vb := reflect.ValueOf(a), reflect.ValueOf(b)
	if !va.IsValid() || !vb.IsValid() || va.Type() != vb.Type() {
		return false
	}
	if va.Kind() != reflect.Struct {
		return reflect.DeepEqual(a, b)
	}
	t := va.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" { // unexported — invisible to business identity
			continue
		}
		if isManagedCarrier(f) { // the embedded framework-managed carrier is never identity
			continue
		}
		if !reflect.DeepEqual(va.Field(i).Interface(), vb.Field(i).Interface()) {
			return false
		}
	}
	return true
}

// isManagedCarrier reports whether a struct field is the embedded framework
// managed-column carrier (domain.Managed) — embedded either by value (the
// canonical form) or as a pointer. Either way the carrier is framework-owned
// and never business identity, so IsSameByBusinessFields skips it.
func isManagedCarrier(f reflect.StructField) bool {
	if !f.Anonymous {
		return false
	}
	t := f.Type
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t == reflect.TypeOf(Managed{})
}
