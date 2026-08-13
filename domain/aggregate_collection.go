package domain

import (
	"fmt"
	"reflect"
	"sync"
	"unicode"
)

// CollectionNamed is the narrow contract CollectionSegmentOf resolves against:
// "this type knows the name of its collection". Every AggregateValueObject
// satisfies it (the interface declares CollectionName), and so may a type that
// is only ever a read-side schema anchor — the resolution needs the name, not
// the whole child contract, and asking for more than it needs would couple the
// two for no reason.
type CollectionNamed interface {
	CollectionName() string
}

// collectionSegmentCache memoizes the resolved (and validated) collection name
// per AVO type. CollectionName is contractually constant per type, and the
// resolution walks reflection + a zero-value method call, so it runs once per
// type per process and every later lookup is a map read on the validation path
// of every aggregate write.
var collectionSegmentCache sync.Map // reflect.Type → string

// CollectionSegmentOf resolves the collection name the AVO type t declares via
// AggregateValueObject.CollectionName. It is the SINGLE derivation point of a
// child collection's name in the framework — the notification wire path
// (childCollectionSegment) and the read side (a child's document segment and Go
// filter/sort segment) both resolve through here, so the two can never drift.
//
// t may be the AVO struct type or a pointer to it; both resolve to the same
// name. The name is read from a zero value of t, which is why CollectionName
// must not depend on receiver state.
//
// Panics — the same posture as every other identifier declaration in the
// framework (an invalid name is a programming error, never user input, and a
// silent fallback is precisely the inference this contract removes):
//
//   - t (nor *t) implements CollectionNamed;
//   - the declared name is empty or is not a valid exported Go field name
//     (first rune an ASCII uppercase letter, the rest letters or digits).
//
// For a persisted child the panic lands at boot, where core.TableSchema.Child
// resolves the segment; for a child that is only ever validated it lands on the
// first validation of that type.
func CollectionSegmentOf(t reflect.Type) string {
	if t == nil {
		panic("domain.CollectionSegmentOf: nil type — an aggregate child must be a declared AggregateValueObject")
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if cached, ok := collectionSegmentCache.Load(t); ok {
		return cached.(string)
	}
	named, ok := reflect.New(t).Interface().(CollectionNamed)
	if !ok {
		panic(fmt.Sprintf(
			"domain.CollectionSegmentOf: %s does not declare CollectionName() — an aggregate child names its "+
				"own collection (the segment it occupies inside the aggregate, e.g. \"Addresses\"); the framework "+
				"does not derive one from the type name",
			t))
	}
	name := named.CollectionName()
	if err := validateCollectionName(name); err != nil {
		panic(fmt.Sprintf("domain.CollectionSegmentOf: %s declares CollectionName() = %q — %s", t, name, err))
	}
	collectionSegmentCache.Store(t, name)
	return name
}

// validateCollectionName enforces the CollectionName contract: the declared
// name doubles as a Go segment (it must match the read DTO's field for the
// collection) and as a document key, so it has to be spellable as an exported
// Go field name. Letters beyond the first rune may be non-ASCII ("Endereços"
// is a valid Go field name and a valid collection); the FIRST rune is held to
// ASCII A-Z so the lower-camel wire rendering stays well defined.
func validateCollectionName(name string) error {
	if name == "" {
		return fmt.Errorf("the name is empty; declare the collection's name, e.g. \"Addresses\"")
	}
	for i, r := range name {
		if i == 0 {
			if r < 'A' || r > 'Z' {
				return fmt.Errorf("it must start with an ASCII uppercase letter (A-Z), as an exported Go field name does")
			}
			continue
		}
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			return fmt.Errorf("%q is not a letter or a digit; the name must be spellable as a Go field name", r)
		}
	}
	return nil
}
