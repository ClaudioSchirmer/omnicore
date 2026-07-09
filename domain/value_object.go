package domain

import (
	"fmt"
	"reflect"
)

type ValueObject[T any] interface {
	Value() T
	IsValid(fieldName string, ctx *NotificationContext) bool
}

type EnumValueObject[T comparable] interface {
	ValueObject[T]
	UnknownNotification() Notification
}

func EnumDescriptionKey(v any) string {
	t := reflect.TypeOf(v)
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return fmt.Sprintf("%s.%v", t.Name(), v)
}

// ValidateEnum is the framework's zero-value guard for enum-shaped value
// objects. It rejects only the typed zero value of T (e.g. LangUnknown,
// ModeUnknown, EventUnknown, StatusUnknown) — the canonical "unset" sentinel
// of an iota-based enum — and emits the registered UnknownNotification when
// it fires.
//
// What it does NOT do:
//
//   - It does NOT validate v against an allowlist of declared values. A cast
//     like `Language(99)` is non-zero, so it passes. Consumers that need range
//     enforcement (e.g. parsing untrusted external input directly into the
//     enum type) must check membership themselves before calling IsValid —
//     the AllLanguages() / Values() pattern already in the package is the
//     idiomatic surface for that.
//
// This is a deliberate contract, not a bug: enums in this codebase always
// flow through a closed-set parser (translator/middleware/Request DTO) that
// already rejects unknown wire values at the boundary, so a second range
// check inside the domain would duplicate the gate without adding safety.
// The function name `ValidateEnum` predates this clarification — pinned by
// language_test.go::TestLanguageIsValid/out-of-range_value_currently_passes
// so a future change in semantic surfaces as a test failure.
func ValidateEnum[T comparable](e EnumValueObject[T], fieldName string, ctx *NotificationContext) bool {
	v := e.Value()
	var zero T
	if v == zero {
		if ctx != nil {
			ctx.AddNotificationMessage(NotificationMessage{
				FieldName:    fieldName,
				Notification: e.UnknownNotification(),
			})
		}
		return false
	}
	return true
}
