package domain

import (
	"reflect"
	"testing"
	"time"
)

// --- ValidateEnumNamed (the C5 string seat) ---------------------------------

func TestValidateEnumNamed(t *testing.T) {
	ctx := NewNotificationContext("X")
	if !ValidateEnumNamed(colorRed, "Shade", ctx) {
		t.Error("a declared member should validate")
	}
	if ValidateEnumNamed(colorUnknown, "Shade", ctx) {
		t.Error("the sentinel should not validate")
	}
	msgs := ctx.Messages()
	if len(msgs) != 1 || msgs[0].ResolveFieldName() != "shade" {
		t.Errorf("expected one message on %q, got %+v", "shade", msgs)
	}
	if ValidateEnumNamed(colorUnknown, "Shade", nil) {
		t.Error("a nil ctx still answers false, without panicking")
	}
}

// --- Managed carrier: getters, populate hook, stamp verbs -------------------

type managedCarrier struct{ Managed }

func TestManaged_SetManagedColumnsAndGetters(t *testing.T) {
	c := &managedCarrier{}
	now := time.Now()
	if !SetManagedColumns(c, 7, &now, &now, nil) {
		t.Fatal("a target embedding Managed must accept SetManagedColumns")
	}
	if c.GetRevision() != 7 {
		t.Errorf("revision = %d, want 7", c.GetRevision())
	}
	if c.GetCreatedAt() != &now || c.GetUpdatedAt() != &now {
		t.Error("created/updated must be the populated instants")
	}
	if c.GetDeletedAt() != nil {
		t.Error("deleted must stay nil on a live row")
	}
	if SetManagedColumns(&struct{}{}, 1, nil, nil, nil) {
		t.Error("a target with no carrier must answer false")
	}
}

func TestManaged_StampVerbsAndStampFields(t *testing.T) {
	c := &managedCarrier{}
	c.Stamp("PaidAt")
	c.StampNull("PaidAt") // two verbs on one field is one request; the last wins
	c.StampEmpty("Count")
	reqs := RequestedStamps(c)
	want := []StampRequest{{Field: "PaidAt", Op: StampToNull}, {Field: "Count", Op: StampToEmpty}}
	if !reflect.DeepEqual(reqs, want) {
		t.Fatalf("stamps = %+v, want %+v", reqs, want)
	}
	if got := StampFields(reqs); !reflect.DeepEqual(got, []string{"PaidAt", "Count"}) {
		t.Errorf("StampFields = %v", got)
	}
	if StampFields(nil) != nil {
		t.Error("no requests → nil names")
	}
	if RequestedStamps(&struct{}{}) != nil {
		t.Error("a target with no carrier requested nothing")
	}
	for op, name := range map[StampOp]string{StampFill: "Stamp", StampToNull: "StampNull", StampToEmpty: "StampEmpty"} {
		if op.String() != name {
			t.Errorf("StampOp(%d).String() = %q, want %q", op, op.String(), name)
		}
	}
}

// --- Notification base seal + kernel Semantic overrides ---------------------

func TestNotificationSemantics(t *testing.T) {
	NotificationBase{}.isNotification() // the seal method itself
	if got := (NotificationBase{}).Semantic(); got != SemanticValidation {
		t.Errorf("default semantic = %v, want SemanticValidation", got)
	}
	cases := map[NotificationSemantic]Notification{
		SemanticNotFound:      RecordNotFoundNotification{},
		SemanticSchema:        InvalidFilterValueNotification{},
		SemanticStateConflict: ConcurrentModificationNotification{},
		SemanticConflict:      EntityAlreadyAddedNotification{},
		SemanticForbidden:     InsertNotAllowedNotification{},
	}
	for want, n := range cases {
		s, ok := n.(interface{ Semantic() NotificationSemantic })
		if !ok || s.Semantic() != want {
			t.Errorf("%T.Semantic() != %v", n, want)
		}
	}
}

func TestEventType_Value(t *testing.T) {
	if EventError.Value() != int(EventError) {
		t.Errorf("EventType.Value() = %d", EventError.Value())
	}
}

// --- ApplyToAggregateItem ----------------------------------------------------

func TestApplyToAggregateItem(t *testing.T) {
	p := &refProvider{}
	ensureInit(p)
	AddAggregateChild(p, refAVO{Street: "a"})
	ar := p.GetAggregateRoot()

	id := NewID("11111111-1111-1111-1111-111111111111")
	ok := ApplyToAggregateItem(ar, refAVO{Street: "a"}, func(ptr any) bool {
		a, isAVO := ptr.(*refAVO)
		if !isAVO {
			return false
		}
		a.SetID(id)
		return true
	})
	if !ok {
		t.Fatal("a tracked item must accept the mutation")
	}
	items := GetAggregateItemsOf[refAVO](ar)
	if len(items) != 1 || items[0].Item.GetID().Value() != id.Value() {
		t.Fatalf("the mutation must land on the TRACKED value, got %+v", items)
	}

	if ApplyToAggregateItem(nil, refAVO{Street: "a"}, func(any) bool { return true }) {
		t.Error("nil root → false")
	}
	if ApplyToAggregateItem(ar, refAVO{Street: "ghost"}, func(any) bool { return true }) {
		t.Error("an untracked item → false, never an error")
	}
	if ApplyToAggregateItem(ar, refAVO{Street: "a"}, func(any) bool { return false }) {
		t.Error("a mutate that declines → false")
	}
}

// --- field-ref plumbing branches ---------------------------------------------

func TestBindFieldBase_IgnoresInvalidBases(t *testing.T) {
	assertUnbound := func(t *testing.T, base any, kind string) {
		t.Helper()
		r := NewRules(ModeInsert, NewNotificationContext("X"), nil)
		r.bindFieldBase(base)
		if r.base.IsValid() {
			t.Errorf("%s must not bind", kind)
		}
	}
	assertUnbound(t, nil, "nil base")
	assertUnbound(t, 42, "a non-pointer")
	var np *refEntity
	assertUnbound(t, np, "a nil pointer")
	s := "x"
	assertUnbound(t, &s, "a pointer to a non-struct")
}

func TestAddNotification_NilCtxIsANoOp(t *testing.T) {
	r := NewRules(ModeInsert, nil, nil)
	r.AddNotification(nil, RequiredFieldNotification{}, false) // must not resolve, must not panic
	r.AddNotificationWithVars(nil, RequiredFieldNotification{}, nil, false)
}

func TestResolveFieldRef_SelfPointerMatchesNoField(t *testing.T) {
	e := &refEntity{}
	r, _ := boundRules(t, e)
	mustPanicRef(t, "matches no exported field", func() {
		r.AddNotification(e, RequiredFieldNotification{}, false)
	})
}

type dashEntity struct {
	D string `notifyAs:"-" labelKey:"-"`
}

func TestDashTagsAreNoDeclaration(t *testing.T) {
	e := &dashEntity{}
	ctx := NewNotificationContext("X")
	r := NewRulesFor(ModeInsert, ctx, e)
	r.AddNotification(&e.D, RequiredFieldNotification{}, false)
	m := ctx.Messages()[0]
	if got := m.ResolveFieldName(); got != "d" {
		t.Errorf("a %q tag is no declaration — field = %q, want the camel default %q", "-", got, "d")
	}
	if m.LabelKey != "" {
		t.Errorf("a %q labelKey is no declaration — got %q", "-", m.LabelKey)
	}
}

func TestResolveNotifyAs_Branches(t *testing.T) {
	if resolveNotifyAs(nil, "X") != "" {
		t.Error("nil type → no token")
	}
	if resolveNotifyAs(reflect.TypeOf(eqEntity{}), "") != "" {
		t.Error("empty name → no token")
	}
	if got := resolveNotifyAs(reflect.TypeOf(&eqEntity{}), "Contact"); got != "contato" {
		t.Errorf("pointer type must resolve through Elem — got %q", got)
	}
	if resolveNotifyAs(reflect.TypeOf(42), "X") != "" {
		t.Error("non-struct → no token")
	}
}

func TestAssertDeclaredChildBuildsRules_Branches(t *testing.T) {
	assertDeclaredChildBuildsRules(nil)       // a nil sample is skipped, not resolved
	assertDeclaredChildBuildsRules(&refAVO{}) // a pointer sample unwraps and passes
	mustPanicRef(t, "POINTER receiver", func() {
		assertDeclaredChildBuildsRules(noRulesAVO{})
	})
}
