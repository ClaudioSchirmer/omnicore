package infra

import "testing"

// ─── DeleteOnArchive opt-in ──────────────────────────────────────────────────

func TestViewDefinition_DeleteOnArchiveDefaultFalse_Flat(t *testing.T) {
	v := View("things").Root("things")
	if v.DeletesOnArchive() {
		t.Fatal("DeletesOnArchive() must default to false on a flat view")
	}
}

func TestViewDefinition_DeleteOnArchiveDefaultFalse_Aggregate(t *testing.T) {
	v := View("users").Root("users").
		EmbedMany("addresses", From("addresses").On("user_id"))
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
		EmbedMany("addresses", From("addresses").On("user_id"))
	if !v.DeletesOnArchive() {
		t.Fatal("expected DeletesOnArchive() = true after builder on aggregate view")
	}
	if len(v.Embeds()) != 1 {
		t.Fatalf("aggregate view should carry 1 embed, got %d", len(v.Embeds()))
	}
}

func TestSource_SchemaDef_NilWhenUnset(t *testing.T) {
	bare := From("addresses").On("user_id")
	if bare.SchemaDef() != nil {
		t.Error("SchemaDef() must be nil when .Schema(...) was not called")
	}
	ts := NewExternalSchema("users").PK("ID", "id")
	withSchema := FromMongo("users").On("buyer_id").Schema(ts)
	if withSchema.SchemaDef() != ts {
		t.Error("SchemaDef() must return the schema passed to .Schema(...)")
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

func (v vsChild) GetID() string { return v.ID }

func TestViewNode_TranslatesGoPathToColumnAndBack(t *testing.T) {
	rootSchema := NewTableSchema[vsRoot]("people").
		PK("ID", "person_pk").
		Field("Email", "mail").
		SoftDelete("removed_at").
		CreatedAt("created_at")
	childSchema := NewExternalSchema("tags").
		PK("ID", "tag_pk").
		FK("person_ref").
		Field("ZipCode", "zip")

	v := View("people").Root("people").Schema(rootSchema).
		EmbedMany("addresses", From("tags").On("person_ref").As("Addresses").Schema(childSchema))

	node := v.buildViewNode()

	// Go path → column path
	if col, ok := node.columnPath([]string{"Email"}); !ok || col[0] != "mail" {
		t.Errorf("Email → %v,%v want [mail]", col, ok)
	}
	if col, ok := node.columnPath([]string{"Addresses", "ZipCode"}); !ok || col[0] != "addresses" || col[1] != "zip" {
		t.Errorf("Addresses.ZipCode → %v,%v want [addresses zip]", col, ok)
	}
	if sd, ok := node.softDeleteColumn(); !ok || sd != "removed_at" {
		t.Errorf("soft-delete = %q,%v want removed_at", sd, ok)
	}
	// Managed columns translate forward (Go logical name → column) symmetrically
	// with the read-back, so a typed Response can sort/project on them.
	if col, ok := node.columnPath([]string{"CreatedAt"}); !ok || col[0] != "created_at" {
		t.Errorf("CreatedAt → %v,%v want [created_at]", col, ok)
	}
	if col, ok := node.columnPath([]string{"DeletedAt"}); !ok || col[0] != "removed_at" {
		t.Errorf("DeletedAt → %v,%v want [removed_at]", col, ok)
	}

	// column doc → Go doc (read-back), recursive into the embed
	doc := map[string]any{
		"person_pk":  "p1",
		"mail":       "a@x.test",
		"created_at": "2026-06-19T00:00:00Z",
		"addresses":  []any{map[string]any{"tag_pk": "t1", "zip": "10001", "person_ref": "p1"}},
	}
	got := node.toGoDoc(doc)
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
