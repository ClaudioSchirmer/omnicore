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
// managed-column carrier. Until domain.Managed exists (introduced when the
// aggregate loader surfaces managed columns into the entity), no field
// qualifies and IsSameByBusinessFields compares every exported field.
func isManagedCarrier(reflect.StructField) bool {
	return false // superseded when domain.Managed lands: f.Anonymous && f.Type == reflect.TypeOf(Managed{})
}
