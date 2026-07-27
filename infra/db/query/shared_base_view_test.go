package query

import (
	"context"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// White-box coverage for the SharedBaseView kind: the builder guards, the
// base-rooted composition (active-first role selection under separate-FK
// multiplicity), the ViewNode role segments (translation + strip), the rebuild
// hash roles block, the export role branches and the composed-column allowlist.

// --- fixtures ---------------------------------------------------------------
//
// Two roles over one person identity, mirroring the reference consumer:
// sbvUser links shared-PK (fk == PK), sbvEmployee links separate-FK and owns a
// child collection — so one fixture exercises both link models.

type sbvUser struct {
	Name              string
	Document          string
	UserName          string
	EmailNotification *bool
}

type sbvEmployee struct {
	Name           string
	Document       string
	EmployeeNumber string
}

type sbvDependent struct{ Name string }

type sbvAddr struct{ Street string }

func sbvBase() *core.TableSchema {
	return core.NewSharedBaseSchema("sbv_persons").Revision("revision").
		PK("id").
		Field("Document", "document").
		Field("Name", "name").
		NaturalKey("document").
		SoftDelete("deleted_at").
		Child(core.NewTableSchema[sbvAddr]("sbv_addresses").
			PK("id").FK("person_id").Field("Street", "street").SoftDelete("deleted_at"))
}

func sbvUserSchema() *core.TableSchema {
	return core.NewTableSchema[*sbvUser]("sbv_users").
		PK("id").
		Field("UserName", "user_name").
		SoftDelete("deleted_at").
		Sibling(core.NewSiblingSchema[*sbvUser]("sbv_user_configs").
						Field("EmailNotification", "email_notification")).
		SharedBase(sbvBase(), "id") // shared-PK model
}

func sbvEmployeeSchema() *core.TableSchema {
	return core.NewTableSchema[*sbvEmployee]("sbv_employees").
		PK("id").
		Field("EmployeeNumber", "employee_number").
		SoftDelete("deleted_at").
		Child(core.NewTableSchema[sbvDependent]("sbv_dependents").
							PK("id").FK("employee_id").Field("Name", "dep_name").SoftDelete("deleted_at")).
		SharedBase(sbvBase(), "person_id") // separate-FK model
}

func sbvView() *ViewDefinition {
	return SharedBaseView("sbv_persons_view").Schema(sbvBase()).
		Role(sbvUserSchema()).
		Role(sbvEmployeeSchema()).
		Version(1)
}

var (
	sbvAddrSeg = domain.PluralizeWord("sbvAddr")      // base-child segment at the root
	sbvDepSeg  = domain.PluralizeWord("sbvDependent") // employee-child segment inside the role
)

// --- builder ----------------------------------------------------------------

func TestSharedBaseView_BuilderAccessors(t *testing.T) {
	v := sbvView()
	if !v.IsSharedBaseView() {
		t.Error("IsSharedBaseView must be true")
	}
	if View("plain").IsSharedBaseView() {
		t.Error("a regular View must not report IsSharedBaseView")
	}
	if v.RootTable() != "sbv_persons" {
		t.Errorf("RootTable = %q, want the base table", v.RootTable())
	}
	roles := v.RoleViews()
	if len(roles) != 2 {
		t.Fatalf("RoleViews = %d, want 2", len(roles))
	}
	if roles[0].Segment != "sbvUser" || roles[0].FKColumn != "id" {
		t.Errorf("role[0] = %+v, want segment sbvUser fk id (shared-PK)", roles[0])
	}
	if roles[1].Segment != "sbvEmployee" || roles[1].FKColumn != "person_id" {
		t.Errorf("role[1] = %+v, want segment sbvEmployee fk person_id", roles[1])
	}
}

func TestSharedBaseView_BuilderPanics(t *testing.T) {
	assertPanics(t, "Role before Schema", func() {
		SharedBaseView("x").Role(sbvUserSchema())
	})
	assertPanics(t, "Role on a regular View", func() {
		View("plain").Role(sbvUserSchema())
	})
	assertPanics(t, "nil role", func() {
		SharedBaseView("x").Schema(sbvBase()).Role(nil)
	})
	assertPanics(t, "type-less role", func() {
		SharedBaseView("x").Schema(sbvBase()).Role(core.NewExternalSchema("ext").PK("id"))
	})
	assertPanics(t, "role without SharedBase", func() {
		SharedBaseView("x").Schema(sbvBase()).Role(
			core.NewTableSchema[*sbvUser]("plain_users").PK("id").Field("UserName", "user_name"))
	})
	assertPanics(t, "role of another base table", func() {
		otherBase := core.NewSharedBaseSchema("other_persons").Revision("revision").PK("id").
			Field("Document", "document").Field("Name", "name").NaturalKey("document")
		role := core.NewTableSchema[*sbvUser]("sbv_users").PK("id").
			Field("UserName", "user_name").SharedBase(otherBase, "id")
		SharedBaseView("x").Schema(sbvBase()).Role(role)
	})
	assertPanics(t, "divergent base declaration", func() {
		divergent := core.NewSharedBaseSchema("sbv_persons").Revision("revision").PK("id").
			Field("Document", "document").NaturalKey("document") // missing Name + SoftDelete
		role := core.NewTableSchema[*sbvUser]("sbv_users").PK("id").
			Field("UserName", "user_name").SharedBase(divergent, "id")
		SharedBaseView("x").Schema(sbvBase()).Role(role)
	})
	assertPanics(t, "duplicate role segment", func() {
		SharedBaseView("x").Schema(sbvBase()).Role(sbvUserSchema()).Role(sbvUserSchema())
	})
}

func TestValidateViewSchemas_SharedBaseView(t *testing.T) {
	// No roles → boot error.
	err := ValidateViewSchemas([]*ViewDefinition{SharedBaseView("empty").Schema(sbvBase()).Version(1)})
	if err == nil || !strings.Contains(err.Error(), "declares no .Role") {
		t.Fatalf("a role-less SharedBaseView must be rejected, got %v", err)
	}
	// No .Schema(...) → boot error (the schema-mandatory gate, shared with a regular View).
	err = ValidateViewSchemas([]*ViewDefinition{SharedBaseView("noschema").Version(1)})
	if err == nil || !strings.Contains(err.Error(), "no root .Schema") {
		t.Fatalf("a schema-less SharedBaseView must be rejected, got %v", err)
	}
	// A non-shared-base .Schema(...) → boot error (the base-kind gate that used to
	// panic in the constructor).
	err = ValidateViewSchemas([]*ViewDefinition{SharedBaseView("wrongkind").Schema(sbvUserSchema()).Version(1)})
	if err == nil || !strings.Contains(err.Error(), "must be a core.NewSharedBaseSchema") {
		t.Fatalf("a non-shared-base SharedBaseView schema must be rejected, got %v", err)
	}
	// A well-formed two-role view passes.
	if err := ValidateViewSchemas([]*ViewDefinition{sbvView()}); err != nil {
		t.Fatalf("valid SharedBaseView rejected: %v", err)
	}
	// An embed claiming a role's segment collides.
	v := sbvView().EmbedMany(JoinUpstream(core.NewExternalSchema("ext_coll").PK("id"), "sbvUser", "mirror")).On("person_id")
	err = ValidateViewSchemas([]*ViewDefinition{v})
	if err == nil || !strings.Contains(err.Error(), `segment "sbvUser"`) {
		t.Fatalf("an embed colliding with a role segment must be rejected, got %v", err)
	}
}

// TestValidateViewSchemas_PlainViewRejectsSharedBaseSchema proves the mirror of
// the base-kind gate: a regular query.View may NOT be rooted at a
// core.NewSharedBaseSchema — that identity is a SharedBaseView's job. The two
// constructors are type-exclusive in BOTH directions.
func TestValidateViewSchemas_PlainViewRejectsSharedBaseSchema(t *testing.T) {
	// A plain View rooted at a shared-base schema → boot error.
	err := ValidateViewSchemas([]*ViewDefinition{View("plain").Schema(sbvBase()).Version(1)})
	if err == nil || !strings.Contains(err.Error(), "is a core.NewSharedBaseSchema") {
		t.Fatalf("a plain View rooted at a shared-base schema must be rejected, got %v", err)
	}
	// The positive control: a plain View rooted at a regular TableSchema passes.
	ok := View("plain").Schema(core.NewTableSchema[embedFixture]("users").PK("id").SoftDelete("deleted_at")).Version(1)
	if err := ValidateViewSchemas([]*ViewDefinition{ok}); err != nil {
		t.Fatalf("a plain View over a regular TableSchema must pass, got %v", err)
	}
}

// --- composer ----------------------------------------------------------------

// sbvComposerEngine scripts the relational reads of a two-role person and
// records every SQL + args pair for shape assertions.
func sbvComposerEngine(t *testing.T, rows map[string][]map[string]any, calls *[]string, args *[][]any) *fakeEngine {
	t.Helper()
	return composerEngine(func(sql string, a []any) ([]map[string]any, error) {
		*calls = append(*calls, sql)
		*args = append(*args, a)
		for marker, result := range rows {
			if strings.Contains(sql, marker) {
				return result, nil
			}
		}
		return nil, nil
	})
}

func TestComposeBaseRooted_TwoRoles(t *testing.T) {
	var calls []string
	var args [][]any
	eng := sbvComposerEngine(t, map[string][]map[string]any{
		"FROM sbv_persons":      mapsFromColsData([]string{"id", "document", "name"}, [][]any{{"p1", "D1", "Ana"}}),
		"FROM sbv_addresses":    mapsFromColsData([]string{"id", "person_id", "street"}, [][]any{{"a1", "p1", "Main St"}, {"a2", "p1", "2nd Ave"}}),
		"FROM sbv_users":        mapsFromColsData([]string{"id", "user_name"}, [][]any{{"p1", "ana"}}),
		"FROM sbv_user_configs": mapsFromColsData([]string{"id", "email_notification"}, [][]any{{"p1", true}}),
		"FROM sbv_employees":    mapsFromColsData([]string{"id", "person_id", "employee_number"}, [][]any{{"e9", "p1", "M1"}}),
		"FROM sbv_dependents":   mapsFromColsData([]string{"id", "employee_id", "dep_name"}, [][]any{{"d1", "e9", "Rita"}}),
	}, &calls, &args)

	doc, err := NewComposer(eng).Compose(context.Background(), sbvView(), "p1")
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if doc["name"] != "Ana" || doc["document"] != "D1" {
		t.Errorf("base fields must land flat at the root, got %v", doc)
	}
	if addrs, ok := doc[sbvAddrSeg].([]Document); !ok || len(addrs) != 2 {
		t.Errorf("base-children must nest at the ROOT under %q, got %#v", sbvAddrSeg, doc[sbvAddrSeg])
	}
	user, ok := doc["sbvUser"].(Document)
	if !ok {
		t.Fatalf("user role segment missing, got %#v", doc["sbvUser"])
	}
	if user["user_name"] != "ana" {
		t.Errorf("role-private field must land inside the segment, got %v", user)
	}
	if user["email_notification"] != true {
		t.Errorf("role sibling must merge FLAT inside the segment, got %v", user)
	}
	emp, ok := doc["sbvEmployee"].(Document)
	if !ok {
		t.Fatalf("employee role segment missing, got %#v", doc["sbvEmployee"])
	}
	deps, ok := emp[sbvDepSeg].([]Document)
	if !ok || len(deps) != 1 || deps[0]["dep_name"] != "Rita" {
		t.Errorf("role children must nest INSIDE the segment, got %#v", emp[sbvDepSeg])
	}
	// The dependents fetch must key on the CHOSEN ROLE ROW's PK (e9), never the
	// base id — under separate-FK they differ.
	found := false
	for i, sql := range calls {
		if strings.Contains(sql, "FROM sbv_dependents") {
			found = true
			if len(args[i]) != 1 || args[i][0] != "e9" {
				t.Errorf("dependents must be fetched by the role row PK e9, got %v", args[i])
			}
		}
	}
	if !found {
		t.Error("no dependents fetch recorded")
	}
	// No role sub-document may leak base flat fields.
	if _, has := user["name"]; has {
		t.Errorf("base fields must not appear inside a role segment, got %v", user)
	}
}

func TestComposeBaseRooted_AbsentRoleIsExplicitNull(t *testing.T) {
	var calls []string
	var args [][]any
	eng := sbvComposerEngine(t, map[string][]map[string]any{
		"FROM sbv_persons": mapsFromColsData([]string{"id", "document", "name"}, [][]any{{"p1", "D1", "Ana"}}),
		"FROM sbv_users":   mapsFromColsData([]string{"id", "user_name"}, [][]any{{"p1", "ana"}}),
		// sbv_employees: no rows at all (active probe AND remnant probe empty).
	}, &calls, &args)

	doc, err := NewComposer(eng).Compose(context.Background(), sbvView(), "p1")
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	seg, present := doc["sbvEmployee"]
	if !present {
		t.Fatal("an absent role must write an EXPLICIT segment key (the store upserts via $set)")
	}
	if seg != nil {
		t.Fatalf("absent role segment must be nil, got %#v", seg)
	}
}

func TestComposeBaseRooted_RemnantWhenNoActive(t *testing.T) {
	var calls []string
	var args [][]any
	eng := sbvComposerEngine(t, map[string][]map[string]any{
		"FROM sbv_persons": mapsFromColsData([]string{"id", "document", "name"}, [][]any{{"p1", "D1", "Ana"}}),
		"FROM sbv_users":   mapsFromColsData([]string{"id", "user_name"}, [][]any{{"p1", "ana"}}),
		// Employees: the active probe (deleted_at IS NULL) misses; the remnant
		// probe (IS NOT NULL ORDER BY ... DESC) hits the latest archived row.
		"IS NOT NULL ORDER BY": mapsFromColsData([]string{"id", "person_id", "employee_number", "deleted_at"},
			[][]any{{"e_old", "p1", "M0", "2026-01-01"}}),
	}, &calls, &args)

	doc, err := NewComposer(eng).Compose(context.Background(), sbvView(), "p1")
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	emp, ok := doc["sbvEmployee"].(Document)
	if !ok {
		t.Fatalf("archived remnant must compose (keep mode), got %#v", doc["sbvEmployee"])
	}
	if emp["deleted_at"] == nil {
		t.Error("the remnant segment must carry its soft-delete timestamp (the SQL mirror)")
	}
	// The remnant probe must be deterministic: newest archived first.
	found := false
	for _, sql := range calls {
		if strings.Contains(sql, "IS NOT NULL ORDER BY") {
			found = true
			if !strings.Contains(sql, "ORDER BY deleted_at DESC") {
				t.Errorf("remnant pick must order by deleted_at DESC, got %q", sql)
			}
		}
	}
	if !found {
		t.Error("no remnant probe recorded")
	}
}

func TestComposeBaseRooted_ActiveWinsWithoutRemnantProbe(t *testing.T) {
	var calls []string
	var args [][]any
	eng := sbvComposerEngine(t, map[string][]map[string]any{
		"FROM sbv_persons":   mapsFromColsData([]string{"id", "document", "name"}, [][]any{{"p1", "D1", "Ana"}}),
		"FROM sbv_users":     mapsFromColsData([]string{"id", "user_name"}, [][]any{{"p1", "ana"}}),
		"FROM sbv_employees": mapsFromColsData([]string{"id", "person_id", "employee_number"}, [][]any{{"e9", "p1", "M1"}}),
	}, &calls, &args)

	if _, err := NewComposer(eng).Compose(context.Background(), sbvView(), "p1"); err != nil {
		t.Fatalf("Compose: %v", err)
	}
	for _, sql := range calls {
		if strings.Contains(sql, "IS NOT NULL ORDER BY") {
			t.Errorf("an active role must short-circuit the remnant probe, got %q", sql)
		}
	}
}

func TestComposeBaseRooted_DeleteOnArchiveSkipsRemnant(t *testing.T) {
	var calls []string
	var args [][]any
	eng := sbvComposerEngine(t, map[string][]map[string]any{
		"FROM sbv_persons": mapsFromColsData([]string{"id", "document", "name"}, [][]any{{"p1", "D1", "Ana"}}),
		"FROM sbv_users":   mapsFromColsData([]string{"id", "user_name"}, [][]any{{"p1", "ana"}}),
	}, &calls, &args)

	view := SharedBaseView("hot").Schema(sbvBase()).Role(sbvUserSchema()).Role(sbvEmployeeSchema()).Version(1).DeleteOnArchive()
	doc, err := NewComposer(eng).Compose(context.Background(), view, "p1")
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if seg, present := doc["sbvEmployee"]; !present || seg != nil {
		t.Fatalf("under DeleteOnArchive an archived remnant must not compose — explicit nil segment, got %#v", seg)
	}
	for _, sql := range calls {
		if strings.Contains(sql, "IS NOT NULL ORDER BY") {
			t.Errorf("DeleteOnArchive must skip the remnant probe entirely, got %q", sql)
		}
	}
}

func TestComposeAll_BaseRooted(t *testing.T) {
	var calls []string
	var args [][]any
	eng := sbvComposerEngine(t, map[string][]map[string]any{
		"FROM sbv_persons": mapsFromColsData([]string{"id", "document", "name"},
			[][]any{{"p1", "D1", "Ana"}, {"p2", "D2", "Bea"}}),
		"FROM sbv_users": mapsFromColsData([]string{"id", "user_name"}, [][]any{{"p1", "ana"}}),
	}, &calls, &args)

	docs, err := NewComposer(eng).ComposeAll(context.Background(), sbvView())
	if err != nil {
		t.Fatalf("ComposeAll: %v", err)
	}
	if len(docs) != 2 {
		t.Fatalf("expected 2 person docs, got %d", len(docs))
	}
	for _, d := range docs {
		if _, present := d["sbvEmployee"]; !present {
			t.Errorf("every person doc must carry every role segment (nil when absent), got %v", d)
		}
	}
}

// --- ViewNode ----------------------------------------------------------------

func TestSharedBaseViewNode_ColumnPaths(t *testing.T) {
	n := sbvView().BuildViewNode()
	cases := []struct {
		goPath []string
		want   string
	}{
		{[]string{"Name"}, "name"},
		{[]string{"Document"}, "document"},
		{[]string{sbvAddrSeg, "Street"}, sbvAddrSeg + ".street"},
		{[]string{"sbvUser", "UserName"}, "sbvUser.user_name"},
		{[]string{"sbvUser", "EmailNotification"}, "sbvUser.email_notification"}, // sibling, flat in the segment
		{[]string{"sbvEmployee", "EmployeeNumber"}, "sbvEmployee.employee_number"},
		{[]string{"sbvEmployee", sbvDepSeg, "Name"}, "sbvEmployee." + sbvDepSeg + ".dep_name"},
	}
	for _, tc := range cases {
		got, ok := n.ColumnPath(tc.goPath)
		if !ok || strings.Join(got, ".") != tc.want {
			t.Errorf("ColumnPath(%v) = %v ok=%v, want %q", tc.goPath, got, ok, tc.want)
		}
	}
	if _, ok := n.ColumnPath([]string{"sbvUser", "Nope"}); ok {
		t.Error("unknown role sub-field must not translate")
	}
}

func TestSharedBaseViewNode_ToGoDoc(t *testing.T) {
	n := sbvView().BuildViewNode()
	doc := map[string]any{
		"_id":      "p1",
		"name":     "Ana",
		"document": "D1",
		"sbvUser":  map[string]any{"id": "p1", "user_name": "ana", "email_notification": true},
		"sbvEmployee": map[string]any{
			"id": "e9", "employee_number": "M1",
			sbvDepSeg: []any{map[string]any{"id": "d1", "dep_name": "Rita"}},
		},
	}
	out := n.ToGoDoc(doc)
	if out["Name"] != "Ana" {
		t.Errorf("root base field must translate, got %v", out)
	}
	user, ok := out["sbvUser"].(map[string]any)
	if !ok || user["UserName"] != "ana" || user["EmailNotification"] != true {
		t.Errorf("role segment must translate recursively, got %#v", out["sbvUser"])
	}
	emp, _ := out["sbvEmployee"].(map[string]any)
	deps, ok := emp[sbvDepSeg].([]any)
	if !ok || len(deps) != 1 {
		t.Fatalf("role children must survive translation, got %#v", emp)
	}
	if dep, _ := deps[0].(map[string]any); dep["Name"] != "Rita" {
		t.Errorf("role-child columns must translate (dep_name→Name), got %#v", deps[0])
	}
}

func TestSharedBaseViewNode_ToGoDoc_NullSegmentSurvives(t *testing.T) {
	n := sbvView().BuildViewNode()
	out := n.ToGoDoc(map[string]any{"_id": "p1", "name": "Ana", "sbvEmployee": nil})
	if v, present := out["sbvEmployee"]; !present || v != nil {
		t.Errorf("an explicit null role segment must survive translation, got %#v (present=%v)", v, present)
	}
}

func TestSharedBaseViewNode_StripArchivedRole(t *testing.T) {
	n := sbvView().BuildViewNode()
	doc := map[string]any{
		"name":        "Ana",
		"sbvUser":     map[string]any{"id": "p1", "user_name": "ana", "deleted_at": "2026-01-01"},
		"sbvEmployee": map[string]any{"id": "e9", "employee_number": "M1", "deleted_at": nil},
	}
	n.StripArchivedChildren(doc)
	if doc["sbvUser"] != nil {
		t.Errorf("an ARCHIVED role segment must be hidden on a default read, got %#v", doc["sbvUser"])
	}
	if emp, ok := doc["sbvEmployee"].(map[string]any); !ok || emp["employee_number"] != "M1" {
		t.Errorf("an active role segment must survive the strip, got %#v", doc["sbvEmployee"])
	}
}

func TestSharedBaseViewNode_StripRecursesIntoRoleChildren(t *testing.T) {
	n := sbvView().BuildViewNode()
	doc := map[string]any{
		"sbvEmployee": map[string]any{
			"id": "e9",
			sbvDepSeg: []any{
				map[string]any{"id": "d1", "dep_name": "Rita"},
				map[string]any{"id": "d2", "dep_name": "Old", "deleted_at": "2026-01-01"},
			},
		},
	}
	n.StripArchivedChildren(doc)
	emp, _ := doc["sbvEmployee"].(map[string]any)
	deps, _ := emp[sbvDepSeg].([]any)
	if len(deps) != 1 {
		t.Fatalf("archived role-child entries must strip, got %#v", deps)
	}
}

func TestSharedBaseViewNode_ChildSoftDeletePaths(t *testing.T) {
	paths := sbvView().BuildViewNode().ChildSoftDeletePaths()
	want := map[string]string{
		sbvAddrSeg:                 "deleted_at",
		"sbvUser":                  "deleted_at",
		"sbvEmployee":              "deleted_at",
		"sbvEmployee." + sbvDepSeg: "deleted_at",
	}
	for k, v := range want {
		if paths[k] != v {
			t.Errorf("ChildSoftDeletePaths[%q] = %q, want %q (all: %v)", k, paths[k], v, paths)
		}
	}
}

// --- rebuild hash --------------------------------------------------------------

func TestSharedBaseView_RebuildHashMovesWithRoles(t *testing.T) {
	none := SharedBaseView("v").Schema(sbvBase()).Version(1)
	one := SharedBaseView("v").Schema(sbvBase()).Role(sbvUserSchema()).Version(1)
	two := SharedBaseView("v").Schema(sbvBase()).Role(sbvUserSchema()).Role(sbvEmployeeSchema()).Version(1)
	if none.RebuildHash() == one.RebuildHash() || one.RebuildHash() == two.RebuildHash() {
		t.Error("adding a role must move the RebuildHash (forgot-to-bump guard)")
	}
	// Order-independent: declaring roles in any order yields the same hash.
	twoSwapped := SharedBaseView("v").Schema(sbvBase()).Role(sbvEmployeeSchema()).Role(sbvUserSchema()).Version(1)
	if two.RebuildHash() != twoSwapped.RebuildHash() {
		t.Error("role declaration order must not change the RebuildHash")
	}
}

// --- export ---------------------------------------------------------------------

func TestSharedBaseView_ExportPlanRoleBranches(t *testing.T) {
	plan := sbvView().ExportPlan()
	rootCols := map[string]bool{}
	for _, c := range plan.Root.Columns {
		rootCols[c.GoField] = true
	}
	if !rootCols["Name"] || !rootCols["Document"] {
		t.Errorf("root must carry the base business columns, got %v", plan.Root.Columns)
	}
	byGoSeg := map[string]int{}
	for i, ch := range plan.Root.Children {
		byGoSeg[ch.GoSegment] = i
	}
	if _, ok := byGoSeg[sbvAddrSeg]; !ok {
		t.Fatalf("base-children must branch at the root, got %v", byGoSeg)
	}
	ui, ok := byGoSeg["sbvUser"]
	if !ok {
		t.Fatalf("user role must branch at the root, got %v", byGoSeg)
	}
	userNode := plan.Root.Children[ui]
	if userNode.WireSegment != "sbvUser" && userNode.WireSegment != domain.ToLowerCamel("sbvUser") {
		t.Errorf("role wire segment = %q", userNode.WireSegment)
	}
	userCols := map[string]bool{}
	for _, c := range userNode.Columns {
		userCols[c.GoField] = true
	}
	if !userCols["UserName"] || !userCols["EmailNotification"] {
		t.Errorf("role branch must carry role fields + sibling fields, got %v", userNode.Columns)
	}
	if userCols["Name"] || userCols["Document"] {
		t.Errorf("role branch must NOT repeat the base flat columns, got %v", userNode.Columns)
	}
	ei := byGoSeg["sbvEmployee"]
	empNode := plan.Root.Children[ei]
	if len(empNode.Children) != 1 || empNode.Children[0].GoSegment != sbvDepSeg {
		t.Errorf("employee branch must nest its own child collection, got %+v", empNode.Children)
	}
}

// --- composed-column allowlist ---------------------------------------------------

func TestSharedBaseView_ComposedColumnSet(t *testing.T) {
	set := sbvView().composedColumnSet()
	for _, want := range []string{
		"name", "document", "deleted_at",
		sbvAddrSeg, sbvAddrSeg + ".street",
		"sbvUser", "sbvUser.user_name", "sbvUser.email_notification", "sbvUser.deleted_at",
		"sbvEmployee.employee_number",
		"sbvEmployee." + sbvDepSeg + ".dep_name",
	} {
		if _, ok := set[want]; !ok {
			t.Errorf("composedColumnSet missing %q", want)
		}
	}
	// The base's flat columns never live inside a role segment.
	if _, ok := set["sbvUser.name"]; ok {
		t.Error("base flat columns must not be addressable inside a role segment")
	}
}

// The latent own-children gap fix applies to REGULAR views too: an index on an
// own-child path must validate.
func TestComposedColumnSet_OwnChildrenWalk(t *testing.T) {
	schema := core.NewTableSchema[*sbvEmployee]("plain_emps").
		PK("id").
		Field("EmployeeNumber", "employee_number").
		Child(core.NewTableSchema[sbvDependent]("plain_deps").
			PK("id").FK("emp_id").Field("Name", "dep_name"))
	set := View("plain").Version(1).Schema(schema).composedColumnSet()
	if _, ok := set[sbvDepSeg+".dep_name"]; !ok {
		t.Errorf("own-child columns must be addressable (%s.dep_name), got %v", sbvDepSeg, set)
	}
}

// --- coverage: no-SoftDelete role + error propagation -------------------------

// sbvBadge is a role WITHOUT SoftDelete: hard delete is delete, so a single
// FK fetch decides the segment.
type sbvBadge struct {
	Name     string
	Document string
	Code     string
}

func sbvBadgeSchema() *core.TableSchema {
	return core.NewTableSchema[*sbvBadge]("sbv_badges").
		PK("id").
		Field("Code", "code").
		SharedBase(sbvBase(), "person_id")
}

func TestComposeBaseRooted_RoleWithoutSoftDelete(t *testing.T) {
	var calls []string
	var args [][]any
	eng := sbvComposerEngine(t, map[string][]map[string]any{
		"FROM sbv_persons": mapsFromColsData([]string{"id", "document", "name"}, [][]any{{"p1", "D1", "Ana"}}),
		"FROM sbv_badges":  mapsFromColsData([]string{"id", "person_id", "code"}, [][]any{{"b1", "p1", "C7"}}),
	}, &calls, &args)

	view := SharedBaseView("v").Schema(sbvBase()).Role(sbvBadgeSchema()).Version(1)
	doc, err := NewComposer(eng).Compose(context.Background(), view, "p1")
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	badge, ok := doc["sbvBadge"].(Document)
	if !ok || badge["code"] != "C7" {
		t.Fatalf("a no-SoftDelete role must compose from the single FK fetch, got %#v", doc["sbvBadge"])
	}
	for _, sql := range calls {
		if strings.Contains(sql, "FROM sbv_badges") && strings.Contains(sql, "deleted_at") {
			t.Errorf("a no-SoftDelete role fetch must carry no archive predicate, got %q", sql)
		}
	}
}

// sbvErrEngine fails any SQL containing the marker, letting each merge step's
// error propagation be exercised in isolation.
func sbvErrEngine(marker string) *fakeEngine {
	base := map[string][]map[string]any{
		"FROM sbv_persons":    mapsFromColsData([]string{"id", "document", "name"}, [][]any{{"p1", "D1", "Ana"}}),
		"FROM sbv_addresses":  mapsFromColsData([]string{"id", "person_id", "street"}, [][]any{{"a1", "p1", "Main St"}}),
		"FROM sbv_users":      mapsFromColsData([]string{"id", "user_name"}, [][]any{{"p1", "ana"}}),
		"FROM sbv_employees":  mapsFromColsData([]string{"id", "person_id", "employee_number"}, [][]any{{"e9", "p1", "M1"}}),
		"FROM sbv_dependents": mapsFromColsData([]string{"id", "employee_id", "dep_name"}, [][]any{{"d1", "e9", "Rita"}}),
	}
	return composerEngine(func(sql string, a []any) ([]map[string]any, error) {
		if strings.Contains(sql, marker) {
			return nil, errFake
		}
		for m, result := range base {
			if strings.Contains(sql, m) {
				return result, nil
			}
		}
		return nil, nil
	})
}

func TestComposeBaseRooted_ErrorPropagation(t *testing.T) {
	for _, marker := range []string{
		"FROM sbv_addresses",    // base-children merge
		"FROM sbv_users",        // role row fetch
		"FROM sbv_user_configs", // role sibling merge
		"FROM sbv_dependents",   // role children merge
	} {
		if _, err := NewComposer(sbvErrEngine(marker)).Compose(context.Background(), sbvView(), "p1"); err == nil {
			t.Errorf("an engine failure on %q must propagate", marker)
		}
	}
}

func TestSharedBaseViewNode_NoSoftDeleteRole(t *testing.T) {
	view := SharedBaseView("v").Schema(sbvBase()).Role(sbvBadgeSchema()).Version(1)
	n := view.BuildViewNode()
	// ChildSoftDeletePaths: the badge role has no soft-delete, so no path for it
	// (the base-child path stays).
	paths := n.ChildSoftDeletePaths()
	if _, has := paths["sbvBadge"]; has {
		t.Errorf("a no-SoftDelete role must contribute no strip path, got %v", paths)
	}
	if paths[sbvAddrSeg] != "deleted_at" {
		t.Errorf("base-child strip path must stay, got %v", paths)
	}
	// StripArchivedChildren: the badge segment survives untouched (no lifecycle),
	// and its sub-tree still recurses (no child collections here — no panic).
	doc := map[string]any{"sbvBadge": map[string]any{"id": "b1", "code": "C7"}}
	n.StripArchivedChildren(doc)
	if badge, ok := doc["sbvBadge"].(map[string]any); !ok || badge["code"] != "C7" {
		t.Errorf("a no-SoftDelete role segment must survive the strip, got %#v", doc["sbvBadge"])
	}
}

// Defensive branches: the schema-less node, nil doc, explicit embeds (not
// lifecycle-carrying) and a soft-delete-less child are all no-ops for the
// strip/paths pair.
func TestViewNode_StripAndPathsDefensiveBranches(t *testing.T) {
	empty := &ViewNode{}
	if paths := empty.ChildSoftDeletePaths(); paths != nil {
		t.Errorf("schema-less node must yield nil paths, got %v", paths)
	}
	empty.StripArchivedChildren(map[string]any{"x": 1})  // no panic
	sbvView().BuildViewNode().StripArchivedChildren(nil) // nil doc no-op

	// A view with an explicit external embed (no lifecycle) and a
	// soft-delete-less own child: neither contributes a strip path, and the
	// strip leaves both segments untouched.
	schema := core.NewTableSchema[*sbvEmployee]("se_emps").
		PK("id").
		Field("EmployeeNumber", "employee_number").
		Child(core.NewTableSchema[sbvDependent]("se_deps").
			PK("id").FK("emp_id").Field("Name", "dep_name")) // no SoftDelete
	v := View("se").Version(1).Schema(schema).
		EmbedMany(JoinUpstream(core.NewExternalSchema("se_ext").PK("id"), "Mirror", "mirror")).On("emp_id")
	n := v.BuildViewNode()
	if paths := n.ChildSoftDeletePaths(); len(paths) != 0 {
		t.Errorf("no lifecycle segments here — paths must be empty, got %v", paths)
	}
	doc := map[string]any{
		sbvDepSeg: []any{map[string]any{"id": "d1", "dep_name": "Rita"}},
		"mirror":  []any{map[string]any{"id": "m1", "deleted_at": "2026-01-01"}},
	}
	n.StripArchivedChildren(doc)
	if deps, _ := doc[sbvDepSeg].([]any); len(deps) != 1 {
		t.Errorf("a soft-delete-less child collection must not strip, got %#v", doc[sbvDepSeg])
	}
	if mirror, _ := doc["mirror"].([]any); len(mirror) != 1 {
		t.Errorf("an explicit embed must never strip (upstream lifecycle), got %#v", doc["mirror"])
	}
}

// Coverage of the ROLE-VIEW branches the base-rooted fixtures cannot reach:
// the base-children segment claim in the collision validation and the
// base-children walk (plus a nil embed source) in the composed-column set.
func TestRoleView_BaseChildBranches(t *testing.T) {
	roleView := View("sbv_users_role").Version(1).Schema(sbvUserSchema())
	// A valid role view passes (the base-child segment claim runs clean).
	if err := ValidateViewSchemas([]*ViewDefinition{roleView}); err != nil {
		t.Fatalf("valid role view rejected: %v", err)
	}
	// An embed claiming the base-child's derived segment collides.
	colliding := View("sbv_users_role2").Version(1).Schema(sbvUserSchema()).
		EmbedMany(JoinUpstream(core.NewExternalSchema("ext").PK("id"), sbvAddrSeg, "mirror")).On("person_id")
	if err := ValidateViewSchemas([]*ViewDefinition{colliding}); err == nil ||
		!strings.Contains(err.Error(), "base-child") {
		t.Fatalf("an embed colliding with a base-child segment must be rejected, got %v", err)
	}
	// The composed-column set walks the base-children of a role view (the
	// addresses subtree is addressable) and tolerates a nil embed source.
	roleView.embeds = append(roleView.embeds, embedDef{leg: nil, many: true})
	set := roleView.composedColumnSet()
	if _, ok := set[sbvAddrSeg+".street"]; !ok {
		t.Errorf("a role view's base-child columns must be addressable, got %v", set)
	}
	if _, ok := set["ghost"]; ok {
		t.Error("a nil embed source must contribute nothing")
	}
}
