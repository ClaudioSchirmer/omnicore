package relational

import (
	"context"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/db/criteria"
	"github.com/ClaudioSchirmer/omnicore/infra/db/query"
)

// A declared read join adds Go fields the TableSchema does not know about. The
// document is built schema-first and translated by column, so both passes drop
// them — these prove the values reach the served document anyway, and that they
// are addressable in a criteria exactly like a schema field.

// joinKit is an aggregate whose entity carries a field no schema column backs.
type joinKit struct {
	domain.AggregateRoot
	Name string
	// CustomerName arrives from a declared join, not from the kits table.
	CustomerName string
}

func (e *joinKit) Modes() []domain.EntityMode                       { return []domain.EntityMode{domain.ModeInsert} }
func (e *joinKit) BuildRules(string, domain.Service, *domain.Rules) {}
func (e *joinKit) GetAggregateRoot() *domain.AggregateRoot          { return &e.AggregateRoot }
func (e *joinKit) AggregateChildren() []domain.AggregateValueObject {
	return []domain.AggregateValueObject{joinPart{}}
}

type joinPart struct {
	domain.Managed
	Label string
	// CityName likewise arrives from a join declared on the CHILD.
	CityName string
}

func (p joinPart) BuildRules(string, domain.Service, *domain.Rules) {}
func (p joinPart) CollectionName() string                           { return "Parts" }
func (p joinPart) IsSameBusinessIdentity(o domain.AggregateValueObject) bool {
	x, ok := o.(joinPart)
	return ok && x.Label == p.Label
}

func joinPartSchema() *core.TableSchema {
	return core.NewTableSchema[joinPart]("kit_parts").ID("id").ParentID("kit_id").Field("Label", "label")
}

func joinKitSchema() *core.TableSchema {
	return core.NewTableSchema[*joinKit]("kits").ID("id").Field("Name", "name").Child(joinPartSchema())
}

// joinFieldLoader serves one aggregate and reports the join fields a declaration
// added — the shape query.AggregateReader exposes to a read model.
type joinFieldLoader struct {
	ent    domain.Entity
	fields map[string][]string
}

func (l *joinFieldLoader) FindAllEntities(context.Context, *criteria.Query) ([]domain.Entity, error) {
	if l.ent == nil {
		return nil, nil
	}
	return []domain.Entity{l.ent}, nil
}
func (l *joinFieldLoader) CountEntities(context.Context, *criteria.Query) (int64, error) {
	return 1, nil
}
func (l *joinFieldLoader) Schema() *core.TableSchema       { return joinKitSchema() }
func (l *joinFieldLoader) JoinFields() map[string][]string { return l.fields }

func joinKitEntity() *joinKit {
	e := &joinKit{Name: "starter", CustomerName: "ana"}
	e.SetID(domain.NewID("11111111-1111-1111-1111-111111111111"))
	p := joinPart{Label: "bolt", CityName: "porto"}
	p.SetID(domain.NewID("22222222-2222-2222-2222-222222222222"))
	e.AggregateConstructor([]domain.AggregateValueObject{p})
	return e
}

func joinReader(fields map[string][]string) *ViewReader {
	l := &joinFieldLoader{ent: joinKitEntity(), fields: fields}
	return NewViewReader([]*query.RelationalViewDefinition{query.RelationalView("v", l)})
}

// The root's join field is served under its Go name, next to the schema's own.
func TestJoinField_ReachesTheServedDocument(t *testing.T) {
	r := joinReader(map[string][]string{"kits": {"CustomerName"}})
	page, err := r.ReadPage(context.Background(), "v", queries.ReadCriteria{Limit: 10})
	if err != nil {
		t.Fatalf("ReadPage: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("expected one item, got %d", len(page.Items))
	}
	if page.Items[0]["CustomerName"] != "ana" {
		t.Errorf("the join field must be served, got %#v", page.Items[0]["CustomerName"])
	}
	if page.Items[0]["Name"] != "starter" {
		t.Errorf("the schema's own field must still be served, got %#v", page.Items[0]["Name"])
	}
}

// A child's join field lands on the elements of that child's segment.
func TestJoinField_ReachesTheChildElements(t *testing.T) {
	r := joinReader(map[string][]string{"kit_parts": {"CityName"}})
	page, err := r.ReadPage(context.Background(), "v", queries.ReadCriteria{Limit: 10})
	if err != nil {
		t.Fatalf("ReadPage: %v", err)
	}
	parts, ok := page.Items[0]["Parts"].([]any)
	if !ok || len(parts) != 1 {
		t.Fatalf("expected the child segment, got %#v", page.Items[0]["Parts"])
	}
	el, _ := parts[0].(map[string]any)
	if el["CityName"] != "porto" {
		t.Errorf("the child's join field must be served, got %#v", el["CityName"])
	}
	if el["label"] != "bolt" && el["Label"] != "bolt" {
		t.Errorf("the child's own field must still be served, got %#v", el)
	}
}

// With no join declared the document is exactly what it was: a field the entity
// happens to carry but no declaration mentions is NOT served.
func TestJoinField_UndeclaredFieldIsNotServed(t *testing.T) {
	r := joinReader(nil)
	page, err := r.ReadPage(context.Background(), "v", queries.ReadCriteria{Limit: 10})
	if err != nil {
		t.Fatalf("ReadPage: %v", err)
	}
	if _, present := page.Items[0]["CustomerName"]; present {
		t.Error("a field no join declared must not be served")
	}
}

// A ROOT join field is addressable in a filter — the loader resolves it, so the
// engine must admit it instead of refusing as an unknown name.
func TestJoinField_IsFilterable(t *testing.T) {
	r := joinReader(map[string][]string{"kits": {"CustomerName"}})
	_, err := r.ReadPage(context.Background(), "v", queries.ReadCriteria{
		Limit:  10,
		Filter: map[string]any{"CustomerName": "ana"},
	})
	if err != nil {
		t.Fatalf("a declared root join field must be filterable: %v", err)
	}
}

func TestJoinField_IsSortable(t *testing.T) {
	r := joinReader(map[string][]string{"kits": {"CustomerName"}})
	_, err := r.ReadPage(context.Background(), "v", queries.ReadCriteria{
		Limit:   10,
		OrderBy: []queries.OrderByField{{Field: "CustomerName"}},
	})
	if err != nil {
		t.Fatalf("a declared root join field must be sortable: %v", err)
	}
}

// A CHILD join field is load-only: filtering the root by it is the 1:N pushdown
// a single root SELECT cannot express, so it is refused like any child field.
func TestJoinField_ChildFieldIsNotFilterable(t *testing.T) {
	r := joinReader(map[string][]string{"kit_parts": {"CityName"}})
	_, err := r.ReadPage(context.Background(), "v", queries.ReadCriteria{
		Limit:  10,
		Filter: map[string]any{"CityName": "porto"},
	})
	assertUnsupportedCapability400(t, err, "CityName")
}

// Undeclared names still fail: a join widens the vocabulary by exactly what it
// declared, and not one name more.
func TestJoinField_UndeclaredNameStillRefused(t *testing.T) {
	r := joinReader(map[string][]string{"kits": {"CustomerName"}})
	_, err := r.ReadPage(context.Background(), "v", queries.ReadCriteria{
		Limit:  10,
		Filter: map[string]any{"Fantasma": "x"},
	})
	assertUnsupportedCapability400(t, err, "Fantasma")
}

// The served join field is projectable like any other: ?fields= keeps it, and
// drops it when the consumer did not ask.
func TestJoinField_HonorsTheProjection(t *testing.T) {
	r := joinReader(map[string][]string{"kits": {"CustomerName"}})
	page, err := r.ReadPage(context.Background(), "v", queries.ReadCriteria{
		Limit:      10,
		Projection: queries.ProjectOnlyPaths("CustomerName"),
	})
	if err != nil {
		t.Fatalf("ReadPage: %v", err)
	}
	item := page.Items[0]
	if item["CustomerName"] != "ana" {
		t.Errorf("the selected join field must survive the projection, got %#v", item)
	}
	if _, present := item["Name"]; present {
		t.Errorf("an unselected field must be pruned, got %#v", item)
	}
}

// ReadByID serves the join fields too — the by-id read is the same document.
func TestJoinField_ReachesTheByIDDocument(t *testing.T) {
	r := joinReader(map[string][]string{"kits": {"CustomerName"}})
	doc, ok, err := r.ReadByID(context.Background(), "v", "11111111-1111-1111-1111-111111111111", queries.ReadCriteria{})
	if err != nil || !ok {
		t.Fatalf("ReadByID = (%v, %v, %v)", doc, ok, err)
	}
	if doc["CustomerName"] != "ana" {
		t.Errorf("the join field must be served by id too, got %#v", doc["CustomerName"])
	}
}

// The child pass walks the aggregate's items. An entity that is not an aggregate
// root, or a schema with no children, has nothing to walk — and must not panic.
func TestApplyJoinFields_NoChildrenIsANoOp(t *testing.T) {
	doc := map[string]any{"Name": "x"}
	applyJoinFields(doc, &guardEnt{Name: "x"}, guardSchema("gadgets"),
		map[string][]string{"gadgets": {"Name"}})
	if doc["Name"] != "x" {
		t.Errorf("doc = %v", doc)
	}
	// A schema WITH children over a non-aggregate entity: nothing to walk.
	applyJoinFields(doc, &guardEnt{}, joinKitSchema(), map[string][]string{"kit_parts": {"CityName"}})
}

func TestApplyJoinFields_NothingDeclaredIsANoOp(t *testing.T) {
	doc := map[string]any{"Name": "x"}
	applyJoinFields(doc, joinKitEntity(), joinKitSchema(), nil)
	applyJoinFields(nil, joinKitEntity(), joinKitSchema(), map[string][]string{"kits": {"CustomerName"}})
	if len(doc) != 1 {
		t.Errorf("doc = %v", doc)
	}
}

// copyGoFields reads by name off whatever it is given: a nil pointer, a
// non-struct or an absent field are skipped rather than zero-filled, so a
// mismatch never invents a value.
func TestCopyGoFields_SkipsWhatItCannotRead(t *testing.T) {
	doc := map[string]any{}
	copyGoFields(doc, (*joinKit)(nil), []string{"CustomerName"})
	copyGoFields(doc, "not a struct", []string{"CustomerName"})
	copyGoFields(doc, &joinKit{}, []string{"NaoExiste"})
	if len(doc) != 0 {
		t.Errorf("nothing readable must produce nothing, got %v", doc)
	}
	copyGoFields(doc, &joinKit{CustomerName: "ana"}, []string{"CustomerName"})
	if doc["CustomerName"] != "ana" {
		t.Errorf("a readable field must be copied, got %v", doc)
	}
}

// A LEFT JOIN with no counterpart reads back as a nil pointer on the entity. The
// document must carry a PLAIN nil — present-with-nil, the same shape every other
// NULL in it takes — never the typed nil the field holds. A typed nil survives
// into the Result fill as "a value that exists", which is how "there is no
// counterpart" turns into an empty string on the wire.
func TestCopyGoFields_NullPointerBecomesAPlainNil(t *testing.T) {
	doc := map[string]any{}
	copyGoFields(doc, &joinNullable{}, []string{"CarrierCode"})
	v, present := doc["CarrierCode"]
	if !present {
		t.Fatalf("a NULL join field must still be PRESENT in the document, got %v", doc)
	}
	if v != nil {
		t.Fatalf("a NULL join field must be a plain nil, got %#v", v)
	}
}

// The non-nil twin is copied as the pointer it is — the reader does not flatten
// a value the consumer's Result may well declare as a pointer too.
func TestCopyGoFields_NonNullPointerIsCopied(t *testing.T) {
	code := "DHL"
	doc := map[string]any{}
	copyGoFields(doc, &joinNullable{CarrierCode: &code}, []string{"CarrierCode"})
	got, ok := doc["CarrierCode"].(*string)
	if !ok || got == nil || *got != "DHL" {
		t.Fatalf("a present join field must be copied, got %#v", doc["CarrierCode"])
	}
}

// joinNullable carries the pointer field a LeftJoin declaration requires.
type joinNullable struct {
	domain.AggregateRoot
	CarrierCode *string
}

func (e *joinNullable) Modes() []domain.EntityMode                       { return []domain.EntityMode{domain.ModeInsert} }
func (e *joinNullable) BuildRules(string, domain.Service, *domain.Rules) {}
