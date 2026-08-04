package domain

import (
	"fmt"
	"reflect"
)

// ValueObject is a self-validating domain primitive that owns exactly one rule
// and the notification that rule emits (Email, Phone, ZipCode, Document, …).
// It is the "raw" kind: the type declares its OWN IsValid because the rule is
// bespoke — a regex, a length cap, a format check. See the Value Objects manual
// section.
type ValueObject[T any] interface {
	Value() T
	IsValid(fieldName string, ctx *NotificationContext) bool
}

// EnumValueObject is a closed-set domain primitive backed by an int or a string.
// E is the enum type; T is its underlying scalar. Like ValueObject it exposes
// Value() T (the underlying, declared per enum); unlike a raw ValueObject it
// does NOT write IsValid — it declares its members (Values) and the notification
// for a value outside the set (UnknownNotification), and the framework validates
// membership itself (ValidateEnum, or the ValidateValueObject seam where the
// type is erased).
//
// The zero value of E is the canonical "Unknown" sentinel and is never a
// member; parsing an unknown wire value converges to it (EnumByValue), so the
// sentinel is the single "invalid" state the guard rejects. Description keys
// (EnumDescriptionKey) derive from the underlying value — "Ethnicity.1" for an
// int enum, "Relationship.spouse" for a string enum — resolved through the
// Translator at the boundary; the domain never translates.
type EnumValueObject[E comparable, T comparable] interface {
	comparable
	Value() T
	Values() []E
	UnknownNotification() Notification
}

// enumSet is the membership-only view of an EnumValueObject (Value() omitted) the
// generic helpers constrain on, so E is inferred from the argument WITHOUT the
// caller spelling out the underlying T — which Go cannot infer from a method
// return. A concrete enum satisfies this because it has Values + UnknownNotification.
type enumSet[E comparable] interface {
	comparable
	Values() []E
	UnknownNotification() Notification
}

// ValueObjectValidator is the interface the entity's validation pass consumes: a
// raw ValueObject satisfies it directly (its own IsValid), and an
// EnumValueObject is adapted to it by the framework (enumMembershipValidator).
type ValueObjectValidator interface {
	IsValid(fieldName string, ctx *NotificationContext) bool
}

// EnumDescriptionKey is the translation key of an enum value object's value:
// "<TypeName>.<value>" (e.g. "Ethnicity.1", "EventType.LOG"). Resolve it
// against the Translator with the request language at the boundary — the domain
// exposes the key, never the translated string.
func EnumDescriptionKey(v any) string {
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return ""
		}
		rv = rv.Elem()
	}
	return fmt.Sprintf("%s.%v", rv.Type().Name(), rv.Interface())
}

// ValidateEnum reports whether e is a declared member of its own Values() set.
// If it is not — the zero/Unknown sentinel, or an out-of-range cast — it emits
// the enum's UnknownNotification on ctx and returns false. This is the typed
// entry used where the concrete enum type is known (an AVO's BuildRules); the
// ValidateValueObject seam validates the same way by reflection where the type is
// erased.
func ValidateEnum[E enumSet[E]](e E, fieldName string, ctx *NotificationContext) bool {
	for _, member := range e.Values() {
		if e == member {
			return true
		}
	}
	if ctx != nil {
		ctx.AddNotification(fieldName, e.UnknownNotification())
	}
	return false
}

// EnumByValue returns the member of E whose underlying value equals raw — an int
// or a string, typically straight off the wire — or the zero value of E (the
// Unknown sentinel) when nothing matches. It is the framework's getByValue: the
// closed-set gate at the boundary, converging any unknown input to Unknown so a
// later ValidateEnum rejects it.
func EnumByValue[E enumSet[E]](raw any) E {
	var zero E
	for _, member := range zero.Values() {
		if sameUnderlying(reflect.ValueOf(member), raw) {
			return member
		}
	}
	return zero
}

// sameUnderlying compares an enum member's underlying scalar (int or string) to
// a raw wire value, tolerating the several int widths a decoder may hand back.
func sameUnderlying(member reflect.Value, raw any) bool {
	switch member.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, ok := asInt64(raw)
		return ok && member.Int() == n
	case reflect.String:
		s, ok := raw.(string)
		return ok && member.String() == s
	}
	return false
}

func asInt64(raw any) (int64, bool) {
	switch n := raw.(type) {
	case int:
		return int64(n), true
	case int8:
		return int64(n), true
	case int16:
		return int64(n), true
	case int32:
		return int64(n), true
	case int64:
		return n, true
	case float64: // JSON numbers decode to float64
		if n == float64(int64(n)) {
			return int64(n), true
		}
	}
	return 0, false
}

// enumUnknownNotifier is the non-generic view the validation seam uses to detect
// an enum value object (its generic Values() cannot sit in a plain interface)
// and reach its UnknownNotification. Membership is then checked by reflection.
type enumUnknownNotifier interface {
	UnknownNotification() Notification
}

// validatorFor returns the ValueObjectValidator for vo — the vo itself when it
// is a raw ValueObject (writes IsValid), a membership validator when it is an
// EnumValueObject (Values + UnknownNotification), or nil when it is neither.
// Shared by ValidateValueObject (the explicit path) and the entity's automatic
// field discovery.
func validatorFor(vo any) ValueObjectValidator {
	switch v := vo.(type) {
	case ValueObjectValidator:
		return v
	case enumUnknownNotifier:
		return enumMembershipValidator{e: v}
	default:
		return nil
	}
}

// enumMembershipValidator adapts an EnumValueObject — which does not write
// IsValid — to the ValueObjectValidator the entity validation pass expects. At
// the ValidateValueObject seam the concrete enum type is erased, so membership is
// checked against Values() reflectively; the behavior matches ValidateEnum.
type enumMembershipValidator struct {
	e enumUnknownNotifier
}

func (v enumMembershipValidator) IsValid(fieldName string, ctx *NotificationContext) bool {
	rv := reflect.ValueOf(v.e)
	values := rv.MethodByName("Values")
	if !values.IsValid() {
		panic(fmt.Sprintf("enum value object %T must declare Values() []T", v.e))
	}
	members := values.Call(nil)[0]
	self := rv.Interface()
	for i := 0; i < members.Len(); i++ {
		if self == members.Index(i).Interface() {
			return true
		}
	}
	if ctx != nil {
		ctx.AddNotification(fieldName, v.e.UnknownNotification())
	}
	return false
}

// --- persistence seam -------------------------------------------------------
//
// The infrastructure layer persists a VO field as its underlying scalar and
// reconstructs it on read. These non-generic helpers are the seam it consumes:
// detection (IsValueObject/IsEnumValueObject), extraction (ValueObjectValue) and
// reconstruction (NewValueObjectValue). They mirror the discrimination
// validatorFor already does over the SAME two interfaces, so a VO never has to
// declare anything new — Value() plus IsValid (raw) or UnknownNotification
// (enum) are the signals. Reflection is used because Value()/Values() are
// generic in the underlying T, so a caller without the static type cannot invoke
// them directly.

// IsValueObject reports whether x is a raw ValueObject (writes its own IsValid).
func IsValueObject(x any) bool {
	_, ok := x.(ValueObjectValidator)
	return ok
}

// IsEnumValueObject reports whether x is an EnumValueObject. Detected by its
// EXCLUSIVE signal — UnknownNotification() — never by "has no IsValid": an enum's
// IsValid lives on the enumMembershipValidator adapter, not on the concrete type,
// so the two interfaces are disjoint and one direct assertion each is enough.
func IsEnumValueObject(x any) bool {
	_, ok := x.(enumUnknownNotifier)
	return ok
}

// ValueObjectValue returns the underlying persistable scalar of a value object of
// either kind — what Value() yields (the string behind an Email, the int behind a
// UserProfile) — and true; (nil,false) when x is not a value object or is a nil
// nullable VO (the caller maps that to SQL NULL). Reflection-based because
// Value() is generic in the underlying T.
func ValueObjectValue(x any) (any, bool) {
	if !IsValueObject(x) && !IsEnumValueObject(x) {
		return nil, false
	}
	rv := reflect.ValueOf(x)
	for rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return nil, false
		}
		rv = rv.Elem()
	}
	m := rv.MethodByName("Value")
	if !m.IsValid() {
		return nil, false
	}
	return m.Call(nil)[0].Interface(), true
}

// NewValueObjectValue builds a value object of type voType from a raw underlying
// scalar — the read-side inverse of ValueObjectValue. A raw VO is a plain type
// conversion (Email("x")). An EnumVO CONVERGES through membership: a member maps
// to itself, an out-of-set value to the zero/Unknown sentinel (so a tampered or
// legacy row reconstructs as Unknown, never a phantom member). Returns the built
// value as any.
func NewValueObjectValue(voType reflect.Type, raw any) (any, error) {
	if _, isEnum := reflect.Zero(voType).Interface().(enumUnknownNotifier); isEnum {
		return enumMemberOrUnknown(voType, raw).Interface(), nil
	}
	rv := reflect.ValueOf(raw)
	if !rv.IsValid() {
		return reflect.Zero(voType).Interface(), nil
	}
	if !rv.Type().ConvertibleTo(voType) {
		return nil, fmt.Errorf("cannot build value object %s from %s", voType, rv.Type())
	}
	return rv.Convert(voType).Interface(), nil
}

// enumMemberOrUnknown returns the member of enumType whose underlying scalar
// equals raw, or the zero value (the Unknown sentinel) when none matches — the
// reflective twin of EnumByValue, for the persistence layer that holds the
// enum's reflect.Type rather than its static type.
func enumMemberOrUnknown(enumType reflect.Type, raw any) reflect.Value {
	zero := reflect.Zero(enumType)
	values := zero.MethodByName("Values")
	if !values.IsValid() {
		return zero
	}
	members := values.Call(nil)[0]
	for i := 0; i < members.Len(); i++ {
		if member := members.Index(i); sameUnderlying(member, raw) {
			return member
		}
	}
	return zero
}
