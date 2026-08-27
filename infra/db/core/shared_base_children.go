package core

import "fmt"

// SharedBase native children (base-children). A SharedBase (e.g. pessoa) may own
// 1:N native collections via .Child(...) — data that belongs to the shared
// IDENTITY, not to any single role (e.g. a person's addresses), so it is shared
// across every role (aluno, professor) that references the base. Unlike a role's
// own aggregate children (ParentID → role id), a base-child's rows key on the base's
// DETERMINISTIC id (the role's ParentID to the base = UUIDv5(naturalKey)). The domain
// stays oblivious: the role's aggregate lists the child type in AggregateChildren()
// exactly like any other child; only the infra schema routes it to the base.
//
// The single difference the write/load/compose paths act on is the OWNER: this
// file gives them the base-vs-role partition (ResolveAggregateChild) plus the boot
// validation that the two never overlap and that the base-child's lifecycle stays
// coherent with the base.

// BaseChildSchemas returns, for a ROLE schema that references a shared base, the
// base's declared native children (nil for a non-role schema or a base without
// children). They are persisted/loaded/composed keyed by the base's deterministic
// id, NOT the role id.
func (s *TableSchema) BaseChildSchemas() []*TableSchema {
	base, _, ok := s.SharedBaseRef()
	if !ok {
		return nil
	}
	return base.ChildSchemas()
}

// BaseChildSchema resolves a base-child schema by Go type name (nil when the role
// declares no shared base, or the base declares no such child).
func (s *TableSchema) BaseChildSchema(typeName string) *TableSchema {
	base, _, ok := s.SharedBaseRef()
	if !ok {
		return nil
	}
	return base.childSchema(typeName)
}

// ResolveAggregateChild resolves an aggregate child type to its owning schema and
// reports whether it belongs to the shared BASE (fromBase=true → ParentID to the base's
// deterministic id) versus the role itself (fromBase=false → ParentID to the role id).
// The write and load paths route each child collection by this flag. ok=false when
// neither the role nor its base declares the type. A role's own child is checked
// first (it can never be a base-child too — ValidateSharedBaseChildren forbids the
// overlap at boot).
func (s *TableSchema) ResolveAggregateChild(typeName string) (child *TableSchema, fromBase bool, ok bool) {
	if c := s.childSchema(typeName); c != nil {
		return c, false, true
	}
	if c := s.BaseChildSchema(typeName); c != nil {
		return c, true, true
	}
	return nil, false, false
}

// EffectiveChildNames returns the Go type names of every aggregate child the role
// persists — its own children UNION the shared base's children. The aggregate
// boundary check (domain AggregateChildren() ⟺ schema) validates against this set,
// so a base-child counts as "declared by the schema". Order is unspecified.
func (s *TableSchema) EffectiveChildNames() []string {
	own := s.ChildSchemaNames()
	base, _, ok := s.SharedBaseRef()
	if !ok {
		return own
	}
	out := make([]string, 0, len(own)+len(base.children))
	out = append(out, own...)
	for name := range base.children {
		out = append(out, name)
	}
	return out
}

// ValidateSharedBaseChildren runs at WithSchema (order-independent, like
// ValidateChildDepth/ValidateSiblings) on a ROLE schema. It asserts the
// base-vs-role invariants that need both schemas in hand: no child type is owned
// by both the role and its base, and no base-child declares an archived state its
// base can never drive.
//
// The second rule is one-directional on purpose. A base-child WITH DeletedAt under
// a base WITHOUT one is refused: the base never archives, so nothing would ever
// cascade onto that column and the archived state would be reachable only by a
// per-item removal — a lifecycle with no owner. The opposite pairing is legal: a
// base-child that declares no column under an archivable base simply removes by
// DELETE and takes no part in the base's cascade, exactly like a role's own child
// (aggregate-persistence: "What a child's DeletedAt declares"). A no-op for a
// schema without a shared base.
func (s *TableSchema) ValidateSharedBaseChildren() {
	base, _, ok := s.SharedBaseRef()
	if !ok {
		return
	}
	for _, bc := range base.ChildSchemas() {
		name := bc.typ.Name()
		if s.childSchema(name) != nil {
			panic(fmt.Sprintf(
				"infra.TableSchema(%s): aggregate child %q is declared on BOTH the role and its shared base "+
					"%q — a child type belongs to exactly one owner (the role OR the base). Drop one .Child(%q).",
				s.table, name, base.table, name))
		}
		if bc.deletedAt != "" && base.deletedAt == "" {
			panic(fmt.Sprintf(
				"infra.TableSchema(%s): base child %q declares DeletedAt but its shared base %q has none — the "+
					"base never archives, so nothing would ever cascade onto that column and the archived state "+
					"would have no owner. Declare DeletedAt on the base too, or drop it from the child (a "+
					"base-child without the column removes by DELETE, which is a complete lifecycle of its own).",
				s.table, name, base.table))
		}
	}
}
