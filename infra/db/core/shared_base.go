package core

import "fmt"

// SharedBase (Modelagem 2 / Party-Role) — an identity table shared by multiple
// ROLE schemas. Unlike a Sibling (1:1 private, shared PK), a shared base is
// referenced by N independent roles (aluno, professor, usuario) via a foreign
// key, deduplicated by a NATURAL KEY whose value derives the base's
// deterministic id (UUIDv5). The base has NO lifecycle of its own: roles control
// their own soft-delete, and the base is governed by reference counting per its
// OrphanPolicy. Declared once with NewSharedBase and referenced from each role
// with .SharedBase(base, fkColumn) — the SAME instance referenced by every role
// IS the cross-schema registry.

// OrphanPolicy governs a shared base row when no role references it anymore
// (every referencing role row — of any type, active or archived — is gone).
type OrphanPolicy int

const (
	// DeleteWhenUnreferenced hard-deletes the base row once the last referencing
	// role row is gone (the default — the base exists only to serve its roles).
	DeleteWhenUnreferenced OrphanPolicy = iota
	// KeepOrphan leaves the base row in place even with no role referencing it.
	KeepOrphan
)

// RoleRef names a role table that references a shared base + the FK column it
// links through + the role's own soft-delete column ("" when the role has none).
// A shared base accumulates one per referencing role (the instance is the
// cross-schema registry). The unified lifecycle uses SoftDeleteCol to tell an
// ACTIVE role row apart from an archived one when it decides whether the base
// (driven by its roles) should be active or archived.
type RoleRef struct {
	Table         string
	FKColumn      string
	SoftDeleteCol string
}

// roleLink is, on a shared base, one referencing role: a pointer to the role
// schema (so its soft-delete column reads lazily, after the schema is fully
// assembled) + the FK column the role links through to the base's PK.
type roleLink struct {
	schema   *TableSchema
	fkColumn string
}

// sharedBaseLink is a role schema's reference to its shared base: the base
// schema + the role's FK column pointing to the base's PK, plus the pre-resolved
// scan plan (base column → field index in the ROLE's Go type) so the loader can
// scan the shared columns straight into the role struct.
type sharedBaseLink struct {
	base      *TableSchema
	fkColumn  string
	scanCols  []string       // base columns, in declaration order
	scanByCol map[string]int // base column → field index in the role's Go type
}

// NewSharedBase starts a shared-base schema for table. It is TYPE-LESS like
// NewExternalSchema (its fields are validated against each referencing role's Go
// type at .SharedBase time, since the same base is shared across several roles).
// Declare its columns with Field, the dedup/identity column with NaturalKey, and
// the lifecycle with OrphanPolicy.
func NewSharedBase(table string) *TableSchema {
	s := newSchema(table)
	s.isSharedBase = true
	return s
}

// NaturalKey declares the shared base's natural-key column — the immutable
// business key that deduplicates the identity and derives its deterministic id.
// SharedBase only; the column must be a declared Field of the base.
func (s *TableSchema) NaturalKey(column string) *TableSchema {
	if !s.isSharedBase {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): NaturalKey applies only to a SharedBase (NewSharedBase).", s.table))
	}
	if column == "" {
		panic(fmt.Sprintf("infra.TableSchema(%s): NaturalKey requires a non-empty column.", s.table))
	}
	s.naturalKeyCol = column
	return s
}

// OrphanPolicy sets the shared base's reference-count lifecycle. SharedBase only.
func (s *TableSchema) OrphanPolicy(p OrphanPolicy) *TableSchema {
	if !s.isSharedBase {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): OrphanPolicy applies only to a SharedBase (NewSharedBase).", s.table))
	}
	s.orphanPolicy = p
	return s
}

// SharedBase attaches a shared base to this role schema: the base identity plus
// the role's FK column referencing the base's deterministic id. A role
// references AT MOST ONE shared base (more than one would mean multiple
// candidate natural keys — a domain concern, not infra). The base's fields are
// validated against THIS role's Go type (the base is type-less; each role
// supplies the type to validate the shared columns against).
func (s *TableSchema) SharedBase(base *TableSchema, fkColumn string) *TableSchema {
	if s.secondary || s.isSharedBase {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): only a root/role schema may reference a SharedBase.", s.table))
	}
	if base == nil || !base.isSharedBase {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): SharedBase(...) expects a NewSharedBase(...); got a non-shared-base schema.",
			s.table))
	}
	if s.sharedBaseLink != nil {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): a role may reference at most ONE shared base — multiple shared bases mean "+
				"multiple candidate natural keys, a domain concern. Model it in the domain or a manual handler.",
			s.table))
	}
	if fkColumn == "" {
		panic(fmt.Sprintf("infra.TableSchema(%s): SharedBase requires a non-empty FK column.", s.table))
	}
	if !base.hasPKDeclared() {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): shared base %q declares no PK — declare .PK(column) for the id column "+
				"(the deterministic UUIDv5 lands there; the role FK references it).", s.table, base.table))
	}
	if base.naturalKeyCol == "" {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): shared base %q declares no NaturalKey — it is the source of the deterministic "+
				"id and of de-duplication.", s.table, base.table))
	}
	if _, ok := base.byCol[base.naturalKeyCol]; !ok {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): shared base %q NaturalKey(%q) is not a declared field of the base.",
			s.table, base.table, base.naturalKeyCol))
	}
	if len(base.fields) == 0 {
		panic(fmt.Sprintf("infra.TableSchema(%s): shared base %q declares no fields.", s.table, base.table))
	}
	if _, dup := s.byCol[fkColumn]; dup {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): the shared-base link column %q must not also be a declared field — it is the "+
				"FK to the base.", s.table, fkColumn))
	}
	// The base is type-less; validate its shared field Go-names exist on THIS
	// role's Go type (the role anchors the type the shared columns map into) and
	// capture the resolved field indices for the read-side scan.
	scanCols := make([]string, 0, len(base.fields))
	scanByCol := make(map[string]int, len(base.fields))
	if s.typ != nil {
		for _, f := range base.fields {
			idx := exportedFieldIndex(s.typ, f.goName)
			if idx < 0 {
				panic(fmt.Sprintf(
					"infra.TableSchema(%s): shared base %q field %q is not an exported field of %s — a role must "+
						"carry every shared-base field.", s.table, base.table, f.goName, s.typ.Name()))
			}
			scanCols = append(scanCols, f.column)
			scanByCol[f.column] = idx
		}
	}
	s.sharedBaseLink = &sharedBaseLink{base: base, fkColumn: fkColumn, scanCols: scanCols, scanByCol: scanByCol}
	// Register this role on the base — the shared instance is the cross-schema
	// registry the refcount delete + CDC fan-out + lifecycle convergence enumerate.
	// Store the schema pointer (not a snapshot) so the role's soft-delete column is
	// read lazily, order-independent of .SoftDelete vs .SharedBase.
	base.referencingRoleLinks = append(base.referencingRoleLinks, roleLink{schema: s, fkColumn: fkColumn})
	return s
}

// --- accessors ---------------------------------------------------------------

// IsSharedBase reports whether this schema is a shared base (NewSharedBase).
func (s *TableSchema) IsSharedBase() bool { return s != nil && s.isSharedBase }

// NaturalKeyColumn returns the shared base's natural-key column ("" when not a
// shared base or undeclared).
func (s *TableSchema) NaturalKeyColumn() string { return s.naturalKeyCol }

// OrphanPolicyValue returns the shared base's orphan policy.
func (s *TableSchema) OrphanPolicyValue() OrphanPolicy { return s.orphanPolicy }

// SharedBaseRef returns the role's shared base + FK column, and whether the role
// declares one.
func (s *TableSchema) SharedBaseRef() (base *TableSchema, fkColumn string, ok bool) {
	if s == nil || s.sharedBaseLink == nil {
		return nil, "", false
	}
	return s.sharedBaseLink.base, s.sharedBaseLink.fkColumn, true
}

// ReferencingRoles returns, for a shared base, the role tables that reference it
// (empty for a non-base or an unreferenced base), each with its FK column and its
// soft-delete column resolved LAZILY from the role schema — correct regardless of
// .SoftDelete vs .SharedBase declaration order. The refcount delete + lifecycle
// convergence walk it.
func (s *TableSchema) ReferencingRoles() []RoleRef {
	if s == nil || len(s.referencingRoleLinks) == 0 {
		return nil
	}
	out := make([]RoleRef, 0, len(s.referencingRoleLinks))
	for _, l := range s.referencingRoleLinks {
		out = append(out, RoleRef{Table: l.schema.table, FKColumn: l.fkColumn, SoftDeleteCol: l.schema.softDelete})
	}
	return out
}

// SharedBaseScanPlan returns the base's read scan plan resolved against the
// role's Go type: the base columns and the column → role-field-index map, so the
// loader scans the shared columns straight into the role struct. ok=false when
// the role declares no shared base.
func (s *TableSchema) SharedBaseScanPlan() (cols []string, byCol map[string]int, ok bool) {
	if s == nil || s.sharedBaseLink == nil {
		return nil, nil, false
	}
	return s.sharedBaseLink.scanCols, s.sharedBaseLink.scanByCol, true
}
