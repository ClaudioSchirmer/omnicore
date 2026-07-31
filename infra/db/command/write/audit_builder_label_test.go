package write

import (
	"testing"

	"github.com/google/uuid"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// labelTestEntity exercises `labelKey:"..."` tags on flat fields. Only Name is
// labeled — Email is intentionally bare so the test can assert both branches
// of the FieldLabelKey rule (populated vs omitted) inside one Changes slice.
type labelTestEntity struct {
	domain.BaseEntity
	Name  string `labelKey:"UserNameField"`
	Email string
}

func (e *labelTestEntity) Modes() []domain.EntityMode {
	return []domain.EntityMode{
		domain.ModeInsert, domain.ModeUpdate, domain.ModeDelete,
	}
}
func (e *labelTestEntity) BuildRules(string, domain.Service, *domain.Rules) {}

// labelTestAddress is the aggregate child carrying a label tag; surfaces via
// ChildEvent.Changes when the root is updated and the child changes.
type labelTestAddress struct {
	domain.Managed
	ZipCode string `labelKey:"AddressZipCodeField"`
	Bare    string
}

func (a labelTestAddress) BuildRules(string, domain.Service, *domain.Rules) {}

// labelTestAggregate roots the aggregate so the auditor's children path fires.
type labelTestAggregate struct {
	domain.AggregateRoot
	Name string `labelKey:"AggregateNameField"`
}

func (e *labelTestAggregate) Modes() []domain.EntityMode {
	return []domain.EntityMode{
		domain.ModeInsert, domain.ModeUpdate, domain.ModeDelete,
	}
}
func (e *labelTestAggregate) BuildRules(string, domain.Service, *domain.Rules) {}
func (e *labelTestAggregate) GetAggregateRoot() *domain.AggregateRoot {
	return &e.AggregateRoot
}
func (e *labelTestAggregate) AggregateChildren() []domain.AggregateValueObject {
	return []domain.AggregateValueObject{labelTestAddress{}}
}

var labelTestEntitySchema = NewTableSchema[*labelTestEntity]("label_test_entities").
	ID("id").
	Field("Name", "name").
	Field("Email", "email")

var labelTestAggSchema = NewTableSchema[*labelTestAggregate]("label_test_aggregates").
	ID("id").
	Field("Name", "name").
	Child(NewTableSchema[labelTestAddress]("label_test_addresses").
		ID("id").
		ParentID("label_test_aggregate_id").
		Field("ZipCode", "zip_code").
		Field("Bare", "bare"))

type labelFixture struct {
	Tagged   string `labelKey:"TaggedKey"`
	Bare     string
	Dashed   string `labelKey:"-"`
	EmptyTag string `labelKey:""`
}

var labelFixtureSchema = NewTableSchema[labelFixture]("label_fixtures").
	Field("Tagged", "tagged").
	Field("Bare", "bare").
	Field("Dashed", "dashed").
	Field("EmptyTag", "empty_tag")

func TestBuildUpdateEvent_FieldChangeCarriesLabelKeyWhenTagPresent(t *testing.T) {
	e := &labelTestEntity{Name: "alice", Email: "a@x.com"}
	e.SetID(domain.NewID(uuid.NewString()))

	u, err := domain.GetUpdatable(e, func(t *labelTestEntity) error {
		t.Name = "alicia"   // labeled change
		t.Email = "b@x.com" // unlabeled change
		return nil
	}, nil, "GetUpdatable")
	if err != nil {
		t.Fatalf("GetUpdatable: %v", err)
	}

	ev := BuildUpdateEvent(newBuilderCtx(), u, labelTestEntitySchema, nil)
	if ev.Kind != "delta" {
		t.Fatalf("Kind = %q, want delta", ev.Kind)
	}

	var nameChange, emailChange *struct{ field, key string }
	for _, ch := range ev.Changes {
		if ch.Field == "Name" {
			nameChange = &struct{ field, key string }{ch.Field, ch.FieldLabelKey}
		}
		if ch.Field == "Email" {
			emailChange = &struct{ field, key string }{ch.Field, ch.FieldLabelKey}
		}
	}
	if nameChange == nil || nameChange.key != "UserNameField" {
		t.Errorf("name change FieldLabelKey = %+v, want UserNameField", nameChange)
	}
	if emailChange == nil || emailChange.key != "" {
		t.Errorf("email change FieldLabelKey = %+v, want empty (no label tag)", emailChange)
	}
}

func TestBuildUpdateEvent_ChildEventChangesCarryLabelKey(t *testing.T) {
	root := &labelTestAggregate{Name: "agg"}
	root.SetID(domain.NewID(uuid.NewString()))

	// Seed an existing child (CONSTRUCTOR — trusted DB-loaded state)
	root.AggregateConstructor([]domain.AggregateValueObject{
		domain.WithID(labelTestAddress{ZipCode: "10000", Bare: "before"}, domain.NewID("addr-1")),
	})

	u, err := domain.GetUpdatable(root, func(r *labelTestAggregate) error {
		// Replace the same child with mutated values → CurrentStatus=Changed,
		// surfacing via ChildEvent.Changes.
		domain.ChangeAggregateChild(r,
			domain.WithID(labelTestAddress{ZipCode: "10000", Bare: "before"}, domain.NewID("addr-1")),
			domain.WithID(labelTestAddress{ZipCode: "20000", Bare: "after"}, domain.NewID("addr-1")),
		)
		return nil
	}, nil, "GetUpdatable")
	if err != nil {
		t.Fatalf("GetUpdatable: %v", err)
	}

	ev := BuildUpdateEvent(newBuilderCtx(), u, labelTestAggSchema, nil)
	children, ok := ev.Children["labelTestAddress"]
	if !ok || len(children) == 0 {
		t.Fatalf("expected at least one labelTestAddress child event; got %+v", ev.Children)
	}
	var found *struct{ field, key string }
	var bareFound *struct{ field, key string }
	for _, ch := range children {
		if ch.Op != "updated" {
			continue
		}
		for _, fc := range ch.Changes {
			if fc.Field == "ZipCode" {
				found = &struct{ field, key string }{fc.Field, fc.FieldLabelKey}
			}
			if fc.Field == "Bare" {
				bareFound = &struct{ field, key string }{fc.Field, fc.FieldLabelKey}
			}
		}
	}
	if found == nil || found.key != "AddressZipCodeField" {
		t.Errorf("child zip_code FieldLabelKey = %+v, want AddressZipCodeField", found)
	}
	if bareFound == nil || bareFound.key != "" {
		t.Errorf("child bare FieldLabelKey = %+v, want empty (no label tag)", bareFound)
	}
}

func TestComputeChanges_NilLabelMapEmitsEmptyKeys(t *testing.T) {
	prev := map[string]any{"name": "a", "email": "a@x"}
	cur := map[string]any{"name": "b", "email": "b@x"}
	out := computeChanges(prev, cur, nil)
	if len(out) != 2 {
		t.Fatalf("expected 2 changes, got %d", len(out))
	}
	for _, ch := range out {
		if ch.FieldLabelKey != "" {
			t.Errorf("nil labelsByCol should keep FieldLabelKey empty, got %q on %q", ch.FieldLabelKey, ch.Field)
		}
	}
}

func TestLabelKeysByGoField_BuildsFromExportedTaggedFields(t *testing.T) {
	got := labelFixtureSchema.LabelKeysByGoField()
	if got["Tagged"] != "TaggedKey" {
		t.Errorf("Tagged field missing or wrong: %+v", got)
	}
	if _, has := got["Bare"]; has {
		t.Errorf("Bare field must not appear in label map (no tag)")
	}
	if _, has := got["Dashed"]; has {
		t.Errorf("Dashed field must not appear (label:\"-\")")
	}
	if _, has := got["EmptyTag"]; has {
		t.Errorf("EmptyTag field must not appear (label:\"\")")
	}
}
