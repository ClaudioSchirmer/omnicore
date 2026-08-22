package relational

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/command/read"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/db/criteria"
	"github.com/ClaudioSchirmer/omnicore/infra/db/query"
)

// fakeLoader is a no-DB query.AggregateReader: it records whether the load ran
// and returns an empty result, so a MaxLimit test can prove the ceiling rejects
// BEFORE any load and that BypassMaxLimit lets the load through.
type fakeLoader struct {
	table      string
	findCalled bool
}

func (f *fakeLoader) FindAllEntities(context.Context, *criteria.Query) ([]domain.Entity, error) {
	f.findCalled = true
	return nil, nil
}
func (f *fakeLoader) CountEntities(context.Context, *criteria.Query) (int64, error) { return 0, nil }
func (f *fakeLoader) Schema() *core.TableSchema                                     { return guardSchema(f.table) }

// guardEnt is a minimal entity — enough to build an AggregateLoader over a schema
// without a database: the declaration only ever reads the loader's schema.
type guardEnt struct {
	domain.BaseEntity
	Name string
	// Material is the field a 1:1 sibling schema maps (a sibling is over the
	// owner's Go type); guardSchema does not map it, siblingSchema does.
	Material string
}

func (e *guardEnt) Modes() []domain.EntityMode                       { return []domain.EntityMode{domain.ModeInsert} }
func (e *guardEnt) BuildRules(string, domain.Service, *domain.Rules) {}

func guardSchema(table string) *core.TableSchema {
	return core.NewTableSchema[*guardEnt](table).ID("id").Field("Name", "name")
}

func guardLoader(table string) query.AggregateReader {
	return read.NewAggregateLoader[*guardEnt](nil, func() *guardEnt { return &guardEnt{} }).WithSchema(guardSchema(table))
}

// A well-formed declaration registers. There is no loader/schema cross-check to
// run any more: the loader IS the declaration's only structural input and the
// schema comes from it, so the mismatch the old boot guard existed for cannot be
// expressed.
func TestNewViewReader_RegistersADeclaredView(t *testing.T) {
	r := NewViewReader([]*query.RelationalViewDefinition{
		query.RelationalView("v", guardLoader("gadgets")),
	})
	if r.Empty() {
		t.Fatal("a declared relational view must be registered")
	}
	if names := r.ViewNames(); !names["v"] {
		t.Errorf("ViewNames() must route the declared name, got %v", names)
	}
}

// A malformed entry is SKIPPED, never panicked on: the declarations reaching this
// constructor are boot-validated (query.ValidateRelationalViews), so a nil loader
// has already aborted the boot with an actionable message. Skipping is the
// belt-and-braces posture, not the diagnostic.
func TestNewViewReader_SkipsMalformedDeclarations(t *testing.T) {
	r := NewViewReader([]*query.RelationalViewDefinition{
		nil,
		query.RelationalView("no_loader", nil),
	})
	if !r.Empty() {
		t.Fatal("a malformed declaration must not be registered")
	}
}

// assertUnsupportedCapability400 checks that a capability the relational reader
// cannot serve surfaces as a NotificationCarrier whose single notification is a
// UnsupportedCapabilityNotification with SemanticSchema — the wire mapping turns
// that into a 400 (not a generic 500), and the offending field/capability rides
// through as the notification's field name.
func assertUnsupportedCapability400(t *testing.T, err error, wantField string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an unsupported-capability error, got nil")
	}
	var carrier domain.NotificationCarrier
	if !errors.As(err, &carrier) {
		t.Fatalf("error must be a NotificationCarrier (maps to a typed status), got %T: %v", err, err)
	}
	ctxs := carrier.NotificationContexts()
	if len(ctxs) != 1 || len(ctxs[0].Messages()) != 1 {
		t.Fatalf("expected exactly one notification, got contexts=%d", len(ctxs))
	}
	msg := ctxs[0].Messages()[0]
	if got := reflect.TypeOf(msg.Notification).Name(); got != "UnsupportedCapabilityNotification" {
		t.Errorf("notification = %q, want UnsupportedCapabilityNotification", got)
	}
	if got := msg.Notification.Semantic(); got != domain.SemanticSchema {
		t.Errorf("semantic = %v, want SemanticSchema (→400)", got)
	}
	if msg.ResolveFieldName() != wantField && msg.FieldName != wantField {
		t.Errorf("field = %q, want the offending capability %q", msg.ResolveFieldName(), wantField)
	}
}

// TestUnsupportedChildFilter_MapsTo400 covers a filter pushed at a child (dotted)
// field: a root SELECT cannot express it, so the reader rejects it as a 400.
func TestUnsupportedChildFilter_MapsTo400(t *testing.T) {
	_, err := toExpr(guardSchema("gadgets"), map[string]any{"Addresses.ZipCode": "12345"})
	assertUnsupportedCapability400(t, err, "Addresses.ZipCode")
}

// TestUnsupportedChildSort_MapsTo400 covers a sort on a child (dotted) field:
// a root ORDER BY cannot express it, so the reader rejects it as a 400.
func TestUnsupportedChildSort_MapsTo400(t *testing.T) {
	err := applySort(guardSchema("gadgets"), criteria.Where(nil), []queries.OrderByField{{Field: "Addresses.ZipCode"}})
	assertUnsupportedCapability400(t, err, "Addresses.ZipCode")
}

// siblingSchema is guardSchema plus a 1:1 sibling (qa satellite) carrying
// "Material" — the shared-PK secondary the loader reaches with a LEFT JOIN.
func siblingSchema(table string) *core.TableSchema {
	return core.NewTableSchema[*guardEnt](table).ID("id").Field("Name", "name").
		Sibling(core.NewSiblingSchema[*guardEnt](table+"_specs").Field("Material", "material"))
}

// baseEnt is a shared-base ROLE entity: it embeds the base's identity and adds a
// role-private field, so a schema over it can declare SharedBase (the loader then
// LEFT JOINs the base to reach its fields for a filter/sort).
type baseEnt struct {
	domain.BaseEntity
	HolderName string
	// DisplayName is a shared-base field; the role must carry every base field
	// as an exported Go field (the shared columns scan into the role struct).
	DisplayName string
}

func (e *baseEnt) Modes() []domain.EntityMode                       { return []domain.EntityMode{domain.ModeInsert} }
func (e *baseEnt) BuildRules(string, domain.Service, *domain.Rules) {}

// sharedBaseSchema is a role schema whose shared base owns "DisplayName" — a base
// field the relational reader must now treat as servable (1:1 base JOIN).
func sharedBaseSchema(table string) *core.TableSchema {
	base := core.NewSharedBaseSchema(table+"_base").ID("id").Revision("revision").
		Field("DisplayName", "display_name").NaturalID("display_name")
	return core.NewTableSchema[*baseEnt](table).ID("id").
		SharedBase(base, "id").Field("HolderName", "holder_name")
}

// TestServableSiblingField_Passes is the sibling half of the 1:1 relaxation: a
// flat (non-dotted) root-level sibling field (Material) lives in a shared-PK
// satellite the loader LEFT JOINs, so a filter AND a sort on it are servable —
// no longer the 400 they once were.
func TestServableSiblingField_Passes(t *testing.T) {
	if _, err := toExpr(siblingSchema("gadgets"), map[string]any{"Material": "steel"}); err != nil {
		t.Fatalf("a root-level sibling field must be servable (1:1 LEFT JOIN), got %v", err)
	}
	if err := applySort(siblingSchema("gadgets"), criteria.Where(nil), []queries.OrderByField{{Field: "Material"}}); err != nil {
		t.Fatalf("a sort on a root-level sibling field must be servable, got %v", err)
	}
}

// TestServableSharedBaseField_Passes is the shared-base half of the 1:1
// relaxation: a base field (DisplayName) the loader reaches by joining the role
// to its shared base is servable for both filter and sort.
func TestServableSharedBaseField_Passes(t *testing.T) {
	if _, err := toExpr(sharedBaseSchema("holders"), map[string]any{"DisplayName": "ACME"}); err != nil {
		t.Fatalf("a shared-base field must be servable (1:1 base JOIN), got %v", err)
	}
	if err := applySort(sharedBaseSchema("holders"), criteria.Where(nil), []queries.OrderByField{{Field: "DisplayName"}}); err != nil {
		t.Fatalf("a sort on a shared-base field must be servable, got %v", err)
	}
}

// managedSchema is guardSchema plus the three managed columns and a ParentID —
// the slots that surface under FIXED logical Go names (the three-name contract),
// which is how every consumer above infra addresses them.
func managedSchema(table string) *core.TableSchema {
	return core.NewTableSchema[*guardEnt](table).ID("id").Field("Name", "name").
		ParentID("owner_id").
		CreatedAt("created_at").UpdatedAt("updated_at").DeletedAt("deleted_at")
}

// TestServableManagedColumns_Passes — the managed slots and the ParentID
// projection resolve on BOTH backings or on neither, because both go through
// the one resolution surface (core.TableSchema.Resolve). They used to be two
// separate walks, so the SAME Request DTO, and the SAME ToCriteria, got
// different capabilities depending on which backing served the view.
//
// `OrderBy: CreatedAt desc` written by a Query's ToCriteria is the most ordinary
// default ordering there is; it worked on the Mongo projection and answered 400
// on the RelationalSource twin. Which field a consumer may address is the
// Request DTO's business (and the dev's, through ToCriteria) — never the
// backing's.
func TestServableManagedColumns_Passes(t *testing.T) {
	schema := managedSchema("gadgets")
	for _, field := range []string{"CreatedAt", "UpdatedAt", "DeletedAt", "ParentID"} {
		if _, err := toExpr(schema, map[string]any{field: "x"}); err != nil {
			t.Errorf("a filter on the managed field %q must be servable, got %v", field, err)
		}
		if err := applySort(schema, criteria.Where(nil), []queries.OrderByField{{Field: field, Desc: true}}); err != nil {
			t.Errorf("a sort on the managed field %q must be servable, got %v", field, err)
		}
	}
}

// TestUnsupportedUndeclaredManagedColumn_MapsTo400 is the other half of the
// rule: the logical name resolves only when the schema DECLARES that slot. A
// view with no DeletedAt has no archived state to address, so the name is as
// unknown as any other.
func TestUnsupportedUndeclaredManagedColumn_MapsTo400(t *testing.T) {
	_, err := toExpr(guardSchema("gadgets"), map[string]any{"CreatedAt": "x"})
	assertUnsupportedCapability400(t, err, "CreatedAt")
}

// TestUnsupportedUnknownField_MapsTo400 keeps the negative control: a flat field
// that belongs to NO schema (not root, not a sibling, not the base) is still a
// 400 — the relaxation admits 1:1 satellites, not arbitrary names.
func TestUnsupportedUnknownField_MapsTo400(t *testing.T) {
	_, err := toExpr(siblingSchema("gadgets"), map[string]any{"Nonexistent": "x"})
	assertUnsupportedCapability400(t, err, "Nonexistent")
}

// TestServableRootField_Passes is the positive control: a bona fide root column
// (Name) is NOT rejected — parity with the Mongo reader for root filters/sorts.
func TestServableRootField_Passes(t *testing.T) {
	if _, err := toExpr(guardSchema("gadgets"), map[string]any{"Name": "x"}); err != nil {
		t.Fatalf("a root-own field must be servable, got %v", err)
	}
	if err := applySort(guardSchema("gadgets"), criteria.Where(nil), []queries.OrderByField{{Field: "Name"}}); err != nil {
		t.Fatalf("a root-own sort field must be servable, got %v", err)
	}
}

// TestApplyProjection covers the ?fields= pruning the relational reader mirrors
// from the Mongo Find projection: inclusion keeps only the asked fields (id
// dropped unless asked), a dotted key prunes INTO the child array (leaf-level,
// matching Mongo — Fix #6), exclusion drops the listed leaves (nested included),
// and an empty projection is a no-op.
func TestApplyProjection(t *testing.T) {
	base := func() map[string]any {
		return map[string]any{
			"ID": "x", "_id": "x", "Code": "c", "Name": "n",
			"WidgetParts": []any{
				map[string]any{"ID": "p1", "Label": "L1", "Slot": "s1"},
				map[string]any{"ID": "p2", "Label": "L2", "Slot": "s2"},
			},
		}
	}

	// inclusion — only Code survives (id dropped, mirroring Mongo).
	d := base()
	applyProjection(d, queries.ProjectOnlyPaths("Code"))
	if len(d) != 1 || d["Code"] != "c" {
		t.Errorf("inclusion should keep only Code, got %v", d)
	}

	// inclusion of a bare segment keeps the whole child array untouched.
	d = base()
	applyProjection(d, queries.ProjectOnlyPaths("WidgetParts"))
	if len(d) != 1 {
		t.Fatalf("bare-segment inclusion should keep only WidgetParts, got %v", d)
	}
	if el := d["WidgetParts"].([]any)[0].(map[string]any); len(el) != 3 {
		t.Errorf("bare segment must keep each element WHOLE, got %v", el)
	}

	// inclusion of a DOTTED key prunes each element to the asked leaf (Fix #6).
	d = base()
	applyProjection(d, queries.ProjectOnlyPaths("WidgetParts.Label"))
	parts, ok := d["WidgetParts"].([]any)
	if !ok || len(d) != 1 || len(parts) != 2 {
		t.Fatalf("nested inclusion should keep the WidgetParts array, got %v", d)
	}
	for _, p := range parts {
		el := p.(map[string]any)
		if len(el) != 1 || el["Label"] == nil {
			t.Errorf("nested inclusion must prune each element to Label only, got %v", el)
		}
	}

	// exclusion drops only the listed top-level field.
	d = base()
	applyProjection(d, queries.Projection{Mode: queries.ProjectExcept, Paths: map[string]bool{"Name": true}})
	if _, ok := d["Name"]; ok {
		t.Errorf("exclusion should drop Name, got %v", d)
	}
	if _, ok := d["Code"]; !ok {
		t.Error("exclusion must keep the unlisted fields")
	}

	// nested exclusion drops the leaf from every element (Fix #6).
	d = base()
	applyProjection(d, queries.Projection{Mode: queries.ProjectExcept, Paths: map[string]bool{"WidgetParts.Slot": true}})
	for _, p := range d["WidgetParts"].([]any) {
		el := p.(map[string]any)
		if _, ok := el["Slot"]; ok {
			t.Errorf("nested exclusion must drop Slot from each element, got %v", el)
		}
		if el["Label"] == nil {
			t.Errorf("nested exclusion must keep the unlisted leaf, got %v", el)
		}
	}

	// empty projection is a no-op.
	d = base()
	applyProjection(d, queries.Projection{})
	if len(d) != 5 {
		t.Errorf("empty projection must not prune, got %v", d)
	}
}

// relViewWith builds a one-view relational reader over a fakeLoader with the
// given per-view ceiling, so the MaxLimit tests run without a database.
func relViewWith(ceiling int64, fake *fakeLoader) *ViewReader {
	vdef := query.RelationalView("v", fake)
	r := NewViewReader([]*query.RelationalViewDefinition{vdef})
	r.SetMaxLimitResolver(func(string) int64 { return ceiling })
	return r
}

// TestReadPage_MaxLimitRejected proves the relational reader honors the per-view
// MaxLimit cascade EXACTLY like the Mongo reader: a `?first=` over the ceiling is
// the canonical 400 LimitExceededNotification, rejected BEFORE any load runs.
func TestReadPage_MaxLimitRejected(t *testing.T) {
	fake := &fakeLoader{table: "gadgets"}
	r := relViewWith(5, fake)

	_, err := r.ReadPage(context.Background(), "v", queries.ReadCriteria{Limit: 100})
	if err == nil {
		t.Fatal("expected a LimitExceeded rejection for limit over the ceiling")
	}
	var carrier domain.NotificationCarrier
	if !errors.As(err, &carrier) {
		t.Fatalf("ceiling rejection must be a NotificationCarrier (→400), got %T", err)
	}
	msg := carrier.NotificationContexts()[0].Messages()[0]
	if got := reflect.TypeOf(msg.Notification).Name(); got != "LimitExceededNotification" {
		t.Errorf("notification = %q, want LimitExceededNotification", got)
	}
	if fake.findCalled {
		t.Error("the ceiling must reject BEFORE loading — the loader was called")
	}
}

// TestReadPage_BypassMaxLimit proves the trusted export path (BypassMaxLimit) is
// NOT rejected by the ceiling: the same over-ceiling limit loads through, letting
// the export wrapper run its own larger maxExportRows bound verbatim.
func TestReadPage_BypassMaxLimit(t *testing.T) {
	fake := &fakeLoader{table: "gadgets"}
	r := relViewWith(5, fake)

	if _, err := r.ReadPage(context.Background(), "v", queries.ReadCriteria{Limit: 100, BypassMaxLimit: true}); err != nil {
		t.Fatalf("BypassMaxLimit must skip the ceiling, got %v", err)
	}
	if !fake.findCalled {
		t.Error("BypassMaxLimit must let the load run")
	}
}
