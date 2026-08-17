package core

import "fmt"

// SharedBase (Modelagem 2 / Party-Role) — an identity table shared by multiple
// ROLE schemas. Unlike a Sibling (1:1 private, shared ID), a shared base is
// referenced by N independent roles (aluno, professor, usuario) via a foreign
// key, deduplicated by a NATURAL KEY whose value derives the base's
// deterministic id (UUIDv5). The base drives no lifecycle of its own: the roles
// control their own DeletedAt and the base CONVERGES to them when it declares a
// DeletedAt of its own (optional, honored when declared — see the KeepOrphan
// note below), governed by reference counting per its OrphanPolicy. Declared once with NewSharedBaseSchema and referenced from each role
// with .SharedBase(base, parentIDColumn) — the SAME instance referenced by every role
// IS the cross-schema registry.

// OrphanPolicy governs a shared base row when no role references it anymore
// (every referencing role row — of any type, active or archived — is gone).
type OrphanPolicy int

const (
	// KeepOrphan (the default) leaves the base row in place even with no role
	// referencing it. When the base declares DeletedAt, the orphaned identity is
	// archived (with its native children) instead of staying active — dormant and
	// revivable, never destroyed. Destruction is opt-in via DeleteWhenUnreferenced.
	KeepOrphan OrphanPolicy = iota
	// DeleteWhenUnreferenced hard-deletes the base row (and its native children)
	// once the last referencing role row is gone. The delete is DATABASE-VETOABLE:
	// it runs under a savepoint, and a foreign-key violation from ANY referencing
	// table — including one the schema registry does not know about (another
	// system sharing the database) — keeps the base instead of failing the role
	// delete. Declare the role→base FKs as plain/RESTRICT so the veto can fire; an
	// ON DELETE CASCADE ParentID opts that table into the destruction instead.
	DeleteWhenUnreferenced
)

// RoleRef names a role table that references a shared base + the ParentID column it
// links through + the role's own DeletedAt column ("" when the role has none).
// A shared base accumulates one per referencing role (the instance is the
// cross-schema registry). The unified lifecycle uses DeletedAtCol to tell an
// ACTIVE role row apart from an archived one when it decides whether the base
// (driven by its roles) should be active or archived.
type RoleRef struct {
	Table          string
	ParentIDColumn string
	DeletedAtCol   string
}

// roleLink is, on a shared base, one referencing role: a pointer to the role
// schema (so its DeletedAt column reads lazily, after the schema is fully
// assembled) + the ParentID column the role links through to the base's ID.
type roleLink struct {
	schema         *TableSchema
	parentIDColumn string
}

// sharedBaseLink is a role schema's reference to its shared base: the base
// schema + the role's ParentID column pointing to the base's ID, plus the pre-resolved
// scan plan (base column → field index in the ROLE's Go type) so the loader can
// scan the shared columns straight into the role struct.
type sharedBaseLink struct {
	base           *TableSchema
	parentIDColumn string
	scanCols       []string             // base columns, in declaration order
	scanByCol      map[string]FieldPath // base column → path into the role's Go type
}

// NewSharedBaseSchema starts a shared-base schema for table. It is TYPE-LESS like
// NewExternalSchema (its fields are validated against each referencing role's Go
// type at .SharedBase time, since the same base is shared across several roles).
// Declare its columns with Field, the dedup/identity column with NaturalID, and
// the lifecycle with OrphanPolicy.
func NewSharedBaseSchema(table string) *TableSchema {
	s := newSchema(table)
	s.isSharedBase = true
	return s
}

// NaturalID declares the shared base's natural-key column — the immutable
// business key that deduplicates the identity and derives its deterministic id.
// SharedBase only; the column must be a declared Field of the base.
func (s *TableSchema) NaturalID(column string) *TableSchema {
	if !s.isSharedBase {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): NaturalID applies only to a SharedBase (NewSharedBaseSchema).", s.table))
	}
	if column == "" {
		panic(fmt.Sprintf("infra.TableSchema(%s): NaturalID requires a non-empty column.", s.table))
	}
	s.mustNotRedeclare(s.naturalIDCol, "NaturalID", column)
	s.naturalIDCol = column
	return s
}

// OrphanPolicy sets the shared base's reference-count lifecycle. SharedBase only.
func (s *TableSchema) OrphanPolicy(p OrphanPolicy) *TableSchema {
	if !s.isSharedBase {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): OrphanPolicy applies only to a SharedBase (NewSharedBaseSchema).", s.table))
	}
	s.orphanPolicy = p
	return s
}

// Revision declares the schema's framework-managed revision column — a BIGINT
// NOT NULL the write path initializes to 1 on the row's creation and
// increments (revision = revision + 1) IN THE SAME STATEMENT of every
// UPDATE/archive/unarchive, under that row's lock. The value is therefore a
// deterministic commit-order token: it travels on the outbox payload
// (_ids.revision for the aggregate's own row, _ids.base_revision for a shared
// base) and the read side refuses any document write carrying an OLDER
// revision — the defense that makes a zombie consumer (a slow pod finishing an
// in-flight event after a partition handoff) harmless. MANDATORY on every
// ROOT schema attached to a repository and on every shared base; a sibling or
// aggregate child declares none (its rows are guarded by its owner's token).
func (s *TableSchema) Revision(column string) *TableSchema {
	if s.secondary || s.parentIDColumn != "" {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): Revision belongs to an ENTITY schema or a SharedBase — a sibling/child row "+
				"is guarded by its owner's revision. Drop this Revision(%q) call.", s.table, column))
	}
	if column == "" {
		panic(fmt.Sprintf("infra.TableSchema(%s): Revision requires a non-empty column.", s.table))
	}
	s.mustNotRedeclare(s.revisionCol, "Revision", column)
	mustNotReservedColumn(s.table, column)
	s.ensureColumnFree(column, "Revision")
	s.revisionCol = column
	return s
}

// RevisionColumn returns the shared base's revision column ("" when not a
// shared base — Revision is mandatory on every attached base, so a role's
// resolved base always answers non-empty).
func (s *TableSchema) RevisionColumn() string { return s.revisionCol }

// SharedBase attaches a shared base to this role schema: the base identity plus
// the role's ParentID column referencing the base's deterministic id. A role
// references AT MOST ONE shared base (more than one would mean multiple
// candidate natural keys — a domain concern, not infra). The base's fields are
// validated against THIS role's Go type (the base is type-less; each role
// supplies the type to validate the shared columns against).
func (s *TableSchema) SharedBase(base *TableSchema, parentIDColumn string) *TableSchema {
	if s.secondary || s.isSharedBase {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): only a root/role schema may reference a SharedBase.", s.table))
	}
	// A role cannot ALSO be an aggregate child: two FKs = two parents, and an
	// ambiguous "ParentID" projection. Reject the combination (the mirror of the guard
	// in ParentID()).
	if s.parentIDColumn != "" {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): a schema cannot declare both SharedBase(...) (role) and ParentID(...) (aggregate "+
				"child) — it would have two parents. Model it as one or the other.", s.table))
	}
	if base == nil || !base.isSharedBase {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): SharedBase(...) expects a NewSharedBaseSchema(...); got a non-shared-base schema.",
			s.table))
	}
	if s.sharedBaseLink != nil {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): a role may reference at most ONE shared base — multiple shared bases mean "+
				"multiple candidate natural keys, a domain concern. Model it in the domain or a manual handler.",
			s.table))
	}
	if parentIDColumn == "" {
		panic(fmt.Sprintf("infra.TableSchema(%s): SharedBase requires a non-empty ParentID column.", s.table))
	}
	if !base.hasPKDeclared() {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): shared base %q declares no ID — declare .ID(column) for the id column "+
				"(the deterministic UUIDv5 lands there; the role ParentID references it).", s.table, base.table))
	}
	if base.naturalIDCol == "" {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): shared base %q declares no NaturalID — it is the source of the deterministic "+
				"id and of de-duplication.", s.table, base.table))
	}
	if _, ok := base.byCol[base.naturalIDCol]; !ok {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): shared base %q NaturalID(%q) is not a declared field of the base.",
			s.table, base.table, base.naturalIDCol))
	}
	if base.revisionCol == "" {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): shared base %q declares no Revision — declare .Revision(column) (BIGINT NOT NULL "+
				"DEFAULT 0 in the migration): it is the write-serialized token that orders concurrent role writes of "+
				"the shared identity on the read side.", s.table, base.table))
	}
	if len(base.fields) == 0 {
		panic(fmt.Sprintf("infra.TableSchema(%s): shared base %q declares no fields.", s.table, base.table))
	}
	if _, dup := s.byCol[parentIDColumn]; dup {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): the shared-base link column %q must not also be a declared field — it is the "+
				"ParentID to the base.", s.table, parentIDColumn))
	}
	// The base is type-less; validate its shared field Go-names exist on THIS
	// role's Go type (the role anchors the type the shared columns map into) —
	// including the closed persistable-type check, since the base's own
	// declaration had no type to validate against — and capture the resolved
	// field indices for the read-side scan.
	scanCols := make([]string, 0, len(base.fields))
	scanByCol := make(map[string]FieldPath, len(base.fields))
	if s.typ != nil {
		for _, f := range base.fields {
			path := s.resolveBaseFieldPath(base, f)
			// A value-object shared field validates its UNDERLYING scalar (the
			// write path unwraps it, the read path reconstructs via the role's
			// field type) — same rule as Field() on a type-anchored schema.
			ft, _ := path.TypeIn(s.typ)
			if _, u, ok := valueObjectField(ft); ok {
				mustSupportedFieldType(base.table, f.goName, u)
			} else {
				mustSupportedFieldType(base.table, f.goName, ft)
			}
			scanCols = append(scanCols, f.column)
			scanByCol[f.column] = path
		}
	}
	s.sharedBaseLink = &sharedBaseLink{base: base, parentIDColumn: parentIDColumn, scanCols: scanCols, scanByCol: scanByCol}
	// Register this role on the base — the shared instance is the cross-schema
	// registry the refcount delete + CDC fan-out + lifecycle convergence enumerate.
	// Store the schema pointer (not a snapshot) so the role's DeletedAt column is
	// read lazily, order-independent of .DeletedAt vs .SharedBase.
	base.referencingRoleLinks = append(base.referencingRoleLinks, roleLink{schema: s, parentIDColumn: parentIDColumn})
	return s
}

// --- accessors ---------------------------------------------------------------

// IsSharedBase reports whether this schema is a shared base (NewSharedBaseSchema).
func (s *TableSchema) IsSharedBase() bool { return s != nil && s.isSharedBase }

// NaturalIDColumn returns the shared base's natural-key column ("" when not a
// shared base or undeclared).
func (s *TableSchema) NaturalIDColumn() string { return s.naturalIDCol }

// OrphanPolicyValue returns the shared base's orphan policy.
func (s *TableSchema) OrphanPolicyValue() OrphanPolicy { return s.orphanPolicy }

// SharedBaseRef returns the role's shared base + ParentID column, and whether the role
// declares one.
func (s *TableSchema) SharedBaseRef() (base *TableSchema, parentIDColumn string, ok bool) {
	if s == nil || s.sharedBaseLink == nil {
		return nil, "", false
	}
	return s.sharedBaseLink.base, s.sharedBaseLink.parentIDColumn, true
}

// ReferencingRoles returns, for a shared base, the role tables that reference it
// (empty for a non-base or an unreferenced base), each with its ParentID column and its
// DeletedAt column resolved LAZILY from the role schema — correct regardless of
// .DeletedAt vs .SharedBase declaration order. The refcount delete + lifecycle
// convergence walk it.
func (s *TableSchema) ReferencingRoles() []RoleRef {
	if s == nil || len(s.referencingRoleLinks) == 0 {
		return nil
	}
	out := make([]RoleRef, 0, len(s.referencingRoleLinks))
	for _, l := range s.referencingRoleLinks {
		out = append(out, RoleRef{Table: l.schema.table, ParentIDColumn: l.parentIDColumn, DeletedAtCol: l.schema.deletedAt})
	}
	return out
}

// SharedBaseScanPlan returns the base's read scan plan resolved against the
// role's Go type: the base columns and the column → role-field-index map, so the
// loader scans the shared columns straight into the role struct. ok=false when
// the role declares no shared base.
func (s *TableSchema) SharedBaseScanPlan() (cols []string, byCol map[string]FieldPath, ok bool) {
	if s == nil || s.sharedBaseLink == nil {
		return nil, nil, false
	}
	return s.sharedBaseLink.scanCols, s.sharedBaseLink.scanByCol, true
}

// AssertSharedBaseEquivalent asserts that two NewSharedBaseSchema declarations of the
// SAME table describe the SAME shape — ID, natural key, orphan policy,
// DeletedAt, field set, and native children. The engine registry accepts a
// base declared once and referenced everywhere OR re-declared identically per
// role file (no singleton required of the consumer); what it must refuse is two
// DIVERGENT declarations of one physical table, where the refcount/lifecycle
// semantics would depend on which instance a write happened to run through.
// Panics at registration (service boot), never on a request.
func AssertSharedBaseEquivalent(a, b *TableSchema) {
	if a.table != b.table {
		panic(fmt.Sprintf(
			"infra.SharedBase: equivalence check across different tables (%q vs %q) — a bug in the caller.",
			a.table, b.table))
	}
	diverges := func(what, va, vb string) {
		panic(fmt.Sprintf(
			"infra.SharedBase(%s): two NewSharedBaseSchema declarations of this table diverge on %s (%q vs %q). "+
				"Every declaration of a shared base must be identical — declare it once and reference it from "+
				"each role, or repeat the exact same declaration.", a.table, what, va, vb))
	}
	if a.idColumn != b.idColumn {
		diverges("the ID column", a.idColumn, b.idColumn)
	}
	if a.naturalIDCol != b.naturalIDCol {
		diverges("the NaturalID column", a.naturalIDCol, b.naturalIDCol)
	}
	if a.orphanPolicy != b.orphanPolicy {
		diverges("the OrphanPolicy", fmt.Sprintf("%d", a.orphanPolicy), fmt.Sprintf("%d", b.orphanPolicy))
	}
	if a.deletedAt != b.deletedAt {
		diverges("the DeletedAt column", a.deletedAt, b.deletedAt)
	}
	if a.revisionCol != b.revisionCol {
		diverges("the Revision column", a.revisionCol, b.revisionCol)
	}
	if len(a.fields) != len(b.fields) {
		diverges("the field count", fmt.Sprintf("%d", len(a.fields)), fmt.Sprintf("%d", len(b.fields)))
	}
	for _, f := range a.fields {
		other, ok := b.byGo[f.goName]
		if !ok || other.column != f.column {
			diverges("field "+f.goName, f.column, other.column)
		}
		// A part of a composite value object carries its provenance (which value
		// object, which field inside it): two bases mapping the same column to
		// DIFFERENT value objects would resolve to different paths on the role.
		if f.voType != other.voType || f.voFieldName != other.voFieldName {
			diverges("the value object behind field "+f.goName,
				compositeOrigin(f), compositeOrigin(other))
		}
	}
	if len(a.children) != len(b.children) {
		diverges("the native-children count", fmt.Sprintf("%d", len(a.children)), fmt.Sprintf("%d", len(b.children)))
	}
	for name, ac := range a.children {
		bc, ok := b.children[name]
		if !ok {
			diverges("native child "+name, ac.table, "<absent>")
		}
		if ac.table != bc.table || ac.parentIDColumn != bc.parentIDColumn || ac.deletedAt != bc.deletedAt {
			diverges("native child "+name,
				ac.table+"/"+ac.parentIDColumn+"/"+ac.deletedAt, bc.table+"/"+bc.parentIDColumn+"/"+bc.deletedAt)
		}
	}
}
