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
	return NewSharedBase("pessoa").
		PK("id").
		Field("Name", "name").
		Field("Created", "document").
		NaturalKey("document").
		Child(NewTableSchema[addrFixture]("endereco").PK("id").FK("pessoa_id").
			Field("Street", "street").Field("Zip", "zip"))
}

func TestBaseChildren_HappyPath_Accessors(t *testing.T) {
	base := baseWithChild()
	role := NewTableSchema[schemaSample]("aluno").
		PK("id").
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
	flat := NewTableSchema[schemaSample]("flat").PK("id").Field("Name", "name")
	if flat.BaseChildSchemas() != nil {
		t.Error("a schema without a shared base must report no base children")
	}
}

func TestBaseChildren_ResolveAggregateChild_RoleVsBase(t *testing.T) {
	base := baseWithChild()
	role := NewTableSchema[schemaSample]("aluno").
		PK("id").
		Field("Removed", "matricula").
		Child(NewTableSchema[embedFixture]("nota").PK("id").FK("aluno_id")). // role's OWN child
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
		NewSharedBase("pessoa").PK("id").Field("Name", "name").NaturalKey("name").
			Sibling(NewSiblingSchema[addrFixture]("pessoa_x").Field("Street", "street"))
	})
	// A base-child cannot itself nest a Child (no grandchildren).
	assertPanics(t, "grandchild under a base child", func() {
		grand := NewTableSchema[embedFixture]("grand").PK("id").FK("endereco_id")
		NewSharedBase("pessoa").PK("id").Field("Name", "name").NaturalKey("name").
			Child(NewTableSchema[addrFixture]("endereco").PK("id").FK("pessoa_id").
				Field("Street", "street").Child(grand))
	})
	// A base-child cannot declare a Sibling (v1).
	assertPanics(t, "sibling under a base child", func() {
		NewSharedBase("pessoa").PK("id").Field("Name", "name").NaturalKey("name").
			Child(NewTableSchema[addrFixture]("endereco").PK("id").FK("pessoa_id").
				Field("Street", "street").
				Sibling(NewSiblingSchema[addrFixture]("endereco_x").Field("Zip", "zip")))
	})
}

func TestBaseChildren_ValidateSharedBaseChildren(t *testing.T) {
	// A child type owned by BOTH the role and its base is rejected.
	assertPanics(t, "child owned by role and base", func() {
		base := baseWithChild() // base owns addrFixture
		role := NewTableSchema[schemaSample]("aluno").PK("id").Field("Removed", "matricula").
			Child(NewTableSchema[addrFixture]("aluno_addr").PK("id").FK("aluno_id").Field("Street", "street")).
			SharedBase(base, "pessoa_id")
		role.ValidateSharedBaseChildren()
	})
	// A base-child with SoftDelete but a base without SoftDelete is rejected.
	assertPanics(t, "base child soft-delete without base soft-delete", func() {
		base := NewSharedBase("pessoa").PK("id").Field("Name", "name").NaturalKey("name").
			Child(NewTableSchema[addrFixture]("endereco").PK("id").FK("pessoa_id").
				Field("Street", "street").SoftDelete("deleted_at"))
		role := NewTableSchema[schemaSample]("aluno").PK("id").Field("Removed", "matricula").
			SharedBase(base, "pessoa_id")
		role.ValidateSharedBaseChildren()
	})
	// A base + base-child that BOTH declare SoftDelete validate cleanly (all-or-nothing satisfied).
	base := NewSharedBase("pessoa").PK("id").Field("Name", "name").NaturalKey("name").SoftDelete("deleted_at").
		Child(NewTableSchema[addrFixture]("endereco").PK("id").FK("pessoa_id").
			Field("Street", "street").SoftDelete("deleted_at"))
	role := NewTableSchema[schemaSample]("aluno").PK("id").Field("Removed", "matricula").
		SharedBase(base, "pessoa_id")
	role.ValidateSharedBaseChildren() // must not panic
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
