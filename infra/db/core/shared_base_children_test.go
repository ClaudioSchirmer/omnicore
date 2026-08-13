package core

import "testing"

// Coverage for SharedBase native children (base-children): the base-vs-role
// partition accessors and every boot guard.

type addrFixture struct {
	ID     string
	Street string
	Zip    string
}

// baseWithChild builds a shared base (pessoa) that owns a native child (endereco).
func baseWithChild() *TableSchema {
	return NewSharedBaseSchema("pessoa").Revision("revision").
		ID("id").
		Field("Name", "name").
		Field("Created", "document").
		NaturalID("document").
		Child(NewTableSchema[addrFixture]("endereco").ID("id").ParentID("pessoa_id").
			Field("Street", "street").Field("Zip", "zip"))
}

func TestBaseChildren_HappyPath_Accessors(t *testing.T) {
	base := baseWithChild()
	role := NewTableSchema[schemaSample]("aluno").
		ID("id").
		Field("Removed", "matricula").
		SharedBase(base, "pessoa_id")

	bc := role.BaseChildSchemas()
	if len(bc) != 1 || bc[0].Table() != "endereco" {
		t.Fatalf("BaseChildSchemas() = %v, want [endereco]", bc)
	}
	if role.BaseChildSchema("addrFixture") == nil {
		t.Error("BaseChildSchema(addrFixture) must resolve the base child")
	}
	if role.BaseChildSchema("nope") != nil {
		t.Error("BaseChildSchema(nope) must be nil")
	}
	// A schema without a shared base exposes no base children.
	flat := NewTableSchema[schemaSample]("flat").ID("id").Field("Name", "name")
	if flat.BaseChildSchemas() != nil {
		t.Error("a schema without a shared base must report no base children")
	}
}

func TestBaseChildren_ResolveAggregateChild_RoleVsBase(t *testing.T) {
	base := baseWithChild()
	role := NewTableSchema[schemaSample]("aluno").
		ID("id").
		Field("Removed", "matricula").
		Child(NewTableSchema[embedFixture]("nota").ID("id").ParentID("aluno_id")). // role's OWN child
		SharedBase(base, "pessoa_id")

	// base-child resolves with fromBase=true
	if c, fromBase, ok := role.ResolveAggregateChild("addrFixture"); !ok || !fromBase || c.Table() != "endereco" {
		t.Errorf("ResolveAggregateChild(addrFixture) = %v,%v,%v — want endereco,true,true", c, fromBase, ok)
	}
	// role's own child resolves with fromBase=false
	if c, fromBase, ok := role.ResolveAggregateChild("embedFixture"); !ok || fromBase || c.Table() != "nota" {
		t.Errorf("ResolveAggregateChild(embedFixture) = %v,%v,%v — want nota,false,true", c, fromBase, ok)
	}
	// unknown type resolves to ok=false
	if _, _, ok := role.ResolveAggregateChild("nope"); ok {
		t.Error("ResolveAggregateChild(nope) must report ok=false")
	}
	// EffectiveChildNames unions role + base children
	names := role.EffectiveChildNames()
	if !contains(names, "embedFixture") || !contains(names, "addrFixture") {
		t.Errorf("EffectiveChildNames() = %v, want both embedFixture and addrFixture", names)
	}
}

func TestBaseChildren_BootGuards(t *testing.T) {
	// A shared base cannot declare a Sibling.
	assertPanics(t, "Sibling on a shared base", func() {
		NewSharedBaseSchema("pessoa").Revision("revision").ID("id").Field("Name", "name").NaturalID("name").
			Sibling(NewSiblingSchema[addrFixture]("pessoa_x").Field("Street", "street"))
	})
	// A base-child cannot itself nest a Child (no grandchildren).
	assertPanics(t, "grandchild under a base child", func() {
		grand := NewTableSchema[embedFixture]("grand").ID("id").ParentID("endereco_id")
		NewSharedBaseSchema("pessoa").Revision("revision").ID("id").Field("Name", "name").NaturalID("name").
			Child(NewTableSchema[addrFixture]("endereco").ID("id").ParentID("pessoa_id").
				Field("Street", "street").Child(grand))
	})
	// A base-child cannot declare a Sibling (v1).
	assertPanics(t, "sibling under a base child", func() {
		NewSharedBaseSchema("pessoa").Revision("revision").ID("id").Field("Name", "name").NaturalID("name").
			Child(NewTableSchema[addrFixture]("endereco").ID("id").ParentID("pessoa_id").
				Field("Street", "street").
				Sibling(NewSiblingSchema[addrFixture]("endereco_x").Field("Zip", "zip")))
	})
}

func TestBaseChildren_ValidateSharedBaseChildren(t *testing.T) {
	// A child type owned by BOTH the role and its base is rejected.
	assertPanics(t, "child owned by role and base", func() {
		base := baseWithChild() // base owns addrFixture
		role := NewTableSchema[schemaSample]("aluno").ID("id").Field("Removed", "matricula").
			Child(NewTableSchema[addrFixture]("aluno_addr").ID("id").ParentID("aluno_id").Field("Street", "street")).
			SharedBase(base, "pessoa_id")
		role.ValidateSharedBaseChildren()
	})
	// A base-child with DeletedAt but a base without DeletedAt is rejected.
	assertPanics(t, "base child DeletedAt without base DeletedAt", func() {
		base := NewSharedBaseSchema("pessoa").Revision("revision").ID("id").Field("Name", "name").NaturalID("name").
			Child(NewTableSchema[addrFixture]("endereco").ID("id").ParentID("pessoa_id").
				Field("Street", "street").DeletedAt("deleted_at"))
		role := NewTableSchema[schemaSample]("aluno").ID("id").Field("Removed", "matricula").
			SharedBase(base, "pessoa_id")
		role.ValidateSharedBaseChildren()
	})
	// A base + base-child that BOTH declare DeletedAt validate cleanly (all-or-nothing satisfied).
	base := NewSharedBaseSchema("pessoa").Revision("revision").ID("id").Field("Name", "name").NaturalID("name").DeletedAt("deleted_at").
		Child(NewTableSchema[addrFixture]("endereco").ID("id").ParentID("pessoa_id").
			Field("Street", "street").DeletedAt("deleted_at"))
	role := NewTableSchema[schemaSample]("aluno").ID("id").Field("Removed", "matricula").
		SharedBase(base, "pessoa_id")
	role.ValidateSharedBaseChildren() // must not panic
}

// TestChild_CannotReferenceSharedBase asserts the boot guard that turns the
// formerly-silent "a child that references a SharedBase" misconfiguration into a
// loud panic. Write and load resolve the shared base from the ROOT schema alone,
// so a SharedBase on a child was accepted and then ignored (never persisted or
// loaded). The guard lives in Child(), so it fires for a root's own child and a
// base-child alike.
func TestChild_CannotReferenceSharedBase(t *testing.T) {
	// A root's own child that itself references a SharedBase is rejected at boot.
	assertPanics(t, "root child referencing a shared base", func() {
		base := NewSharedBaseSchema("pessoa").Revision("revision").ID("id").Field("Street", "b_street").NaturalID("b_street")
		childWithBase := NewTableSchema[addrFixture]("endereco").ID("id").ParentID("root_id").
			SharedBase(base, "pessoa_id")
		NewTableSchema[schemaSample]("root").ID("id").Field("Removed", "matricula").
			Child(childWithBase)
	})
	// The same rule on the base side: a base-child may not reference a SharedBase.
	assertPanics(t, "base child referencing a shared base", func() {
		other := NewSharedBaseSchema("org").Revision("revision").ID("id").Field("Street", "o_street").NaturalID("o_street")
		baseChildWithBase := NewTableSchema[addrFixture]("endereco").ID("id").ParentID("pessoa_id").
			SharedBase(other, "org_id")
		NewSharedBaseSchema("pessoa").Revision("revision").ID("id").Field("Name", "name").NaturalID("name").
			Child(baseChildWithBase)
	})
	// Control: a normal root child (no SharedBase) plus a separate role that DOES
	// reference the base is the supported shape — it must NOT panic.
	base := NewSharedBaseSchema("pessoa").Revision("revision").ID("id").Field("Name", "name").NaturalID("name")
	NewTableSchema[schemaSample]("aluno").ID("id").Field("Removed", "matricula").
		Child(NewTableSchema[addrFixture]("nota").ID("id").ParentID("aluno_id").Field("Street", "street")).
		SharedBase(base, "pessoa_id")
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func (addrFixture) CollectionName() string { return "AddrFixtures" }
