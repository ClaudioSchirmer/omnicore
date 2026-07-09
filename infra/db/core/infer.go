package core

import "reflect"

// TypeName returns the Go type name of T without pointer or package qualifier.
// Used by BaseRepository and AggregateLoader to derive the default ContextName
// when the service has not set one explicitly — convention: ContextName = name
// of the entity type ("User" for *appdomain.User).
//
//	TypeName[*User]()  → "User"
//	TypeName[User]()   → "User"
//
// Returns "" if T is an anonymous type or unnamed interface — the caller
// decides the fallback (BaseRepository/AggregateLoader also return "",
// preserving the pre-existing semantics of "no configured context name").
func TypeName[T any]() string {
	t := reflect.TypeOf((*T)(nil)).Elem()
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t.Name()
}
