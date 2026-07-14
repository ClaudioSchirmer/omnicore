package query

import (
	"github.com/ClaudioSchirmer/omnicore/domain"

	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// ─── test embed-source helpers ───────────────────────────────────────────────
//
// pgEmbed / mongoEmbed build a FromSchema source with a minimal schema (PK + FK)
// for tests. pgEmbed is type-anchored (local PG); mongoEmbed is type-less
// (external/Mongo). For a one-to-one Embed, pass fk="" and set the parent join
// via Source.FK(...).
type embedFixture struct{ ID string }

func pgEmbed(table, fk string) *Source {
	return FromSchema(core.NewTableSchema[embedFixture](table).PK("id").FK(fk))
}

func mongoEmbed(table, fk string) *Source {
	return FromSchema(core.NewExternalSchema(table).PK("id").FK(fk))
}

// rootSchema is a minimal type-anchored schema for a composing test view's root
// (PK + soft-delete). The composer only reads PK + soft-delete from the root
// schema; row columns are read generically, so the dummy type suffices.
func rootSchema(table string) *core.TableSchema {
	return core.NewTableSchema[embedFixture](table).PK("id").SoftDelete("deleted_at")
}

// ─── own children on a schema project automatically (read side) ──────────────

// TestValidateViewSchemas_RejectsAnchoredEmbedSource proves the canonical split:
// Embed/EmbedMany compose only EXTERNAL data, so a write-anchored embed source is
// a boot error. A local aggregate's own data (root / siblings / SharedBase / own
// children) projects automatically from the TableSchema, never through an embed.
func TestValidateViewSchemas_RejectsAnchoredEmbedSource(t *testing.T) {
	src := FromSchema(core.NewTableSchema[embedFixture]("addresses").PK("id").FK("user_id"))
	v := View("users").Version(1).Root("users").
		Schema(rootSchema("users")).
		EmbedMany("addresses", src)

	err := ValidateViewSchemas([]*ViewDefinition{v})
	if err == nil {
		t.Fatal("expected a validation error for a write-anchored embed source")
	}
	if !strings.Contains(err.Error(), "write-anchored") {
		t.Errorf("error should name the external-only rule, got: %v", err)
	}
}

// TestValidateViewSchemas_RootSchemaWithChildrenOK confirms the view ROOT schema
// may carry Child(...) — its own children auto-project, no embed needed.
func TestValidateViewSchemas_RootSchemaWithChildrenOK(t *testing.T) {
	rootWithChild := core.NewTableSchema[embedFixture]("users").PK("id").SoftDelete("deleted_at").
		Child(core.NewTableSchema[schemaSample]("addresses").PK("id").FK("user_id"))
	v := View("users").Version(1).Root("users").
		Schema(rootWithChild)

	if err := ValidateViewSchemas([]*ViewDefinition{v}); err != nil {
		t.Fatalf("root schema with one-level children must pass view validation, got: %v", err)
	}
}

// TestValidateViewSchemas_RejectsSegmentCollision proves the collision guard: a
// legal EXTERNAL embed whose .As segment collides with an auto-projected own-child
// segment is a boot error — one segment, one source.
func TestValidateViewSchemas_RejectsSegmentCollision(t *testing.T) {
	child := core.NewTableSchema[schemaSample]("addresses").PK("id").FK("user_id")
	seg := childDocSegment(child) // the auto own-child doc segment
	rootWithChild := core.NewTableSchema[embedFixture]("users").PK("id").SoftDelete("deleted_at").
		Child(child)
	// A legal external embed whose .As segment collides with the own-child segment.
	v := View("users").Version(1).Root("users").
		Schema(rootWithChild).
		EmbedMany("ext", FromSchema(core.NewExternalSchema("ext_coll").PK("id").FK("user_id")).As(seg))

	err := ValidateViewSchemas([]*ViewDefinition{v})
	if err == nil {
		t.Fatal("expected a collision error for an external embed segment clashing with an auto own-child")
	}
	if !strings.Contains(err.Error(), "exactly one source") {
		t.Errorf("error should name the one-source-per-segment rule, got: %v", err)
	}
}

// ─── mandatory PK on every view schema (read side) ───────────────────────────

// TestValidateViewSchemas_RejectsRootWithoutPK proves a view root schema with no
// explicit PK is a fatal view-validation error.
func TestValidateViewSchemas_RejectsRootWithoutPK(t *testing.T) {
	v := View("users").Version(1).Root("users").
		Schema(core.NewTableSchema[embedFixture]("users").SoftDelete("deleted_at")). // no .PK
		EmbedMany("addresses", pgEmbed("addresses", "user_id"))
	err := ValidateViewSchemas([]*ViewDefinition{v})
	if err == nil || !strings.Contains(err.Error(), "no primary key") {
		t.Errorf("expected root-without-PK error, got %v", err)
	}
}

// TestValidateViewSchemas_RejectsEmbedSourceWithoutPK proves an embed source
// with no explicit PK is a fatal view-validation error.
func TestValidateViewSchemas_RejectsEmbedSourceWithoutPK(t *testing.T) {
	src := FromSchema(core.NewTableSchema[embedFixture]("addresses").FK("user_id")) // no .PK
	v := View("users").Version(1).Root("users").
		Schema(rootSchema("users")).
		EmbedMany("addresses", src)
	err := ValidateViewSchemas([]*ViewDefinition{v})
	if err == nil || !strings.Contains(err.Error(), "no primary key") {
		t.Errorf("expected embed-source-without-PK error, got %v", err)
	}
}

// ─── mandatory join keys on embed sources (read side) ────────────────────────

// TestValidateViewSchemas_RejectsEmbedManyWithoutFK proves an EmbedMany source
// without a foreign key is a fatal view-validation error — the composer joins
// the child rows on it.
func TestValidateViewSchemas_RejectsEmbedManyWithoutFK(t *testing.T) {
	src := FromSchema(core.NewTableSchema[embedFixture]("addresses").PK("id")) // no .FK
	v := View("users").Version(1).Root("users").
		Schema(rootSchema("users")).
		EmbedMany("addresses", src)
	err := ValidateViewSchemas([]*ViewDefinition{v})
	if err == nil || !strings.Contains(err.Error(), "no foreign key") {
		t.Errorf("expected EmbedMany-without-FK error, got %v", err)
	}
}

// TestValidateViewSchemas_RejectsOneToOneEmbedWithoutOn proves a one-to-one
// Embed without .On is a fatal view-validation error — it joins on the parent's
// FK column, which must be named.
func TestValidateViewSchemas_RejectsOneToOneEmbedWithoutOn(t *testing.T) {
	src := FromSchema(core.NewTableSchema[embedFixture]("buyer").PK("id")) // no .On
	v := View("orders").Version(1).Root("orders").
		Schema(rootSchema("orders")).
		Embed("buyer", src)
	err := ValidateViewSchemas([]*ViewDefinition{v})
	if err == nil || !strings.Contains(err.Error(), "parent join key") {
		t.Errorf("expected one-to-one-Embed-without-.On error, got %v", err)
	}
}

// ─── DeleteOnArchive opt-in ──────────────────────────────────────────────────

func TestViewDefinition_DeleteOnArchiveDefaultFalse_Flat(t *testing.T) {
	v := View("things").Root("things")
	if v.DeletesOnArchive() {
		t.Fatal("DeletesOnArchive() must default to false on a flat view")
	}
}

func TestViewDefinition_DeleteOnArchiveDefaultFalse_Aggregate(t *testing.T) {
	v := View("users").Root("users").
		EmbedMany("addresses", pgEmbed("addresses", "user_id"))
	if v.DeletesOnArchive() {
		t.Fatal("DeletesOnArchive() must default to false on an aggregate view")
	}
	if len(v.Embeds()) != 1 {
		t.Fatalf("aggregate view should carry 1 embed, got %d", len(v.Embeds()))
	}
}

func TestViewDefinition_DeleteOnArchiveBuilder_Flat(t *testing.T) {
	v := View("things").DeleteOnArchive().Root("things")
	if !v.DeletesOnArchive() {
		t.Fatal("expected DeletesOnArchive() = true after .DeleteOnArchive() builder")
	}
	if v.RootTable() != "things" {
		t.Errorf("chaining broken: RootTable = %q, want %q", v.RootTable(), "things")
	}
}

func TestViewDefinition_DeleteOnArchiveBuilder_Aggregate(t *testing.T) {
	v := View("users").DeleteOnArchive().Root("users").
		EmbedMany("addresses", pgEmbed("addresses", "user_id"))
	if !v.DeletesOnArchive() {
		t.Fatal("expected DeletesOnArchive() = true after builder on aggregate view")
	}
	if len(v.Embeds()) != 1 {
		t.Fatalf("aggregate view should carry 1 embed, got %d", len(v.Embeds()))
	}
}

func TestSource_SchemaDef_AndKindFromSchema(t *testing.T) {
	ext := core.NewExternalSchema("users").PK("id")
	mongo := FromSchema(ext)
	if mongo.SchemaDef() != ext {
		t.Error("SchemaDef() must return the schema FromSchema was built with")
	}
	if !mongo.IsMongo() {
		t.Error("FromSchema(core.NewExternalSchema(...)) must be a Mongo source")
	}
	pg := FromSchema(core.NewTableSchema[embedFixture]("addresses").PK("id"))
	if pg.IsMongo() {
		t.Error("FromSchema(core.NewTableSchema[...]) must be a PG source")
	}
	if pg.Table() != "addresses" {
		t.Errorf("Table() = %q, want addresses (from schema)", pg.Table())
	}
}

// ─── Schema-driven view translation tree ─────────────────────────────────────

type vsRoot struct {
	Email string
}

type vsChild struct {
	ID      string
	ZipCode string
}

func (v vsChild) GetID() domain.ID { return domain.NewID(v.ID) }

// TestViewNode_OwnChildPathResolves proves the translator registers a root's own
// aggregate children (no embed declared), so a filter/sort on an own-child field
// resolves via ColumnPath and the read-back nests the collection under its Go
// segment — the translator half of Phase-1 own-child projection.
func TestViewNode_OwnChildPathResolves(t *testing.T) {
	childSchema := core.NewTableSchema[csComposeVO]("lines").PK("id").FK("order_id").Field("Label", "label")
	rootWithChild := core.NewTableSchema[*builderTestEntity]("orders").
		PK("id").Field("Name", "name").SoftDelete("deleted_at").
		Child(childSchema)
	node := View("orders").Root("orders").Schema(rootWithChild).BuildViewNode()

	seg := childDocSegment(childSchema)
	if col, ok := node.ColumnPath([]string{seg, "Label"}); !ok || len(col) != 2 || col[0] != seg || col[1] != "label" {
		t.Errorf("%s.Label → %v,%v want [%s label]", seg, col, ok, seg)
	}
	doc := map[string]any{
		"id":   "o1",
		"name": "first",
		seg:    []any{map[string]any{"id": "l1", "order_id": "o1", "label": "L1"}},
	}
	got := node.ToGoDoc(doc)
	rows, ok := got[seg].([]any)
	if !ok || len(rows) != 1 {
		t.Fatalf("read-back %s = %v", seg, got[seg])
	}
	if row, _ := rows[0].(map[string]any); row["Label"] != "L1" {
		t.Errorf("own-child read-back Label = %v want L1 (row=%v)", row["Label"], rows[0])
	}
}

func TestViewNode_TranslatesGoPathToColumnAndBack(t *testing.T) {
	rootSchema := core.NewTableSchema[vsRoot]("people").
		PK("person_pk").
		Field("Email", "mail").
		SoftDelete("removed_at").
		CreatedAt("created_at")
	childSchema := core.NewExternalSchema("tags").
		PK("tag_pk").
		FK("person_ref").
		Field("ZipCode", "zip")

	v := View("people").Root("people").Schema(rootSchema).
		EmbedMany("addresses", FromSchema(childSchema).As("Addresses"))

	node := v.BuildViewNode()

	// Go path → column path
	if col, ok := node.ColumnPath([]string{"Email"}); !ok || col[0] != "mail" {
		t.Errorf("Email → %v,%v want [mail]", col, ok)
	}
	if col, ok := node.ColumnPath([]string{"Addresses", "ZipCode"}); !ok || col[0] != "addresses" || col[1] != "zip" {
		t.Errorf("Addresses.ZipCode → %v,%v want [addresses zip]", col, ok)
	}
	if sd, ok := node.SoftDeleteColumn(); !ok || sd != "removed_at" {
		t.Errorf("soft-delete = %q,%v want removed_at", sd, ok)
	}
	// Managed columns translate forward (Go logical name → column) symmetrically
	// with the read-back, so a typed Response can sort/project on them.
	if col, ok := node.ColumnPath([]string{"CreatedAt"}); !ok || col[0] != "created_at" {
		t.Errorf("CreatedAt → %v,%v want [created_at]", col, ok)
	}
	if col, ok := node.ColumnPath([]string{"DeletedAt"}); !ok || col[0] != "removed_at" {
		t.Errorf("DeletedAt → %v,%v want [removed_at]", col, ok)
	}

	// column doc → Go doc (read-back), recursive into the embed
	doc := map[string]any{
		"person_pk":  "p1",
		"mail":       "a@x.test",
		"created_at": "2026-06-19T00:00:00Z",
		"addresses":  []any{map[string]any{"tag_pk": "t1", "zip": "10001", "person_ref": "p1"}},
	}
	got := node.ToGoDoc(doc)
	if got["Email"] != "a@x.test" {
		t.Errorf("read-back Email = %v", got["Email"])
	}
	if got["CreatedAt"] != "2026-06-19T00:00:00Z" {
		t.Errorf("read-back CreatedAt = %v", got["CreatedAt"])
	}
	if got["ID"] != "p1" {
		t.Errorf("read-back ID = %v want p1", got["ID"])
	}
	addrs, ok := got["Addresses"].([]any)
	if !ok || len(addrs) != 1 {
		t.Fatalf("read-back Addresses = %v", got["Addresses"])
	}
	child := addrs[0].(map[string]any)
	if child["ZipCode"] != "10001" || child["ID"] != "t1" {
		t.Errorf("read-back child = %v", child)
	}
}
