package domain

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// ─── aggregate_root.go: addAggregateItem / isAggregateItemValid branches ─────

// TestAddAggregateItem_ReAddRemovedReactivates covers the StatusRemoved branch
// of addAggregateItem: re-adding a previously removed item flips it back to
// StatusAdded rather than appending a duplicate.
func TestAddAggregateItem_ReAddRemovedReactivates(t *testing.T) {
	p := newProviderEntity()
	item := Rec{Name: "a"}
	p.AggregateConstructor([]AggregateValueObject{item})
	RemoveAggregateChild(p, item)

	if got := GetRemovedItemsOf[Rec](&p.AggregateRoot); len(got) != 1 {
		t.Fatalf("expected 1 removed item, got %d", len(got))
	}

	AddAggregateChild(p, item)

	// A re-added item that ORIGINALLY came from the DB (Constructor) is an UPDATE of
	// the existing row, not a fresh INSERT — categorized by OperationOf(original,
	// current), not currentStatus alone. (Mirrors the reference ddd-kernel.)
	if changed := GetChangedItemsOf[Rec](&p.AggregateRoot); len(changed) != 1 {
		t.Fatalf("re-adding a removed DB item must categorize as changed (UPDATE), got %d", len(changed))
	}
	if added := GetAddedItemsOf[Rec](&p.AggregateRoot); len(added) != 0 {
		t.Fatalf("a re-added DB item must NOT be an insert, got %d added", len(added))
	}
	if removed := GetRemovedItemsOf[Rec](&p.AggregateRoot); len(removed) != 0 {
		t.Fatalf("expected no removed items after re-add, got %d", len(removed))
	}
}

// TestAddAggregateItem_ReAddChangedReactivates covers the StatusChanged branch
// of addAggregateItem.
func TestAddAggregateItem_ReAddChangedReactivates(t *testing.T) {
	p := newProviderEntity()
	original := Rec{Name: "a"}
	p.AggregateConstructor([]AggregateValueObject{original})
	changed := Rec{Name: "b"}
	ChangeAggregateChild(p, original, changed)

	if got := GetChangedItemsOf[Rec](&p.AggregateRoot); len(got) != 1 {
		t.Fatalf("expected 1 changed item, got %d", len(got))
	}

	// Re-add the now-changed item: the setter flips currentStatus to Added, but since
	// it ORIGINALLY came from the DB (Constructor) it stays an UPDATE, not an INSERT.
	AddAggregateChild(p, changed)
	if changedItems := GetChangedItemsOf[Rec](&p.AggregateRoot); len(changedItems) != 1 {
		t.Fatalf("a re-added DB item must remain a changed (UPDATE) item, got %d", len(changedItems))
	}
	if added := GetAddedItemsOf[Rec](&p.AggregateRoot); len(added) != 0 {
		t.Fatalf("a re-added DB item must NOT be an insert, got %d", len(added))
	}
}

// TestAddAggregateItem_AlreadyAddedEmitsNotification covers the
// StatusAdded/StatusConstructor branch that emits EntityAlreadyAdded.
func TestAddAggregateItem_AlreadyAddedEmitsNotification(t *testing.T) {
	p := newProviderEntity()
	item := Rec{Name: "a"}
	AddAggregateChild(p, item)
	AddAggregateChild(p, item) // duplicate → notification, no second entry

	if added := GetAddedItemsOf[Rec](&p.AggregateRoot); len(added) != 1 {
		t.Fatalf("expected exactly 1 added entry after duplicate add, got %d", len(added))
	}
	if !contextHasNotification(p.NotificationContext(), "EntityAlreadyAddedNotification") {
		t.Fatalf("expected EntityAlreadyAddedNotification after duplicate add")
	}
}

// TestAggregateConstructor_NilItemRejected covers the nil branch of
// isAggregateItemValid (reached through the trusted constructor path).
func TestAggregateConstructor_NilItemRejected(t *testing.T) {
	p := newProviderEntity()
	p.AggregateConstructor([]AggregateValueObject{nil, Rec{Name: "a"}})

	all := p.AllAggregateItems()
	if len(all["Rec"]) != 1 {
		t.Fatalf("expected nil item skipped and 1 Rec kept, got %d", len(all["Rec"]))
	}
	if !contextHasNotification(p.NotificationContext(), "EntityDoesNotExistNotification") {
		t.Fatalf("expected EntityDoesNotExistNotification for nil item")
	}
}

// ─── aggregate_root.go: GetChangedItemsOf ────────────────────────────────────

func TestGetChangedItemsOf_ReturnsChangedOnly(t *testing.T) {
	p := newProviderEntity()
	a := Rec{Name: "a"}
	b := Rec{Name: "b"}
	p.AggregateConstructor([]AggregateValueObject{a, b})
	ChangeAggregateChild(p, a, Rec{Name: "a2"})

	changed := GetChangedItemsOf[Rec](&p.AggregateRoot)
	if len(changed) != 1 {
		t.Fatalf("expected 1 changed item, got %d", len(changed))
	}
	if changed[0].Name != "a2" {
		t.Fatalf("expected changed item name 'a2', got %q", changed[0].Name)
	}
}

// ─── entity_base.go: checkService ────────────────────────────────────────────

// serviceRequiredEntity declares RequiresService()==true and ModeInsert, so a
// GetInsertable with a nil service trips the checkService rejection branch.
type serviceRequiredEntity struct {
	BaseEntity
}

func (s *serviceRequiredEntity) Modes() []EntityMode                { return []EntityMode{ModeInsert} }
func (s *serviceRequiredEntity) BuildRules(string, Service, *Rules) {}
func (s *serviceRequiredEntity) RequiresService() bool              { return true }

func TestCheckService_NilServiceRejected(t *testing.T) {
	e := &serviceRequiredEntity{}
	_, err := GetInsertable(e, nil, "GetInsertable")
	if err == nil {
		t.Fatal("expected GetInsertable to fail when service is required but nil")
	}
	if !hasNotification(err, "ServiceIsRequiredNotification") {
		t.Fatalf("expected ServiceIsRequiredNotification, got %v", err)
	}
}

func TestCheckService_ProvidedServiceAllowed(t *testing.T) {
	e := &serviceRequiredEntity{}
	if _, err := GetInsertable(e, ServiceBase{}, "GetInsertable"); err != nil {
		t.Fatalf("expected GetInsertable to succeed when service provided, got %v", err)
	}
}

// ─── entity_base.go: AddFieldNameAlias (non-nil map branch) ───────────────────

func TestAddFieldNameAlias_SecondCallReusesMap(t *testing.T) {
	e := &plainEntity{}
	e.AddFieldNameAlias("Email", "mail")
	e.AddFieldNameAlias("Name", "fullName") // map already non-nil → reuse branch

	if got := e.fieldAliases(); len(got) != 2 {
		t.Fatalf("expected 2 aliases, got %d", len(got))
	}
	if e.fieldAliases()["Email"] != "mail" || e.fieldAliases()["Name"] != "fullName" {
		t.Fatalf("unexpected alias map contents: %v", e.fieldAliases())
	}
}

// ─── notification.go: formatFieldValue branches ──────────────────────────────

func TestFormatFieldValue(t *testing.T) {
	str := "hello"
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"nil", nil, ""},
		{"string", "abc", "abc"},
		{"ptrString", &str, "hello"},
		{"nilPtrString", (*string)(nil), ""},
		{"int", 42, "42"},
		{"bool", true, "true"},
	}
	for _, c := range cases {
		if got := formatFieldValue(c.in); got != c.want {
			t.Errorf("%s: formatFieldValue(%v) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

// stringerVO stands for a value object that renders itself through a POINTER
// receiver — unwrapping one would discard the only method that knows how to
// print it.
type stringerVO struct{ raw string }

func (s *stringerVO) String() string { return "VO(" + s.raw + ")" }

// TestFormatFieldValue_DereferencesEveryPointer pins the fix. A rule echoing an
// optional field answered the caller with a memory address, because only *string
// had a case and fmt.Sprint renders a pointer as its address unless the type
// renders itself. That was invisible to every test asserting "some value came
// back": an address IS a valid Go rendering of a pointer.
//
// The ptrTime/ptrStringer cases are the other half of the contract and are NOT
// regressions being fixed — they always worked, because time.Time has String().
// They are here so a future simplification of the loop cannot quietly unwrap
// them and print a bare struct.
func TestFormatFieldValue_DereferencesEveryPointer(t *testing.T) {
	i, i64, f, b := 15, int64(-7), 2.5, true
	when := time.Date(2026, 8, 15, 10, 30, 0, 0, time.UTC)
	type email string // a raw value object: a named type over a base type
	mail := email("a@b.c")
	vo := &stringerVO{raw: "x"}

	cases := []struct {
		name string
		in   any
		want string
	}{
		{"ptrInt", &i, "15"},
		{"ptrInt64", &i64, "-7"},
		{"ptrFloat", &f, "2.5"},
		{"ptrBool", &b, "true"},
		{"ptrTime", &when, when.String()},
		{"ptrNamedString", &mail, "a@b.c"},
		{"nilPtrInt", (*int)(nil), ""},
		{"nilPtrTime", (*time.Time)(nil), ""},
		// The Stringer keeps its own rendering rather than being unwrapped.
		{"ptrStringer", vo, "VO(x)"},
		{"nilPtrStringer", (*stringerVO)(nil), ""},
	}
	for _, c := range cases {
		if got := formatFieldValue(c.in); got != c.want {
			t.Errorf("%s: formatFieldValue = %q, want %q", c.name, got, c.want)
		}
		if strings.HasPrefix(formatFieldValue(c.in), "0x") {
			t.Errorf("%s: leaked a pointer address into FieldValue", c.name)
		}
	}
}

// TestAddNotification_EchoesOptionalFieldValue proves the fix through the path
// a generated rule actually takes: r.AddNotification with the entity's own
// optional field.
func TestAddNotification_EchoesOptionalFieldValue(t *testing.T) {
	rating := 15
	ctx := NewNotificationContext("Visit")
	r := NewRules(ModeInsert, ctx, nil)
	r.AddNotification("Rating", RequiredFieldNotification{}, &rating)

	msgs := ctx.Messages()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0].FieldValue != "15" {
		t.Errorf("FieldValue = %q, want \"15\" — an optional field must echo its value, not its address", msgs[0].FieldValue)
	}
}

// TestAddNotification_FormatsValueThroughFieldValue exercises formatFieldValue
// via the public AddNotification variadic path for each value flavour.
func TestAddNotification_FormatsValueThroughFieldValue(t *testing.T) {
	ctx := NewNotificationContext("User")
	ctx.AddNotification("Age", RequiredFieldNotification{}, 30)
	ctx.AddNotification("Name", RequiredFieldNotification{}, nil)

	msgs := ctx.Messages()
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
	if msgs[0].FieldValue != "30" {
		t.Errorf("expected FieldValue '30', got %q", msgs[0].FieldValue)
	}
	if msgs[1].FieldValue != "" {
		t.Errorf("expected empty FieldValue for nil, got %q", msgs[1].FieldValue)
	}
}

// ─── notification.go / notification_core.go: marker seals + Semantic ─────────

func TestNotificationSeals_AndSemantics(t *testing.T) {
	// isNotification is the marker seal — invoke it through each base.
	NotificationBase{}.isNotification()
	DomainNotificationBase{}.isNotification()
	ApplicationNotificationBase{}.isNotification()
	InfrastructureNotificationBase{}.isNotification()

	if got := (NotificationBase{}).Semantic(); got != SemanticValidation {
		t.Errorf("NotificationBase default semantic = %v, want SemanticValidation", got)
	}
	if got := (LimitExceededNotification{}).Semantic(); got != SemanticSchema {
		t.Errorf("LimitExceededNotification semantic = %v, want SemanticSchema", got)
	}
}

// ─── service.go: isService seal ──────────────────────────────────────────────

func TestServiceBase_IsServiceSeal(t *testing.T) {
	ServiceBase{}.isService()
	// Compile-time assertion that ServiceBase satisfies Service (the seal method
	// is promoted); a runtime nil check would be meaningless on a concrete value.
	var _ Service = ServiceBase{}
}

// ─── path_render.go: ToLowerCamel / toLowerRune ──────────────────────────────

func TestToLowerCamel_Exported(t *testing.T) {
	cases := []struct {
		in, out string
	}{
		{"ZipCode", "zipCode"},
		{"URLPath", "urlPath"},
		{"id", "id"},
		{"", ""},
	}
	for _, c := range cases {
		if got := ToLowerCamel(c.in); got != c.out {
			t.Errorf("ToLowerCamel(%q) = %q, want %q", c.in, got, c.out)
		}
	}
}

func TestToLowerRune(t *testing.T) {
	if got := toLowerRune('A'); got != 'a' {
		t.Errorf("toLowerRune('A') = %q, want 'a'", got)
	}
	// Non-uppercase passes through unchanged (the else branch).
	if got := toLowerRune('z'); got != 'z' {
		t.Errorf("toLowerRune('z') = %q, want 'z'", got)
	}
	if got := toLowerRune('5'); got != '5' {
		t.Errorf("toLowerRune('5') = %q, want '5'", got)
	}
}

// ─── entity.go: ValidEntity entity() seal markers ────────────────────────────

// sealEntity is a minimal entity declaring all five lifecycle modes so every
// Get* constructor succeeds, letting us exercise each ValidEntity entity() seal.
type sealEntity struct {
	BaseEntity
}

func (s *sealEntity) Modes() []EntityMode {
	return []EntityMode{ModeInsert, ModeUpdate, ModeDelete, ModeArchive, ModeUnarchive}
}
func (s *sealEntity) BuildRules(string, Service, *Rules) {}

func TestValidEntity_EntitySeals(t *testing.T) {
	newSeal := func() *sealEntity {
		e := &sealEntity{}
		e.SetID(NewID(uuid.NewString()))
		return e
	}

	// Insert rejects a pre-set ID, so build the insertable without one.
	ins, err := GetInsertable(&sealEntity{}, nil, "GetInsertable")
	if err != nil {
		t.Fatalf("GetInsertable: %v", err)
	}
	ins.entity()

	upd, err := GetUpdatable(newSeal(), func(*sealEntity) error { return nil }, nil, "GetUpdatable")
	if err != nil {
		t.Fatalf("GetUpdatable: %v", err)
	}
	upd.entity()

	pat, err := GetPartialUpdatable(newSeal(), func(*sealEntity) error { return nil }, nil, "GetPartialUpdatable")
	if err != nil {
		t.Fatalf("GetPartialUpdatable: %v", err)
	}
	pat.entity()

	del, err := GetDeletable(newSeal(), nil, "GetDeletable")
	if err != nil {
		t.Fatalf("GetDeletable: %v", err)
	}
	del.entity()

	arc, err := GetArchivable(newSeal(), nil, "GetArchivable")
	if err != nil {
		t.Fatalf("GetArchivable: %v", err)
	}
	arc.entity()

	unarc, err := GetUnarchivable(newSeal(), nil, "GetUnarchivable")
	if err != nil {
		t.Fatalf("GetUnarchivable: %v", err)
	}
	unarc.entity()

	batch := NewBatch([]ValidEntity{ins, upd, del})
	batch.entity()
	if len(batch.Operations()) != 3 {
		t.Fatalf("expected 3 batch operations, got %d", len(batch.Operations()))
	}
}

// contextHasNotification scans a NotificationContext's messages for a typeName.
func contextHasNotification(ctx *NotificationContext, typeName string) bool {
	if ctx == nil {
		return false
	}
	for _, msg := range ctx.Messages() {
		if NotificationKey(msg.Notification) == typeName {
			return true
		}
	}
	return false
}
