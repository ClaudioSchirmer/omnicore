package domain

import (
	"testing"

	"github.com/google/uuid"
)

// ─── aggregate_root.go: addAggregateItem / isAggregateItemValid branches ─────

// TestAddAggregateItem_ReAddRemovedReactivates covers the StatusRemoved branch
// of addAggregateItem: re-adding a previously removed item flips it back to
// StatusAdded rather than appending a duplicate.
func TestAddAggregateItem_ReAddRemovedReactivates(t *testing.T) {
	p := newProviderEntity()
	item := Rec{ID: "1", Name: "a"}
	p.AggregateConstructor([]AggregateValueObject{item})
	RemoveAggregateChild(p, item)

	if got := GetRemovedItemsOf[Rec](&p.AggregateRoot); len(got) != 1 {
		t.Fatalf("expected 1 removed item, got %d", len(got))
	}

	AddAggregateChild(p, item)

	added := GetAddedItemsOf[Rec](&p.AggregateRoot)
	if len(added) != 1 {
		t.Fatalf("expected re-added item to be StatusAdded, got %d added", len(added))
	}
	if removed := GetRemovedItemsOf[Rec](&p.AggregateRoot); len(removed) != 0 {
		t.Fatalf("expected no removed items after re-add, got %d", len(removed))
	}
}

// TestAddAggregateItem_ReAddChangedReactivates covers the StatusChanged branch
// of addAggregateItem.
func TestAddAggregateItem_ReAddChangedReactivates(t *testing.T) {
	p := newProviderEntity()
	original := Rec{ID: "1", Name: "a"}
	p.AggregateConstructor([]AggregateValueObject{original})
	changed := Rec{ID: "1", Name: "b"}
	ChangeAggregateChild(p, original, changed)

	if got := GetChangedItemsOf[Rec](&p.AggregateRoot); len(got) != 1 {
		t.Fatalf("expected 1 changed item, got %d", len(got))
	}

	// Re-add the now-changed item value → StatusChanged branch flips to Added.
	AddAggregateChild(p, changed)
	if added := GetAddedItemsOf[Rec](&p.AggregateRoot); len(added) != 1 {
		t.Fatalf("expected changed item to flip to StatusAdded, got %d", len(added))
	}
}

// TestAddAggregateItem_AlreadyAddedEmitsNotification covers the
// StatusAdded/StatusConstructor branch that emits EntityAlreadyAdded.
func TestAddAggregateItem_AlreadyAddedEmitsNotification(t *testing.T) {
	p := newProviderEntity()
	item := Rec{ID: "1", Name: "a"}
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
	p.AggregateConstructor([]AggregateValueObject{nil, Rec{ID: "1", Name: "a"}})

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
	a := Rec{ID: "1", Name: "a"}
	b := Rec{ID: "2", Name: "b"}
	p.AggregateConstructor([]AggregateValueObject{a, b})
	ChangeAggregateChild(p, a, Rec{ID: "1", Name: "a2"})

	changed := GetChangedItemsOf[Rec](&p.AggregateRoot)
	if len(changed) != 1 {
		t.Fatalf("expected 1 changed item, got %d", len(changed))
	}
	if changed[0].Name != "a2" {
		t.Fatalf("expected changed item name 'a2', got %q", changed[0].Name)
	}
}

// ─── aggregate_root.go: modeFromActionName ───────────────────────────────────

func TestModeFromActionName(t *testing.T) {
	cases := []struct {
		action string
		want   EntityMode
	}{
		{"GetUpdatable", ModeUpdate},
		{"Update", ModeUpdate},
		{"GetDeletable", ModeDelete},
		{"Delete", ModeDelete},
		{"GetArchivable", ModeArchive},
		{"Archive", ModeArchive},
		{"GetUnarchivable", ModeUnarchive},
		{"Unarchive", ModeUnarchive},
		{"GetInsertable", ModeInsert},
		{"AdminCreate", ModeInsert},
		{"", ModeInsert},
	}
	for _, c := range cases {
		if got := modeFromActionName(c.action); got != c.want {
			t.Errorf("modeFromActionName(%q) = %v, want %v", c.action, got, c.want)
		}
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
	var s Service = ServiceBase{}
	if s == nil {
		t.Fatal("expected ServiceBase to satisfy Service")
	}
}

// ─── path_render.go: PluralizeWord / ToLowerCamel / toLowerRune ──────────────

func TestPluralizeWord(t *testing.T) {
	cases := []struct {
		in, out string
	}{
		{"", ""},
		{"Address", "Addresses"},    // ends 's' → es
		{"Box", "Boxes"},            // ends 'x' → es
		{"Buzz", "Buzzes"},          // ends 'z' → es
		{"Dish", "Dishes"},          // ends 'sh' → es
		{"Match", "Matches"},        // ends 'ch' → es
		{"Category", "Categories"},  // 'y' after consonant → ies
		{"Day", "Days"},             // 'y' after vowel → s
		{"OrderLine", "OrderLines"}, // default → s
		{"a", "as"},                 // single rune, default
	}
	for _, c := range cases {
		if got := PluralizeWord(c.in); got != c.out {
			t.Errorf("PluralizeWord(%q) = %q, want %q", c.in, got, c.out)
		}
	}
}

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

	upd, err := GetUpdatable(newSeal(), func(*sealEntity) {}, nil, "GetUpdatable")
	if err != nil {
		t.Fatalf("GetUpdatable: %v", err)
	}
	upd.entity()

	pat, err := GetPartialUpdatable(newSeal(), func(*sealEntity) {}, nil, "GetPartialUpdatable")
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
