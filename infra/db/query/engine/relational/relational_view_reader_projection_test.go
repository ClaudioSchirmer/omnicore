package relational

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/db/query"
)

// The field-selection gate: a `?fields=` path this read model cannot resolve is
// refused at the entry point with the SAME error the Mongo reader raises for the
// same input (core.UnresolvedFieldPathError → SchemaViolationNotification,
// SemanticSchema → 400). Before it existed the relational backing pruned the path
// away in silence and answered 200 with an empty document, which a consumer
// cannot tell apart from "I have no data".

// assertSchemaViolation400 checks the canonical unresolved-path refusal: a
// NotificationCarrier holding one SchemaViolationNotification, SemanticSchema
// (→400), naming the offending dotted Go path.
func assertSchemaViolation400(t *testing.T, err error, wantPath string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an unresolved-field-path error, got nil")
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
	if got := reflect.TypeOf(msg.Notification).Name(); got != "SchemaViolationNotification" {
		t.Errorf("notification = %q, want SchemaViolationNotification (what the Mongo reader raises)", got)
	}
	if got := msg.Notification.Semantic(); got != domain.SemanticSchema {
		t.Errorf("semantic = %v, want SemanticSchema (→400)", got)
	}
	if msg.FieldName != wantPath {
		t.Errorf("field = %q, want the offending path %q", msg.FieldName, wantPath)
	}
}

// projectionReader registers one relational view over the joinKit aggregate
// (root + a "Parts" child collection), with the declared join fields the test
// asks for.
func projectionReader(joins map[string][]string) *ViewReader {
	return NewViewReader([]*query.RelationalViewDefinition{
		query.RelationalView("kits", &joinFieldLoader{ent: joinKitEntity(), fields: joins}),
	})
}

func fieldsCriteria(paths ...string) queries.ReadCriteria {
	return queries.ReadCriteria{Limit: 10, Projection: queries.ProjectOnlyPaths(paths...)}
}

// A root path the schema does not own is refused — and refused BEFORE any IO,
// like every other capability refusal on this reader.
func TestValidateProjection_UnknownRootPathIs400(t *testing.T) {
	loader := &joinFieldLoader{ent: joinKitEntity()}
	r := NewViewReader([]*query.RelationalViewDefinition{query.RelationalView("kits", loader)})
	_, err := r.ReadPage(context.Background(), "kits", fieldsCriteria("Vendor"))
	assertSchemaViolation400(t, err, "Vendor")
}

// A path INTO a child segment that the child schema does not own is refused the
// same way: the segment resolves, the leaf does not.
func TestValidateProjection_UnknownNestedPathIs400(t *testing.T) {
	r := projectionReader(nil)
	_, err := r.ReadPage(context.Background(), "kits", fieldsCriteria("Parts.Supplier"))
	assertSchemaViolation400(t, err, "Parts.Supplier")
}

// What the view DOES carry passes: a root field, and a leaf inside the child
// collection.
func TestValidateProjection_ResolvablePathsAreServed(t *testing.T) {
	r := projectionReader(nil)
	page, err := r.ReadPage(context.Background(), "kits", fieldsCriteria("Name", "Parts.Label"))
	if err != nil {
		t.Fatalf("a resolvable selection must be served, got %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("want 1 item, got %d", len(page.Items))
	}
	if _, ok := page.Items[0]["Name"]; !ok {
		t.Errorf("the projected root field must survive the prune, got %v", page.Items[0])
	}
}

// A declared ROOT read join adds a Go field the TableSchema knows nothing about —
// the ViewNode cannot resolve it, and it is still served in the document, so the
// gate must admit it.
func TestValidateProjection_RootJoinFieldIsSelectable(t *testing.T) {
	r := projectionReader(map[string][]string{"kits": {"CustomerName"}})
	page, err := r.ReadPage(context.Background(), "kits", fieldsCriteria("CustomerName"))
	if err != nil {
		t.Fatalf("a declared root join field must be selectable, got %v", err)
	}
	if len(page.Items) != 1 || page.Items[0]["CustomerName"] != "ana" {
		t.Errorf("want the join field served, got %v", page.Items)
	}
}

// A join declared on a CHILD lands on that child's segment elements, so it is
// selectable under `<segment>.<field>` — load-only in a criteria, projectable
// here, because the document carries it.
func TestValidateProjection_ChildJoinFieldIsSelectable(t *testing.T) {
	r := projectionReader(map[string][]string{"kit_parts": {"CityName"}})
	if _, err := r.ReadPage(context.Background(), "kits", fieldsCriteria("Parts.CityName")); err != nil {
		t.Fatalf("a declared child join field must be selectable, got %v", err)
	}
}

// The join declaration is read as doc paths: a root join field is NOT reachable
// under a child segment, and a child's is not reachable at the root.
func TestValidateProjection_JoinFieldsAreNotReachableAtTheWrongLevel(t *testing.T) {
	r := projectionReader(map[string][]string{"kits": {"CustomerName"}, "kit_parts": {"CityName"}})
	_, err := r.ReadPage(context.Background(), "kits", fieldsCriteria("CityName"))
	assertSchemaViolation400(t, err, "CityName")
	_, err = r.ReadPage(context.Background(), "kits", fieldsCriteria("Parts.CustomerName"))
	assertSchemaViolation400(t, err, "Parts.CustomerName")
}

// An EXCLUSION naming a path this view has no concept of is as meaningless as an
// inclusion — the Mongo reader translates both modes, so both are checked.
func TestValidateProjection_ExclusionModeIsCheckedToo(t *testing.T) {
	r := projectionReader(nil)
	crit := queries.ReadCriteria{Limit: 10}
	crit.Projection.Drop("Vendor")
	_, err := r.ReadPage(context.Background(), "kits", crit)
	assertSchemaViolation400(t, err, "Vendor")
}

// Several unknown paths in one request always name the same first offender: the
// set is visited in sorted order, so the answer does not depend on map iteration.
func TestValidateProjection_NamesTheFirstOffenderDeterministically(t *testing.T) {
	r := projectionReader(nil)
	for i := 0; i < 8; i++ {
		_, err := r.ReadPage(context.Background(), "kits", fieldsCriteria("Zeta", "Alpha", "Mike"))
		assertSchemaViolation400(t, err, "Alpha")
	}
}

// The by-id read carries the same gate: one vocabulary, both entry points.
func TestValidateProjection_ReadByIDRefusesToo(t *testing.T) {
	r := projectionReader(nil)
	_, _, err := r.ReadByID(context.Background(), "kits",
		"11111111-1111-1111-1111-111111111111", fieldsCriteria("Vendor"))
	assertSchemaViolation400(t, err, "Vendor")
}

// A read that names no fields is untouched: the whole document, no gate.
func TestValidateProjection_NoSelectionIsANoOp(t *testing.T) {
	r := projectionReader(nil)
	if _, err := r.ReadPage(context.Background(), "kits", queries.ReadCriteria{Limit: 10}); err != nil {
		t.Fatalf("a read with no ?fields= must not be gated, got %v", err)
	}
}

// joinProjectionPaths reads the declaration, not the loaded entity: a table no
// child of this schema owns contributes nothing.
func TestJoinProjectionPaths_IgnoresAnUnknownTable(t *testing.T) {
	got := joinProjectionPaths(joinKitSchema(), map[string][]string{"strangers": {"Whatever"}})
	if got != nil {
		t.Errorf("got %v, want nil — a join on a foreign table adds no doc path", got)
	}
	if joinProjectionPaths(joinKitSchema(), nil) != nil {
		t.Error("no declared joins must yield no paths")
	}
}

// baseKid is a shared base's NATIVE child (a collection belonging to the shared
// identity, not to any role) — the second set joinProjectionPaths walks, mirroring
// applyJoinFields so the two passes read the same segments.
type baseKid struct {
	domain.Managed
	Tag string
}

func (k baseKid) BuildRules(string, domain.Service, *domain.Rules) {}
func (k baseKid) CollectionName() string                           { return "Kids" }
func (k baseKid) IsSameBusinessIdentity(o domain.AggregateValueObject) bool {
	x, ok := o.(baseKid)
	return ok && x.Tag == k.Tag
}

// A shared base's native child is walked like the role's own, under the same
// `<segment>.<field>` shape — the symmetry applyJoinFields keeps, asserted rather
// than trusted. In practice the entry stays empty (a join on a base child is
// refused at declaration: it belongs to the base's own repository), so this is the
// belt-and-braces half — what matters is that the two passes see the same segments.
func TestJoinProjectionPaths_BaseChildSegmentIsWalked(t *testing.T) {
	base := core.NewSharedBaseSchema("holders_base").ID("id").Revision("revision").
		Field("DisplayName", "display_name").NaturalID("display_name").
		Child(core.NewTableSchema[baseKid]("holder_kids").ID("id").ParentID("holder_id").Field("Tag", "tag"))
	role := core.NewTableSchema[*baseEnt]("holders").ID("id").
		SharedBase(base, "id").Field("HolderName", "holder_name")

	got := joinProjectionPaths(role, map[string][]string{
		"holders":     {"CustomerName"},
		"holder_kids": {"CityName"},
	})
	want := map[string]bool{"CustomerName": true, "Kids.CityName": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v — the base child's segment is walked like a native one", got, want)
	}
}
