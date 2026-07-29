package query

import (
	"fmt"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// SharedBaseView declares a read-side view ROOTED AT A SHARED BASE — the
// "all-in-one identity" projection: one Mongo document per shared identity
// (person), carrying the base's own fields FLAT at the root, the base's native
// children nested at the root (e.g. Addresses), and ONE SUB-DOCUMENT PER
// DECLARED ROLE (e.g. User, Employee) with that role's private fields, its
// siblings merged flat and its own children nested. It is the read-side mirror
// of the write side's SharedBase normalization — where the role views
// (query.View rooted at a role table) each show one specialization, this view
// shows the whole composed aggregate.
//
//	query.SharedBaseView("persons").
//	    Schema(personBase()).
//	    Role(UserSchema()).
//	    Role(EmployeeSchema()).
//	    Version(1).
//	    Indexes(query.Index("document"))
//
// Document identity: `_id` = the base's deterministic ID (UUIDv5 of the
// natural key) — stable across both role link models (shared-ID and
// separate-ParentID). The root soft-delete gate is the base's SoftDelete column: the
// base archives only when its last active role archives (write-side
// convergence), which is exactly the document-level visibility this view
// wants. Role sub-documents follow the ROLE's lifecycle: an absent role is an
// explicit `null` segment (the store's Upsert is $set — a vanished role must
// overwrite its stale segment), an archived role is stored with its
// soft-delete timestamp and stripped at read time unless ?includeArchived.
//
// Everything a regular view declares applies unchanged: Version (the role set
// participates in the rebuild hash — adding a role without bumping is
// DriftForgotToBump), Indexes/JSONSchema/Collation/Capped/TimeSeries,
// MaxLimit/MaxExportRows, DeleteOnArchive (drop the doc when the BASE
// archives), Embed/EmbedMany for external sources.
//
// The .Schema(...) must be a core.NewSharedBaseSchema declaration — attached exactly
// like a regular view attaches its root schema — and is validated at boot by
// ValidateViewSchemas. Every Role must be a type-anchored schema declaring
// .SharedBase(...) back to the SAME base table with an equivalent declaration
// (core.AssertSharedBaseEquivalent), checked when .Role(...) is declared; so
// .Schema(base) must come before the first .Role(...). A mis-wired view never
// boots.
func SharedBaseView(name string) *ViewDefinition {
	return &ViewDefinition{
		name:             name,
		isSharedBaseView: true,
	}
}

// roleDef is one declared role of a SharedBaseView. The segment — the document
// field AND the Go segment the criteria/Response refer to — is derived from
// the role schema's Go type name ("User", "Employee"), the same derivation rule
// nested children use (childDocSegment), minus the pluralization: a role is a
// single optional sub-document, not a collection.
type roleDef struct {
	schema  *core.TableSchema
	segment string
}

// RoleView is the read-only projection of one declared role, consumed by the
// composer, the SyncEngine routing, the ViewNode and the export planner.
type RoleView struct {
	// Schema is the role's own TableSchema (fields, siblings, children,
	// SharedBaseRef back to the base).
	Schema *core.TableSchema
	// Segment is the document field / Go segment the role projects under.
	Segment string
	// ParentIDColumn is the role table's link to the base ID — the role's own ID
	// column under the shared-ID model, a distinct column under separate-ParentID.
	ParentIDColumn string
}

// Role declares one specialization of the shared identity. Repeatable — the
// number of roles is open-ended. The role's TableSchema carries everything the
// projection needs: the role-private fields, the siblings (merged flat inside
// the segment), the role children (nested inside the segment) and the
// .SharedBase reference that names the ParentID column. The segment is the role's
// TypeName; declare roles of distinct types (two roles of the same Go type
// cannot share one view).
func (v *ViewDefinition) Role(role *core.TableSchema) *ViewDefinition {
	if !v.isSharedBaseView {
		panic(fmt.Sprintf(
			"query view %q: .Role(...) applies only to a query.SharedBaseView — a regular query.View "+
				"projects one role per view (root = the role table)", v.name))
	}
	if v.schema == nil {
		panic(fmt.Sprintf(
			"query.SharedBaseView(%q): declare .Schema(base) before .Role(...) — a role is validated "+
				"against the base identity schema", v.name))
	}
	if role == nil {
		panic(fmt.Sprintf("query.SharedBaseView(%q): .Role(nil)", v.name))
	}
	segment := role.TypeName()
	if segment == "" {
		panic(fmt.Sprintf(
			"query.SharedBaseView(%q): role over table %q is type-less — a role must be a type-anchored "+
				"core.NewTableSchema[T] (the segment name derives from the Go type)", v.name, role.Table()))
	}
	roleBase, _, ok := role.SharedBaseRef()
	if !ok {
		panic(fmt.Sprintf(
			"query.SharedBaseView(%q): role %q (table %q) declares no .SharedBase(...) — it does not "+
				"specialize a shared identity", v.name, segment, role.Table()))
	}
	if roleBase.Table() != v.schema.Table() {
		panic(fmt.Sprintf(
			"query.SharedBaseView(%q): role %q references base table %q, but this view roots at %q",
			v.name, segment, roleBase.Table(), v.schema.Table()))
	}
	// Divergent re-declarations of the same base panic here — same policing the
	// write-side engine registry applies (no singleton required, identity of
	// declaration required).
	core.AssertSharedBaseEquivalent(v.schema, roleBase)
	for _, r := range v.roles {
		if r.segment == segment {
			panic(fmt.Sprintf(
				"query.SharedBaseView(%q): duplicate role segment %q — each role type is declared once",
				v.name, segment))
		}
	}
	v.roles = append(v.roles, roleDef{schema: role, segment: segment})
	return v
}

// IsSharedBaseView reports whether this view roots at a shared base (declared
// via SharedBaseView) as opposed to a regular table root (query.View).
func (v *ViewDefinition) IsSharedBaseView() bool { return v.isSharedBaseView }

// RoleViews returns the declared roles in declaration order.
func (v *ViewDefinition) RoleViews() []RoleView {
	out := make([]RoleView, 0, len(v.roles))
	for _, r := range v.roles {
		_, fkCol, _ := r.schema.SharedBaseRef()
		out = append(out, RoleView{Schema: r.schema, Segment: r.segment, ParentIDColumn: fkCol})
	}
	return out
}
