package domain

import (
	"reflect"
	"strings"
	"testing"
)

// The declared name is returned verbatim — no rule is applied on top of it. The
// point of the contract is that a collection is called what the domain calls it,
// in the domain's own language, so a name no English pluralizer would produce
// must survive untouched.
type ptCollection struct{}

func (ptCollection) CollectionName() string { return "Enderecos" }

type deCollection struct{}

func (deCollection) CollectionName() string { return "Adressen" }

type irregularCollection struct{}

func (irregularCollection) CollectionName() string { return "People" }

func TestCollectionSegmentOf_ReturnsTheDeclaredName(t *testing.T) {
	cases := map[reflect.Type]string{
		reflect.TypeOf(ptCollection{}):        "Enderecos",
		reflect.TypeOf(deCollection{}):        "Adressen",
		reflect.TypeOf(irregularCollection{}): "People",
	}
	for typ, want := range cases {
		if got := CollectionSegmentOf(typ); got != want {
			t.Errorf("CollectionSegmentOf(%s) = %q, want the declared %q", typ, got, want)
		}
	}
}

// A pointer type resolves to the same name as its element type: the aggregate
// stores children as either, and the read side must not see two segments.
func TestCollectionSegmentOf_PointerAndValueAgree(t *testing.T) {
	val := CollectionSegmentOf(reflect.TypeOf(ptCollection{}))
	ptr := CollectionSegmentOf(reflect.TypeOf(&ptCollection{}))
	if val != ptr {
		t.Errorf("value = %q, pointer = %q — a child's collection has ONE name", val, ptr)
	}
}

// The resolution is memoized per type; a second call must not re-enter the
// method (the contract says the name is constant, and the validation path runs
// on every aggregate write).
type countingCollection struct{}

var countingCollectionCalls int

func (countingCollection) CollectionName() string {
	countingCollectionCalls++
	return "Countings"
}

func TestCollectionSegmentOf_ResolvesOncePerType(t *testing.T) {
	typ := reflect.TypeOf(countingCollection{})
	for i := 0; i < 5; i++ {
		CollectionSegmentOf(typ)
	}
	if countingCollectionCalls != 1 {
		t.Errorf("CollectionName called %d times, want 1 (cached per type)", countingCollectionCalls)
	}
}

type emptyCollection struct{}

func (emptyCollection) CollectionName() string { return "" }

type lowerCollection struct{}

func (lowerCollection) CollectionName() string { return "enderecos" }

type punctuatedCollection struct{}

func (punctuatedCollection) CollectionName() string { return "Order_Lines" }

type unnamedCollection struct{}

// Every rejection is a panic naming the offending type and what is wrong with
// the declaration: the name doubles as a Go segment and a document key, so an
// unusable one must never reach a running process.
func TestCollectionSegmentOf_RejectsInvalidDeclarations(t *testing.T) {
	cases := map[string]struct {
		typ  reflect.Type
		want string
	}{
		"empty":       {reflect.TypeOf(emptyCollection{}), "the name is empty"},
		"lowercase":   {reflect.TypeOf(lowerCollection{}), "ASCII uppercase letter"},
		"punctuation": {reflect.TypeOf(punctuatedCollection{}), "not a letter or a digit"},
		"undeclared":  {reflect.TypeOf(unnamedCollection{}), "does not declare CollectionName()"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				r := recover()
				if r == nil {
					t.Fatalf("an invalid declaration must panic")
				}
				if msg, _ := r.(string); !strings.Contains(msg, c.want) {
					t.Errorf("panic = %v, want it to explain %q", r, c.want)
				}
			}()
			CollectionSegmentOf(c.typ)
		})
	}
}

func TestCollectionSegmentOf_NilType(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatalf("a nil type must panic rather than resolve to an empty segment")
		}
	}()
	CollectionSegmentOf(nil)
}

// A name may carry non-ASCII letters past the first rune — "Endereços" is a
// valid Go field name and a valid collection. Only the FIRST rune is held to
// A-Z, so the lower-camel wire rendering stays well defined.
type accentedCollection struct{}

func (accentedCollection) CollectionName() string { return "Endereços" }

func TestCollectionSegmentOf_AcceptsNonASCIILetters(t *testing.T) {
	if got := CollectionSegmentOf(reflect.TypeOf(accentedCollection{})); got != "Endereços" {
		t.Errorf("got %q, want the declared accented name", got)
	}
}

// The wire path renders the DECLARED name in camelCase — the one derivation the
// framework still applies, and a purely mechanical one.
func TestChildCollectionSegment_LowerCamelsTheDeclaredName(t *testing.T) {
	if got := childCollectionSegment(reflect.TypeOf(ptCollection{})); got != "enderecos" {
		t.Errorf("wire segment = %q, want the declared name lower-camelled", got)
	}
}
