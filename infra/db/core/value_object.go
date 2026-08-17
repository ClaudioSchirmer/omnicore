package core

import (
	"reflect"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// Value-object persistence seam (infra side). A domain field may be a value
// object — a named type over a supported scalar whose value lives in Value()
// (Email over string, UserProfile over int). The framework persists the
// UNDERLYING scalar and reconstructs the VO on read, so the driver never sees the
// named type (which the closed set otherwise rejects). This file holds the two
// primitives the write path uses; the read path (scanTargetFor) reconstructs
// through domain.NewValueObjectValue.

// unwrapVO returns the underlying persistable scalar of a value-object value, or
// the value unchanged when it is not a VO. A nil nullable VO becomes untyped nil
// (SQL NULL). It is the single write-side seam applied wherever a persisted field
// value is read off the entity (writeFields, GoFieldValues, sharedBaseValues).
func unwrapVO(v any) any {
	if !domain.IsValueObject(v) && !domain.IsEnumValueObject(v) {
		return v
	}
	// domain.ID satisfies the value-object contract (it has Value()+IsValid) but
	// is NOT a persisted VO: it has dedicated, dialect-native id encoding
	// (BINARY(16)/UUID) that must not be short-circuited to a plain string.
	switch v.(type) {
	case domain.ID, *domain.ID:
		return v
	}
	if u, ok := domain.ValueObjectValue(v); ok {
		return u
	}
	// No underlying scalar came back. Two very different situations reach here,
	// and they must not share an answer: a NIL nullable value object (absent —
	// SQL NULL), or a COMPOSITE value object, which has no single scalar form at
	// all because its value spans several columns. Tell them apart by the value,
	// never by the failure — returning NULL for a composite would bind a silent
	// nil for a value that is actually there.
	rv := reflect.ValueOf(v)
	for rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return nil // nil nullable VO → SQL NULL
		}
		rv = rv.Elem()
	}
	if rv.IsValid() && rv.Kind() == reflect.Struct {
		// A composite never reaches a bind site through the framework's own paths
		// (writeFields walks its PARTS, and a criteria on the value object as a
		// whole cannot resolve — only its parts are declared names). Pass it
		// through untouched so an out-of-band caller gets the driver's loud
		// "unsupported type" instead of a silently NULL column.
		return v
	}
	return nil
}

// UnwrapVO is the exported form — an out-of-package engine's write path reads
// field values through it so a VO binds as its underlying scalar.
func UnwrapVO(v any) any { return unwrapVO(v) }

// valueObjectField probes a declared struct-field type: whether it is a value
// object, whether it is the enum kind, and the underlying scalar type the
// framework persists (from Value()). A pointer field (nullable VO) is
// dereferenced first. ok=false for a plain field.
func valueObjectField(ft reflect.Type) (isEnum bool, underlying reflect.Type, ok bool) {
	et := ft
	for et.Kind() == reflect.Pointer {
		et = et.Elem()
	}
	// domain.ID satisfies the VO contract but has its own dedicated id handling
	// (idScanTarget / the dialect id codecs) — never the generic VO path.
	if et == idType {
		return false, nil, false
	}
	zero := reflect.Zero(et).Interface()
	if !domain.IsValueObject(zero) && !domain.IsEnumValueObject(zero) {
		return false, nil, false
	}
	u, uok := domain.ValueObjectValue(zero)
	if !uok || u == nil {
		return false, nil, false
	}
	return domain.IsEnumValueObject(zero), reflect.TypeOf(u), true
}
