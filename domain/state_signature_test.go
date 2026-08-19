package domain

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The signature is what turns the seal from a claim about PROVENANCE into a
// claim about VALUES. These pin both halves: what it notices, and what it must
// never mistake for tampering.

type sigChild struct {
	Managed
	Label string
	Rank  int
}

func (c sigChild) BuildRules(string, Service, *Rules) {}
func (c sigChild) CollectionName() string             { return "sig_children" }
func (c sigChild) IsSameBusinessIdentity(other AggregateValueObject) bool {
	o, ok := other.(sigChild)
	return ok && o.Label == c.Label
}

type sigEntity struct {
	AggregateRoot
	Name     string
	Seats    int
	Ratio    float64
	Nickname *string
	Since    time.Time
	Tags     []string
	Meta     map[string]string
}

func (e *sigEntity) Modes() []EntityMode {
	return []EntityMode{ModeInsert, ModeUpdate, ModeDelete, ModeArchive, ModeUnarchive}
}
func (e *sigEntity) BuildRules(string, Service, *Rules) {}
func (e *sigEntity) GetAggregateRoot() *AggregateRoot   { return &e.AggregateRoot }
func (e *sigEntity) AggregateChildren() []AggregateValueObject {
	return []AggregateValueObject{sigChild{}}
}

func newSigEntity() *sigEntity {
	nick := "acme"
	e := &sigEntity{
		Name: "Acme", Seats: 42, Ratio: 9.75, Nickname: &nick,
		Since: time.Unix(1700000000, 0).UTC(),
		Tags:  []string{"a", "b"},
		Meta:  map[string]string{"x": "1", "y": "2"},
	}
	e.SetID(NewID(uuid.NewString()))
	ensureInit(e)
	return e
}

func TestStateSignature_StableForUnchangedState(t *testing.T) {
	e := newSigEntity()
	e.AggregateConstructor([]AggregateValueObject{sigChild{Label: "home"}})

	first := stateSignature(e)
	for i := 0; i < 50; i++ {
		if got := stateSignature(e); got != first {
			t.Fatalf("the same state must hash the same every time (run %d)", i)
		}
	}
}

// Map iteration order is random in Go. If the walk did not sort, identical state
// would hash differently between calls and every write would look tampered with.
func TestStateSignature_MapAndChildOrderAreCanonical(t *testing.T) {
	a := newSigEntity()
	a.Meta = map[string]string{"x": "1", "y": "2", "z": "3"}
	b := newSigEntity()
	b.SetID(*a.GetID())
	b.Meta = map[string]string{"z": "3", "x": "1", "y": "2"} // same pairs, built in another order

	if stateSignature(a) != stateSignature(b) {
		t.Error("map key order must not change the signature")
	}
}

func TestStateSignature_NoticesEveryKindOfChange(t *testing.T) {
	base := func() *sigEntity {
		e := newSigEntity()
		e.SetID(NewID("fixed-id"))
		e.AggregateConstructor([]AggregateValueObject{sigChild{Label: "home", Rank: 1}})
		return e
	}
	original := stateSignature(base())

	for _, tc := range []struct {
		what   string
		mutate func(e *sigEntity)
	}{
		{"a string field", func(e *sigEntity) { e.Name = "Other" }},
		{"an int field", func(e *sigEntity) { e.Seats = 43 }},
		{"a float field", func(e *sigEntity) { e.Ratio = 9.76 }},
		{"a pointer becoming nil", func(e *sigEntity) { e.Nickname = nil }},
		{"a time field", func(e *sigEntity) { e.Since = e.Since.Add(time.Second) }},
		{"a slice element", func(e *sigEntity) { e.Tags = []string{"a", "c"} }},
		{"a slice growing", func(e *sigEntity) { e.Tags = []string{"a", "b", "b"} }},
		{"a map value", func(e *sigEntity) { e.Meta["x"] = "9" }},
		{"a child field", func(e *sigEntity) {
			ReplaceAggregateChildrenOf(e, []sigChild{{Label: "work", Rank: 1}})
		}},
		{"a child added", func(e *sigEntity) { AddAggregateChild(e, sigChild{Label: "work"}) }},
		{"a child removed", func(e *sigEntity) { RemoveAggregateChild(e, sigChild{Label: "home", Rank: 1}) }},
	} {
		t.Run(tc.what, func(t *testing.T) {
			e := base()
			tc.mutate(e)
			if stateSignature(e) == original {
				t.Errorf("changing %s must change the signature", tc.what)
			}
		})
	}
}

// A nil pointer and a pointer to the zero value are different states, and the
// column they persist to differs (NULL vs ”), so they must not collide.
func TestStateSignature_NilIsNotZero(t *testing.T) {
	empty := ""
	a := newSigEntity()
	a.SetID(NewID("fixed-id"))
	a.Nickname = nil
	b := newSigEntity()
	b.SetID(NewID("fixed-id"))
	b.Nickname = &empty

	if stateSignature(a) == stateSignature(b) {
		t.Error("a nil pointer must not hash like a pointer to the zero value")
	}
}

// Identity, revision and the managed timestamps live in unexported carriers, so
// they are outside the comparison — which is what lets infra stamp the minted id
// back onto the entity after an insert without tripping the check.
func TestStateSignature_IgnoresIdentityAndManagedColumns(t *testing.T) {
	e := newSigEntity()
	before := stateSignature(e)

	e.SetID(NewID(uuid.NewString()))
	now := time.Now()
	SetManagedColumns(e, 99, &now, &now, &now)

	if stateSignature(e) != before {
		t.Error("identity and managed columns must not take part in the signature")
	}
}

func TestVerifyState_PassesUntouchedAndRefusesMutated(t *testing.T) {
	e := newSigEntity()
	upd, err := GetUpdatable(e, nil, nil, "GetUpdatable")
	if err != nil {
		t.Fatalf("GetUpdatable: %v", err)
	}

	if err := upd.Verify(); err != nil {
		t.Fatalf("an untouched entity must verify: %v", err)
	}

	e.Name = "changed after the seal"
	err = upd.Verify()
	if err == nil {
		t.Fatal("a mutation after the seal must be refused")
	}
	if !strings.Contains(err.Error(), "sigEntity") {
		t.Errorf("the message must name the entity, got %q", err)
	}
}

// Each ValidEntity carries its own truth: mutating the entity invalidates the
// one already sealed, and a fresh Get* produces one that is valid again. This is
// what lets a manual handler build a candidate, change its mind, and build
// another without the framework getting in the way.
func TestVerifyState_ASecondSealIsValidWhileTheFirstIsNot(t *testing.T) {
	e := newSigEntity()
	first, err := GetUpdatable(e, nil, nil, "GetUpdatable")
	if err != nil {
		t.Fatalf("GetUpdatable: %v", err)
	}

	e.Seats = 7 // the handler changes its mind

	second, err := GetUpdatable(e, nil, nil, "GetUpdatable")
	if err != nil {
		t.Fatalf("second GetUpdatable: %v", err)
	}

	if err := second.Verify(); err != nil {
		t.Errorf("the fresh seal must be valid: %v", err)
	}
	if first.Verify() == nil {
		t.Error("the stale seal must be refused")
	}
}

// The sealed shapes are exported, so a zero value can be built outside this
// package even though it can never be populated there. It must not reach a write.
func TestVerifyState_RefusesAForgedZeroValue(t *testing.T) {
	for _, tc := range []struct {
		name   string
		verify func() error
	}{
		{"Insertable", Insertable{}.Verify},
		{"Updatable", Updatable{}.Verify},
		{"Archivable", Archivable{}.Verify},
		{"Unarchivable", Unarchivable{}.Verify},
		{"Deletable", Deletable{}.Verify},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.verify()
			if err == nil {
				t.Fatal("a zero-value write shape must be refused")
			}
			if !strings.Contains(err.Error(), "not produced by a Get*") {
				t.Errorf("the message must say where a valid one comes from, got %q", err)
			}
		})
	}
}

// The walk must handle every kind a persistable field can take — an unexercised
// branch is one that could silently hash two different states the same.
func TestStateSignature_CoversEveryFieldKind(t *testing.T) {
	build := func(f func(*widened)) uint64 {
		w := &widened{U8: 7, U64: 9, Arr: [2]int{1, 2}, Any: "x", Bytes: []byte{1, 2}}
		w.SetID(NewID("fixed"))
		ensureInit(w)
		f(w)
		return stateSignature(w)
	}
	base := build(func(*widened) {})

	for _, tc := range []struct {
		what   string
		mutate func(w *widened)
	}{
		{"a uint8", func(w *widened) { w.U8 = 8 }},
		{"a uint64", func(w *widened) { w.U64 = 10 }},
		{"an array element", func(w *widened) { w.Arr = [2]int{1, 3} }},
		{"an interface value", func(w *widened) { w.Any = "y" }},
		{"an interface going nil", func(w *widened) { w.Any = nil }},
		{"a byte slice", func(w *widened) { w.Bytes = []byte{1, 3} }},
	} {
		t.Run(tc.what, func(t *testing.T) {
			if build(tc.mutate) == base {
				t.Errorf("changing %s must change the signature", tc.what)
			}
		})
	}
}

// Map keys are sorted so the walk is deterministic; the ordering has to work for
// every key kind, not just strings.
func TestStateSignature_SortsMapKeysOfEveryKind(t *testing.T) {
	build := func() *keyed {
		k := &keyed{
			ByInt:   map[int]string{3: "c", 1: "a", 2: "b"},
			ByUint:  map[uint]string{30: "c", 10: "a", 20: "b"},
			ByFloat: map[float64]string{3.5: "c", 1.5: "a", 2.5: "b"},
			ByBool:  map[bool]string{true: "t", false: "f"},
		}
		k.SetID(NewID("fixed"))
		ensureInit(k)
		return k
	}
	first := stateSignature(build())
	for i := 0; i < 30; i++ {
		if stateSignature(build()) != first {
			t.Fatalf("map keys of every kind must order deterministically (run %d)", i)
		}
	}
}

func TestStateSignature_NilEntityIsZero(t *testing.T) {
	if got := stateSignature(nil); got != 0 {
		t.Errorf("stateSignature(nil) = %d, want 0", got)
	}
}

type widened struct {
	AggregateRoot
	U8    uint8
	U64   uint64
	Arr   [2]int
	Any   any
	Bytes []byte
}

func (w *widened) Modes() []EntityMode                { return []EntityMode{ModeUpdate} }
func (w *widened) BuildRules(string, Service, *Rules) {}

type keyed struct {
	AggregateRoot
	ByInt   map[int]string
	ByUint  map[uint]string
	ByFloat map[float64]string
	ByBool  map[bool]string // no ordering — must at least stay stable
}

func (k *keyed) Modes() []EntityMode                { return []EntityMode{ModeUpdate} }
func (k *keyed) BuildRules(string, Service, *Rules) {}
