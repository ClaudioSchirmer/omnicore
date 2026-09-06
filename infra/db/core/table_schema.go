package core

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"time"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// TableSchema is the explicit, complete Go-field ↔ physical-column map for one
// table (a PG table and/or the source of a Mongo view). The framework does NOT
// guess: there is no PascalToSnake/PluralizeSnake column inference and no
// `transient` tag — every persisted field is declared here, Go field on one
// side, physical column on the other. The same TableSchema is attached to the
// repository (NewBaseAggregateRepository.WithSchema) AND to the read-side
// View (View.Schema), so write, criteria, scan, compose and read all translate
// from one declaration. Because the map is complete, both directions are
// lossless by construction — column → Go is a trivial inversion, no acronym
// ambiguity, no reflection at read time.
//
// Type-anchored: NewTableSchema[T] binds the schema to the Go type so the boot
// validates every declared field exists on T and resolves its field index once.
// External view sources (query.JoinUpstream over a type-less NewExternalSchema,
// whose physical columns belong to an upstream service) carry no Go struct.
type TableSchema struct {
	table string
	typ   reflect.Type // nil for external (type-less) schemas

	idGo     string
	idColumn string
	idIndex  int // reflect field index of the Go ID field; -1 when ID is not an
	// exported struct field (the aggregate root carries id privately in
	// BaseEntity and exposes it via GetID/SetID).

	parentIDColumn string // child only — ParentID column referencing the root

	fields []schemaField // non-ID persisted fields, in declaration order
	byGo   map[string]schemaField
	byCol  map[string]schemaField

	deletedAt string // "" = disabled
	createdAt string // "" = disabled (not stamped on insert)
	updatedAt string // "" = disabled (not stamped on insert/update)

	children map[string]*TableSchema // aggregate child schemas, keyed by Go type name

	// direct marks a schema built with NewDirectSchema: ONE table with no
	// aggregate and no domain entity behind it — a control table the service
	// maintains by hand, or an aggregate's child addressed as a plain table to
	// take a fact from it. It is what a DirectRepository anchors on, and it is
	// refused everywhere vertical composition is declared (Child, Sibling,
	// SharedBase), because assembling several tables into one write is the
	// aggregate's job and this shape deliberately does not do it.
	direct bool

	// secondary marks a sibling schema (built with NewSiblingSchema): a private
	// secondary table that shares its owner's primary key (1:1), carries a
	// disjoint subset of the SAME Go type's fields, and has no lifecycle of its
	// own (the owner controls archive/delete). Attached via .Sibling(...); never
	// a repository root.
	secondary bool

	// siblings are this node's declared sibling tables (root or aggregate child),
	// in declaration order. Each shares this node's ID; their fields partition the
	// entity's columns across tables. Width is unlimited; siblings do not nest.
	siblings []*TableSchema

	// M2 SharedBase (NewSharedBaseSchema) — a type-less identity table shared by
	// multiple role schemas, deduplicated by a natural key whose value derives the
	// base's deterministic id. isSharedBase marks the base; naturalIDCol is its
	// dedup/identity column; orphanPolicy governs the base's lifecycle when no
	// role references it. A role schema references its base via sharedBaseLink
	// (set by .SharedBase(base, fk)) — at most one per role.
	isSharedBase bool
	naturalIDCol string
	orphanPolicy OrphanPolicy
	// revisionCol is, on a SHARED BASE, the framework-managed BIGINT column that
	// versions the identity row: every write that touches the base row
	// increments it UNDER THE BASE ROW LOCK, so concurrent role writes serialize
	// in real relational commit order and the value is a deterministic
	// last-writer-wins token for every read-model write of base data. Mandatory
	// on a shared base (enforced at .SharedBase time); meaningless elsewhere.
	revisionCol    string
	sharedBaseLink *sharedBaseLink
	// composites are the value-object decompositions attached to this schema, in
	// declaration order (see composite_vo.go). Each is built whole by
	// NewCompositeValueObject and handed to Composite(...), so the schema keeps
	// no builder state of its own — what it stores is the finished mapping the
	// once-rule checkpoint walks.
	composites []*compositeDecl

	// referencingRoleLinks is, on a SHARED BASE, the set of roles that reference
	// it — each a pointer to the role schema + the ParentID column it links through
	// (populated as each role calls .SharedBase — the instance IS the cross-schema
	// registry). The role's DeletedAt column is read LAZILY from the schema
	// pointer (via ReferencingRoles), so it is correct regardless of whether
	// .DeletedAt was declared before or after .SharedBase. The refcount delete +
	// CDC fan-out + lifecycle convergence enumerate it.
	referencingRoleLinks []roleLink
}

type schemaField struct {
	goName string
	column string
	// path addresses the Go field: a one-element chain for a field declared at
	// the entity's root, a longer one for a part of a composite value object
	// (Composite). Empty = not a struct field (a type-less schema's
	// column, or a shared-base column before a role resolves it).
	path FieldPath

	// Composite-value-object provenance, set only on a PART (zero for a plain
	// field). voType is the value object the part belongs to; voFieldName is the
	// part's name INSIDE that type, which differs from goName whenever .As(...)
	// aliased the exposed name. The pair is what lets a type-less shared base
	// resolve its parts against each role's struct at .SharedBase(...) time,
	// where only the VO type and the in-VO name are meaningful.
	voType      reflect.Type
	voFieldName string

	// labelKey is the header catalog key declared inline on the schema. It is
	// EXTERNAL-ONLY (NewExternalSchema): a type-less schema has no Go struct to
	// carry a labelKey:"…" tag, so the "mini-domain" declares the label here.
	// On a type-anchored schema the struct tag is the single source (declaring
	// a schema-level label there is a boot panic — never two ways to express
	// one domain concept). "" when none.
	labelKey string

	// idKind is the field's identity typing, derived from the Go struct at
	// Field() time (never declared): a domain.ID field is IDValue, a *domain.ID
	// field is IDPointer, everything else IDNone. The TYPE is the declaration —
	// it tells the criteria translator which probes to lift into domain.ID so
	// they bind in the dialect's native id form (the scan side detects the same
	// types per target, see scanTargetFor).
	idKind IDKind

	// Value-object typing, derived from the Go struct at Field() time (the field
	// TYPE is the declaration, like idKind). isVO marks a value-object field
	// (raw or enum); isEnum distinguishes the enum kind; underlyingType is the
	// scalar the framework persists (from Value(): string behind an Email, int
	// behind a UserProfile). The write path unwraps to the underlying and the read
	// path reconstructs the VO; PayloadColumnTypes reports the underlying so the
	// Mongo read-side decoder coerces correctly. Zero-valued for a plain field.
	isVO           bool
	isEnum         bool
	underlyingType reflect.Type

	// inSync / inAudit are the per-axis redaction declared by RedactedField —
	// how this field appears in the copies the FRAMEWORK makes of the row (the
	// outbox payload and everything downstream of it; the audit event). Both are
	// the zero Redactor on a plain Field, which reads as "not declared" and makes
	// every redaction walk a no-op. The relational column and the hydrated entity
	// are never touched by either. See redaction.go.
	inSync  Redactor
	inAudit Redactor

	// stamped marks a field declared with StampedTimeField: the framework owns
	// its VALUE (the write operation's authoritative instant) and the domain owns
	// the WHEN (domain.Managed.Stamp / write.Stamp). writeFields never emits it,
	// so a write that did not ask for it leaves the column out of the statement
	// entirely and an already-stamped row keeps what it had.
	stamped bool

	// stampedCounter narrows stamped: the framework's value for this column is
	// the EXISTING row's value plus one, not the operation's instant. Same
	// contract otherwise — the caller asks, the framework decides.
	stampedCounter bool
}

// IDKind classifies a persisted field's identity typing, derived from its Go
// type by reflection (mirroring how BoolColumns infers bool columns — the type
// system carries the intent, nothing is declared).
type IDKind uint8

const (
	// IDNone — an ordinary field; its values bind exactly as the driver sees
	// them (a string is text, always).
	IDNone IDKind = iota
	// IDValue — a required identity field (domain.ID).
	IDValue
	// IDPointer — a nullable identity field (*domain.ID); nil ⇄ SQL NULL.
	IDPointer
)

// IDKindOf reports the identity typing of a Go field on this schema. The
// managed ID slot ("ID") is ALWAYS IDValue — the framework stores it in the
// dialect's native id form on every schema, so a bare-string ID probe (e.g.
// the exclude-self Ne("ID", id)) must bind like the typed ByID does. Unknown
// fields answer IDNone (the translator already rejects them separately).
func (s *TableSchema) IDKindOf(goField string) IDKind {
	if goField == idGoField {
		return IDValue
	}
	return s.byGo[goField].idKind
}

// NewTableSchema starts a type-anchored schema for table over Go type T. Field
// declarations are validated against T at call time (panic on a missing or
// unexported field) — that is the enforcement that replaces convention. The
// primary key column is NOT assumed: the developer must declare it via
// ID(col) (the Go side is fixed to the Entity contract's "ID").
func NewTableSchema[T any](table string) *TableSchema {
	t := reflect.TypeOf((*T)(nil)).Elem()
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		panic(fmt.Sprintf("infra.NewTableSchema: %s is not a struct", t))
	}
	s := newSchema(table)
	s.typ = t
	return s
}

// NewSiblingSchema starts a SIBLING schema for table over the SAME Go type T as
// its owner: a private secondary table that shares the owner's primary key (1:1)
// and carries a disjoint subset of T's fields. Like NewTableSchema[T] it
// validates every Field against T, but it declares NO primary key (it borrows
// the owner's), NO foreign key, and NO DeletedAt — the owner controls identity
// and lifecycle. Attach it with
// owner.Sibling(NewSiblingSchema[T](table).Field(...)).
func NewSiblingSchema[T any](table string) *TableSchema {
	s := NewTableSchema[T](table)
	s.secondary = true
	return s
}

// NewDirectSchema starts a DIRECT schema for table over Go type T: a table with
// no aggregate and no domain entity over it. It is the map a DirectRepository
// anchors on — a control table the service writes and queries by hand, or an
// aggregate's child addressed as a plain table to count or sum over it.
//
// The table is the WRITE unit and the READ anchor, and those are different
// reaches. A write is one statement against this table and nothing else. A read
// is anchored here and may traverse SIDEWAYS as far as the repository's declared
// joins go — the same horizontal reach an aggregate repository has. What is
// dropped is the DOWNWARD composition (children, 1:1 satellites, a shared
// identity), which is what an aggregate is and what this deliberately is not.
//
// It is the SAME *TableSchema every other path consumes, born from a different
// constructor: there is no conversion step and no second engine behind it. The
// criteria compiler, Resolve, ScanPlan, WriteFields and the statement builders
// are the ones the aggregate path uses.
//
// What it declares like any other schema: ID (mandatory — identity stays
// domain.ID as everywhere else), Field, and optionally DeletedAt / CreatedAt /
// UpdatedAt. What it must NOT declare — each panics at declaration, where the
// mistake was written:
//
//   - Child / Sibling / SharedBase — vertical composition belongs to the
//     aggregate, which persists the whole shape in one transaction with the
//     outbox, the audit trail and the revision guard this path deliberately has
//     none of.
//
// What it does not need: Revision (there is no optimistic guard without a
// loaded entity) and Modes() (there is no entity to declare them).
//
// The anchored type MUST expose an exported `ID domain.ID` field: a plain row
// has no SetID, so without it a row read back would carry no identity and could
// not be the target of the next write. It is scanned like any other column.
//
// Contrast with NewExternalSchema, the other constructor without an entity
// behind it: External names WHOSE the data is (an upstream service's columns,
// read-only, a Mongo view source). Direct names HOW the data is modelled (one
// table, no aggregate) and is read AND written, here, in this database.
func NewDirectSchema[T any](table string) *TableSchema {
	s := NewTableSchema[T](table)
	s.direct = true
	return s
}

// AnchoredType is the Go struct this schema was built over (NewTableSchema[T] /
// NewDirectSchema[T]), nil for a type-less one (NewExternalSchema, a shared base
// before a role resolves it). A repository cross-checks it against its own type
// parameter so one schema serves one row type.
func (s *TableSchema) AnchoredType() reflect.Type {
	if s == nil {
		return nil
	}
	return s.typ
}

// IsDirect reports whether this schema was built with NewDirectSchema — one
// table, no aggregate. A DirectRepository accepts nothing else as its anchor;
// a join TARGET is unconstrained, since a traversal only reads.
func (s *TableSchema) IsDirect() bool { return s != nil && s.direct }

// refuseVertical panics when a DIRECT schema is handed a vertical composition
// declaration. verb names the call, so the message points at the line that has
// to change rather than at the schema in the abstract.
func (s *TableSchema) refuseVertical(verb string) {
	if !s.direct {
		return
	}
	panic(fmt.Sprintf(
		"infra.NewDirectSchema(%s): %s composes tables DOWNWARD, and a Direct schema does not. "+
			"A root and its children are persisted together, in one transaction, with the outbox row, "+
			"the audit event and the revision guard that make an aggregate an aggregate; a Direct write "+
			"is one statement against this table and has none of those by design. (Reaching SIDEWAYS is "+
			"unaffected: declare read.InnerJoin/LeftJoin on the repository.) Declare the shape with "+
			"NewTableSchema and a repository over the entity, or map the other table as its own Direct schema.",
		s.table, verb,
	))
}

// NewExternalSchema starts a type-less schema for a Mongo view source whose
// physical columns belong to an upstream service (consumed via query.JoinUpstream). Field
// declarations carry logical Go names (consumed by the composite view's
// criteria/Response) mapped to the upstream's doc columns; there is no struct
// to validate against.
func NewExternalSchema(table string) *TableSchema {
	return newSchema(table)
}

func newSchema(table string) *TableSchema {
	return &TableSchema{
		table: table,
		// No ID default — idGo/idColumn stay empty until ID(...) is declared
		// (the developer must declare the column; the Go side is the Entity
		// contract's "ID", never guessed from the column name).
		idIndex:  -1,
		byGo:     map[string]schemaField{},
		byCol:    map[string]schemaField{},
		children: map[string]*TableSchema{},
	}
}

// hasPKDeclared reports whether ID(...) was explicitly declared. There is NO
// default primary key — a schema without an explicit ID is a boot failure at
// every consumer checkpoint (WithSchema, Child, ValidateViewSchemas).
func (s *TableSchema) hasPKDeclared() bool { return s.idColumn != "" }

// exportedFieldIndex returns the single-depth field index of an exported field
// named goName, or -1 when absent / unexported / promoted from an embed.
func exportedFieldIndex(t reflect.Type, goName string) int {
	f, ok := t.FieldByName(goName)
	if !ok || f.PkgPath != "" || len(f.Index) != 1 {
		return -1
	}
	return f.Index[0]
}

// ensureColumnFree panics when column is already claimed by the ID, a mapped
// field, or another managed column — enforcing the bijection over the full
// physical column set (ID + every Field + DeletedAt/created/updated)
// regardless of the order ID/Field/DeletedAt/CreatedAt/UpdatedAt are declared.
// `self` names the slot being (re)assigned so reassigning it to the same column
// is not flagged as a self-collision. Field() runs its own equivalent check at
// declaration time; this covers every managed setter + ID so a collision is a
// boot panic no matter which side is declared second.
func (s *TableSchema) ensureColumnFree(column, self string) {
	if column == "" {
		return
	}
	collide := func(role string) {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): column %q claimed by both %s and %s — the map must be a bijection",
			s.table, column, role, self,
		))
	}
	if self != "ID" && column == s.idColumn {
		collide("ID")
	}
	if _, dup := s.byCol[column]; dup {
		collide("a mapped field")
	}
	if self != "DeletedAt" && column == s.deletedAt {
		collide("DeletedAt")
	}
	if self != "CreatedAt" && column == s.createdAt {
		collide("CreatedAt")
	}
	if self != "UpdatedAt" && column == s.updatedAt {
		collide("UpdatedAt")
	}
	if self != "Revision" && column == s.revisionCol {
		collide("Revision")
	}
}

// mustNotRedeclare panics when a single-value slot is being set a SECOND time.
// ensureColumnFree deliberately skips a slot's OWN current value (so an
// idempotent same-column re-set is allowed) — which is exactly what lets a
// DIFFERENT column overwrite the slot unnoticed. Every single-column slot (ID,
// ParentID, DeletedAt, CreatedAt, UpdatedAt, Revision, NaturalID) is declared once.
func (s *TableSchema) mustNotRedeclare(current, name, newCol string) {
	if current != "" {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): %s already declared as %q — declare it once; drop the duplicate %s(%q).",
			s.table, name, current, name, newCol))
	}
}

// mustNotReservedColumn rejects a physical column name starting with "_": the
// underscore prefix is the framework's reserved namespace on the wire — the
// outbox payload carries its structural keys there (_ids, _children,
// _base_children, _op), and Mongo itself owns _id. A user column in that
// namespace would collide with the payload contract, so it is a boot failure at
// declaration, never a runtime surprise.
func mustNotReservedColumn(table, column string) {
	if len(column) > 0 && column[0] == '_' {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): column %q — physical column names starting with %q are reserved by the "+
				"framework (outbox payload structural keys such as _ids/_children); rename the column.",
			table, column, "_"))
	}
}

// idGoField is the fixed Go-side name of every primary key. Identity is locked
// by the domain.Entity/BaseEntity contract — an aggregate root carries it
// privately in BaseEntity and exposes it via GetID/SetID, while an
// AggregateValueObject exposes the exported field "ID" — so the Go side is
// never a free parameter. Only the physical column varies.
const idGoField = "ID"

// parentIDGoField is the fixed Go-side name under which a schema's foreign key is
// EXPOSED ON READ — the read-only twin of idGoField. The ParentID column is written by
// the aggregate / shared-base cascade (it is never a domain field), so it has no
// Go field on the write side; a view/DTO that wants the parent link projects it
// under this fixed logical name, symmetric with "ID". A schema carries at most
// one ParentID (the aggregate-child ParentID or the SharedBase role ParentID, never both — boot-
// guarded), so the name is unambiguous. Reserved: Field("ParentID", …) is a boot
// panic.
const parentIDGoField = "ParentID"

// ID declares the primary-key column — mandatory, no default. The Go side is
// fixed to the BaseEntity/AVO field "ID" (the Entity contract locks identity),
// so only the physical column is declared here — it is what varies (id,
// person_pk, an upstream schema's own name). Single-column. Empty column is
// rejected.
func (s *TableSchema) ID(column string) *TableSchema {
	if s.secondary {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): a sibling declares NO primary key — it BORROWS the owner's: "+
				"the framework writes and joins the sibling table through the owner's ID column name, "+
				"so the sibling's physical ID column must carry that SAME name (usually \"id\"), "+
				"never a custom one like \"<owner>_id\". Drop this ID(%q) call and name the column "+
				"after the owner's ID in the migration.",
			s.table, column,
		))
	}
	if column == "" {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): ID requires a non-empty column — "+
				"a single-column primary key is mandatory on every schema",
			s.table,
		))
	}
	s.mustNotRedeclare(s.idColumn, "ID", column)
	mustNotReservedColumn(s.table, column)
	s.ensureColumnFree(column, "ID")
	s.idGo = idGoField
	s.idColumn = column
	if s.typ != nil {
		s.idIndex = exportedFieldIndex(s.typ, idGoField)
	}
	if s.direct {
		s.requireIDField()
	}
	return s
}

// requireIDField enforces the Direct schema's one extra rule about the anchored
// type: an exported `ID domain.ID` field. An aggregate root carries its id
// privately in BaseEntity and the loader stamps it through SetID; a plain row
// has neither, so the id has to be a field like the others or a row read back
// would come out with no identity — unable to be the target of the next write,
// and silently so.
func (s *TableSchema) requireIDField() {
	fail := func(why string) {
		panic(fmt.Sprintf(
			"infra.NewDirectSchema(%s): %s must declare an exported field `ID domain.ID` — %s. "+
				"A Direct row has no SetID: the id is scanned like any other column, so without the "+
				"field a row read back carries no identity.",
			s.table, s.typ, why,
		))
	}
	if s.idIndex < 0 {
		fail("it has no exported ID field")
	}
	if got := s.typ.Field(s.idIndex).Type; got != reflect.TypeOf(domain.ID{}) {
		fail(fmt.Sprintf("its ID field is %s, not domain.ID", got))
	}
}

// ParentID declares the child's foreign-key column referencing the root. Child only.
func (s *TableSchema) ParentID(column string) *TableSchema {
	if s.secondary {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): a sibling declares NO foreign key — the shared ID IS the link: "+
				"the framework writes and joins the sibling table through the owner's ID column name, "+
				"so the sibling's physical ID column must carry that SAME name (usually \"id\"). "+
				"Drop this ParentID(%q) call.",
			s.table, column,
		))
	}
	// One parent, one ParentID: a second ParentID(...) would silently overwrite the first.
	s.mustNotRedeclare(s.parentIDColumn, "ParentID", column)
	// A schema is EITHER an aggregate child (ParentID to its root) OR a SharedBase role
	// (ParentID to its base) — never both. Two FKs would mean two parents (two candidate
	// links), and would make the read-only "ParentID" projection ambiguous. Reject the
	// combination at boot, in both declaration orders.
	if s.sharedBaseLink != nil {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): a schema cannot declare both ParentID(...) (aggregate child) and SharedBase(...) "+
				"(role) — it would have two parents. Model it as one or the other.", s.table))
	}
	mustNotReservedColumn(s.table, column)
	// The ParentID is written by the aggregate cascade (insertChild sets it to the
	// parent id), so it cannot ALSO be a mapped domain field — that field would be
	// silently overwritten on every write. Reject the reverse declaration order
	// too (Field first, then ParentID). A ID that doubles as the ParentID is fine: the ID is
	// never in byCol, so this check does not fire for the shared-ID model.
	if _, dup := s.byCol[column]; dup {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): ParentID column %q is already a mapped field — the ParentID is written by the aggregate "+
				"cascade (set to the parent id) and cannot also be a domain field; drop the Field mapping.",
			s.table, column))
	}
	s.parentIDColumn = column
	return s
}

// Field declares one persisted Go field ↔ column pair. The field must exist and
// be exported on T (panic otherwise); the column must be unique within the
// schema (bijection).
//
// The optional trailing labelKey declares the field's header catalog key inline
// on the schema. It is EXTERNAL-ONLY: passing a non-empty labelKey on a
// type-anchored schema (NewTableSchema[T]) is a boot panic, because that schema
// already declares the label via the field's labelKey:"…" struct tag — there is
// never two ways to express one domain concept. A type-less external schema
// (NewExternalSchema) has no struct to read, so the schema-level labelKey is the
// only place it can live (the "mini-domain" for upstream columns). At most one
// labelKey may be passed.
func (s *TableSchema) Field(goName, column string, labelKey ...string) *TableSchema {
	lk := ""
	switch len(labelKey) {
	case 0:
	case 1:
		lk = labelKey[0]
	default:
		panic(fmt.Sprintf("infra.TableSchema(%s): field %q accepts at most one labelKey", s.table, goName))
	}
	return s.declareField(goName, column, redactedFieldSpec{labelKey: lk})
}

// StampedTimeField declares a timestamp column whose WHEN belongs to the caller
// and whose VALUE belongs to the framework. This method is the WHAT; who asks
// for the stamp, and how, depends on which kind of schema it was declared on.
//
// ON AN ENTITY SCHEMA (NewTableSchema) the domain asks, because the entity is
// the only thing an entity write takes:
//
//	// infra
//	core.NewTableSchema[*Order]("orders").
//	    ID("id").
//	    Field("Status", "status").
//	    StampedTimeField("PaidAt", "paid_at")
//
//	// domain — the rule that knows the moment (Stamp is promoted from
//	// domain.Managed, which BaseEntity embeds)
//	type Order struct {
//	    domain.BaseEntity
//	    Status string
//	    PaidAt *time.Time      // never assigned by hand
//	}
//
//	func (o *Order) Pay() {
//	    o.Status = "PAID"
//	    o.Stamp("PaidAt")      // ask; do not assign
//	}
//
//	// application — an ordinary write, nothing extra to remember
//	order.Pay()
//	res, err := repo.Update(ctx, order)   // order.PaidAt now holds the row's value
//
// ON A DIRECT SCHEMA (NewDirectSchema) the CALLER asks, because a Direct write
// has no entity — its only channel is the Values map, so the request rides there
// as a marker instead of a value:
//
//	// infra
//	core.NewDirectSchema[JobRow]("job_queue").
//	    ID("id").
//	    Field("Status", "status").
//	    StampedTimeField("StartedAt", "started_at")
//
//	// application — the WHEN is the call site's; the value is still not its own
//	_, err := jobs.Update(ctx, write.Values{
//	    "Status":    "running",
//	    "StartedAt": write.Stamp,
//	}, criteria.Where(criteria.Eq("Status", "pending")))
//
// Same verb, same guarantees, two channels — each path asks through the only
// input it has. Either way the field is addressed by GO FIELD NAME, never by
// column, and one write dates every row it touches with one instant.
//
// This is the seat CreatedAt/UpdatedAt do not cover. Those date the ROW —
// written, last touched — on a schedule the framework fixes. A stamped field
// dates a FACT the business decides has just happened: signed, paid, approved,
// cancelled. Nothing but a rule knows when that is, so the framework cannot
// schedule it; and nothing but the framework should choose the instant, because
// a value read from the writing process is a per-replica clock reading that
// drifts (see relational.clock).
//
// So the column is NEVER written from the struct. On a write that did not
// request it the column is left out of the statement entirely — not bound, not
// set, not nulled — which is also why an already-stamped row keeps its value
// with nobody having to remember to preserve it. A requested one binds the write
// operation's single instant, the same value created_at/updated_at carry, so
// every statement of the operation agrees and the audit event, the outbox
// payload and the response carry the same timestamp without a read-back.
//
// The Go field is *time.Time: until a rule stamps it, the fact has not happened,
// and nil says exactly that where a zero time would lie. Everything else about
// the field is ordinary — it maps into the bijection like any Field, and it
// filters, orders, projects and hydrates the same way.
//
// Refused at declaration on a schema that cannot honor it: a sibling (it carries
// no managed columns of its own — the owner dates the row) and an external
// schema (it never writes). A Stamp naming a field this schema did not declare
// stamped is an error raised by the write, not a panic: the domain cannot see
// the schema, so there is no boot moment at which to catch it.
func (s *TableSchema) StampedTimeField(goName, column string) *TableSchema {
	if s.secondary {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): a sibling declares NO stamped fields — the sibling row is a 1:1 slice "+
				"of the OWNER's row and carries no managed columns of its own. Declare StampedTimeField(%q, %q) "+
				"on the owner.",
			s.table, goName, column,
		))
	}
	if s.typ == nil && !s.isSharedBase {
		panic(fmt.Sprintf(
			"infra.NewExternalSchema(%s): StampedTimeField(%q, %q) is refused — an external schema maps an "+
				"UPSTREAM service's columns for the read side and never writes, so there is no write to stamp on.",
			s.table, goName, column,
		))
	}
	// Declare it as an ordinary field first — every name/column guard, the type
	// resolution against the anchored struct and the bijection claim are exactly
	// the ones a Field passes — then mark the declaration on the three views the
	// schema keeps of it.
	s.declareField(goName, column, redactedFieldSpec{})
	fd := s.fields[len(s.fields)-1]
	// A SHARED BASE is type-less, so there is no struct to check the declaration
	// against yet. Its Go names resolve against each ROLE's struct at
	// .SharedBase(...) time — which is where the type check runs for a base,
	// exactly as it already does for the base's other fields.
	if s.typ != nil {
		mustStampedTimeType(s.table, goName, s.typ.Field(fd.path[0]).Type)
	}
	fd.stamped = true
	s.fields[len(s.fields)-1] = fd
	s.byGo[fd.goName] = fd
	s.byCol[fd.column] = fd
	return s
}

// StampedCounterField declares a per-row counter the framework increments. It is
// the second member of the stamped family, and the split from StampedTimeField
// is the shape of the value, not the mechanism: the caller asks with the same
// marker (domain.Managed.Stamp / write.Stamp), the schema decides what filling
// the column MEANS.
//
//	StampedTimeField("LastAt", "last_at")            // fills with the instant
//	StampedCounterField("TotalCount", "total_count") // fills with existing + 1
//
// PER ROW, not per table. `total_count` on one row counts that row's own events;
// another row counts its own, and the two never interact. That is what makes it
// portable on every engine (`col = col + 1`, evaluated under the row's lock) and
// what makes it NOT a sequence: a sequence is a shared generator handing out
// values unique across the whole table (an invoice number), which needs a
// SEQUENCE object that MySQL and SQLite do not have. Two different problems, two
// different names.
//
// The Go field is int64 — a counter always has a value, starting at 1 on the row
// that creates it, so there is nothing a pointer would say. On an INSERT the
// column binds 1; on an UPDATE and on an upsert conflict it becomes
// `col = col + 1`, computed by the server so two concurrent writers cannot read
// the same value and both write back the same increment.
//
// Refused exactly where StampedTimeField is: an external schema never writes.
func (s *TableSchema) StampedCounterField(goName, column string) *TableSchema {
	if s.secondary {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): a sibling declares NO stamped fields — the sibling row is a 1:1 slice "+
				"of the OWNER's row and carries no framework-owned columns of its own. Declare "+
				"StampedCounterField(%q, %q) on the owner.",
			s.table, goName, column,
		))
	}
	if s.typ == nil && !s.isSharedBase {
		panic(fmt.Sprintf(
			"infra.NewExternalSchema(%s): StampedCounterField(%q, %q) is refused — an external schema maps an "+
				"UPSTREAM service's columns for the read side and never writes, so there is no write to count on.",
			s.table, goName, column,
		))
	}
	s.declareField(goName, column, redactedFieldSpec{})
	fd := s.fields[len(s.fields)-1]
	if s.typ != nil {
		mustStampedCounterType(s.table, goName, s.typ.Field(fd.path[0]).Type)
	}
	fd.stamped, fd.stampedCounter = true, true
	s.fields[len(s.fields)-1] = fd
	s.byGo[fd.goName] = fd
	s.byCol[fd.column] = fd
	return s
}

// stampedCounterType is what a counter must be declared as. Unlike a stamped
// time it is NOT a pointer: a counter always has a value — a row that was just
// created has counted one thing — so there is no absence for nil to describe.
var stampedCounterType = reflect.TypeOf(int64(0))

func mustStampedCounterType(table, goName string, ft reflect.Type) {
	if ft == stampedCounterType || ft == nullableStampedCounterType {
		return
	}
	panic(fmt.Sprintf(
		"infra.TableSchema(%s): stamped counter %q is typed %s — a counter is declared int64, or *int64 "+
			"when it must also be able to say \"no count at all\". int64 is the width the framework "+
			"increments on every engine; the pointer form adds nothing to the increment and only widens what "+
			"the field can hold, which is what StampNull needs (a plain int64 has no absence to write).",
		table, goName, ft,
	))
}

// nullableStampedCounterType is the counter form that can hold an ABSENCE. The
// increment is identical on both — `col = col + 1` is the server's, not the
// field's — so the pointer buys exactly one thing: somewhere for StampNull to
// land. A schema that never clears its counter keeps declaring int64 and reads
// the same as it always did.
var nullableStampedCounterType = reflect.TypeOf((*int64)(nil))

// StampedFieldAcceptsNull reports whether the field can hold the absence
// StampNull writes: a stamped time always can (*time.Time), a stamped counter
// only when declared *int64. It is what the write path consults before it lets a
// request become `col = NULL`, so a plain int64 is refused with a message rather
// than scanned into on the next read.
//
// A type-less schema (a shared base before its roles bind) answers true: there
// is no struct to disprove it against, and the role's own schema is where the
// field is checked.
func (s *TableSchema) StampedFieldAcceptsNull(goName string) bool {
	f, ok := s.byGo[goName]
	if !ok || !f.stamped {
		return false
	}
	if !f.stampedCounter {
		return true // a stamped time is *time.Time by declaration
	}
	if s.typ == nil {
		return true
	}
	return s.typ.Field(f.path[0]).Type == nullableStampedCounterType
}

// IsStampedCounter reports whether goName is a stamped COUNTER (as opposed to a
// stamped time) — what a write path asks to decide between binding the
// operation's instant and emitting the server-side increment.
func (s *TableSchema) IsStampedCounter(goName string) bool {
	f, ok := s.byGo[goName]
	return ok && f.stampedCounter
}

// StampedCounterColumns splits the columns StampColumns resolved into the two
// kinds, preserving declaration order: the ones filled with the operation's
// instant, and the ones filled with the existing value plus one.
func (s *TableSchema) StampedCounterColumns(cols []string) (times, counters []string) {
	for _, c := range cols {
		if f, ok := s.byCol[c]; ok && f.stampedCounter {
			counters = append(counters, c)
			continue
		}
		times = append(times, c)
	}
	return times, counters
}

// stampedTimeType is what a stamped field must be declared as. The pointer is
// not a nullability preference — it is the semantics: before the fact happens
// the column has no value, and a zero time.Time would report midnight of year 1
// as the moment something was signed.
var stampedTimeType = reflect.TypeOf((*time.Time)(nil))

func mustStampedTimeType(table, goName string, ft reflect.Type) {
	if ft == stampedTimeType {
		return
	}
	panic(fmt.Sprintf(
		"infra.TableSchema(%s): stamped field %q is typed %s — a stamped field is declared *time.Time. "+
			"It records WHEN a fact happened, and until the domain stamps it the fact has not happened: nil "+
			"says that, where a zero time.Time would report year 1 as the moment. Declare the column nullable.",
		table, goName, ft,
	))
}

// RedactedField declares a persisted field whose real value stays in the
// relational column and in the hydrated entity, but appears REDACTED in the
// copies the framework makes of the row. Same mapping contract as Field — the
// Go field must exist on the anchored type, the column joins the bijection — and
// the same panics guard both.
//
// Both axes are mandatory (see redaction.go for what each one reaches):
//
//	RedactedField("BankAccount", "bank_account",
//	    core.InSync(core.RedactKeepLast(4)),
//	    core.InAudit(core.RedactWith("***")),
//	)
//
// A missing axis is a construction panic naming it. The framework will not pick
// a redaction policy the developer did not write: silence would have to default
// either to leaking (Plain) or to guessing, and neither is the framework's call.
//
// Nothing on the READ side is refused as a consequence of this declaration.
// Filters, ordering, `?search` and the aggregate DSL keep working exactly as
// they do for any other field — per-identity field authorization is the
// developer's, through ReadCriteria.Restrict inside ToCriteria.
func (s *TableSchema) RedactedField(goName, column string, opts ...RedactedFieldOption) *TableSchema {
	var spec redactedFieldSpec
	for _, opt := range opts {
		if opt != nil {
			opt(&spec)
		}
	}
	if !spec.inSync.declared() {
		panic(missingAxisPanic(s.table, goName, "InSync", "the outbox payload — and with it the topic, every "+
			"consuming service, the failure ledgers and the projected document"))
	}
	if !spec.inAudit.declared() {
		panic(missingAxisPanic(s.table, goName, "InAudit", "the audit event — the audit_events row, the slog "+
			"echo and the /audit endpoint"))
	}
	return s.declareField(goName, column, spec)
}

// missingAxisPanic builds the diagnostic for a RedactedField that left one axis
// undeclared, naming what that axis governs so the fix is obvious.
func missingAxisPanic(table, goName, axis, governs string) string {
	return fmt.Sprintf(
		"infra.TableSchema(%s): RedactedField %q does not declare core.%s(...) — both axes are mandatory. "+
			"%s carries %s; the framework does not choose a redaction policy on your behalf. Declare "+
			"core.%s(core.Plain()) to keep the real value there.",
		table, goName, axis, axis, governs, axis)
}

// declareField is the single field-declaration implementation behind Field and
// RedactedField: the reserved-name guards, the type resolution against the
// anchored struct, the bijection claim, the labelKey rule and the storage
// typing — plus, for a redacted declaration, validating each axis against the
// field's effective scalar type.
func (s *TableSchema) declareField(goName, column string, spec redactedFieldSpec) *TableSchema {
	// "ID" and "ParentID" are reserved logical Go names. "ID" is the primary key
	// (declare it with ID(column)); mapping it as a Field would double-map the
	// identity (the ID already answers ColumnOf/GoOf("ID")). "ParentID" is the
	// read-only foreign-key projection (exposed automatically when the schema
	// declares an ParentID) — it is never a domain field.
	if goName == idGoField {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): %q is the reserved primary-key Go field — declare the ID with ID(column), "+
				"never as a Field.", s.table, idGoField))
	}
	if goName == parentIDGoField {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): %q is the reserved read-only foreign-key projection — it is exposed "+
				"automatically when the schema declares an ParentID and is never mapped as a Field.", s.table, parentIDGoField))
	}
	// A Field declaration ends any open DecomposeValueObject scope: Part(...) and
	// As(...) are the only calls that continue one.
	idx := -1
	if s.typ != nil {
		idx = exportedFieldIndex(s.typ, goName)
		if idx < 0 {
			panic(fmt.Sprintf(
				"infra.TableSchema(%s): %q is not an exported single-depth field of %s — only real persisted fields can be mapped",
				s.table, goName, s.typ.Name(),
			))
		}
		// A COMPOSITE value object spans more than one column, so it cannot be a
		// Field — teach the fix instead of failing later on the closed-set check
		// with a message about an unsupported type.
		mustNotCompositeField(s.table, goName, s.typ.Field(idx).Type)
	}
	s.mustClaimNames(goName, column, "field")
	// A redacted field may occupy any column a plain Field may — the framework's
	// own slots (ID, ParentID, Revision, DeletedAt, CreatedAt, UpdatedAt) are
	// already refused for ANY field, in both declaration orders. The natural key
	// is the single exception that needs its own check, because it is required to
	// be a mapped field and therefore passes the claim above.
	if spec.inSync.declared() || spec.inAudit.declared() {
		s.mustNotRedactNaturalKey(column, goName)
	}
	if spec.labelKey != "" && s.typ != nil {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): schema-level labelKey on field %q is external-only; a type-anchored schema declares the header via the `labelKey:\"…\"` struct tag, not here",
			s.table, goName,
		))
	}
	fd := schemaField{goName: goName, column: column, labelKey: spec.labelKey}
	// The redaction axes are validated against the field's EFFECTIVE scalar —
	// the type behind a value object and behind a nullable pointer, which is what
	// writeFields and GoFieldValues actually put in their maps. A type-less
	// schema has no struct to read, so scalar stays nil and the check is skipped.
	var scalar reflect.Type
	if idx >= 0 {
		fd.path = FieldPath{idx}
		ft := s.typ.Field(idx).Type
		fd.applyTyping(s.table, goName, ft)
		scalar = effectiveScalar(ft)
	}
	fd.inSync = spec.inSync.mustFit(s.table, goName, "InSync", scalar)
	fd.inAudit = spec.inAudit.mustFit(s.table, goName, "InAudit", scalar)
	s.appendField(fd)
	return s
}

// mustClaimNames runs the name/column guards every declared field passes: the
// logical Go name is free, the column is free, not reserved, and not one the
// framework already owns. Shared by Field and by a composite's Part, so a part
// enters the bijection under exactly the same rules as a root field. `what`
// names the declaration in the message ("field" / "value-object part").
func (s *TableSchema) mustClaimNames(goName, column, what string) {
	if _, dup := s.byGo[goName]; dup {
		panic(fmt.Sprintf("infra.TableSchema(%s): %s %q declared twice", s.table, what, goName))
	}
	if _, dup := s.byCol[column]; dup {
		panic(fmt.Sprintf("infra.TableSchema(%s): column %q claimed by more than one field — the map must be a bijection", s.table, column))
	}
	mustNotReservedColumn(s.table, column)
	if column == s.idColumn || column == s.deletedAt || column == s.createdAt || column == s.updatedAt || column == s.revisionCol {
		panic(fmt.Sprintf("infra.TableSchema(%s): %s column %q collides with a ID/managed column", s.table, what, column))
	}
	// The aggregate-child ParentID is OWNED by the write cascade: insertChild sets it to
	// the parent's id, overwriting whatever a mapped field would carry, so a Field
	// on the ParentID column is a silently-ignored write — reject it at boot. Note this
	// is NOT the shared-ID case (ID == ParentID): the ID is never in byCol and is fixed
	// to the "ID" field, so that legitimate model is unaffected.
	if s.parentIDColumn != "" && column == s.parentIDColumn {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): %s column %q is the aggregate-root ParentID — it is written by the cascade "+
				"(set to the parent id), so a mapped field on it would be silently overwritten on every write; "+
				"drop the mapping. (A shared ID that IS the ParentID is fine — that is the ID, not a mapped field.)",
			s.table, what, column))
	}
}

// applyTyping derives a declared field's storage typing from its Go type — the
// closed-set check, the value-object unwrapping, and the identity kind. The
// TYPE is the declaration, so this is the single place that reads it, shared by
// Field and by a composite's Part.
func (f *schemaField) applyTyping(table, goName string, ft reflect.Type) {
	if enum, u, ok := valueObjectField(ft); ok {
		// A value-object field: the framework persists its UNDERLYING scalar,
		// so THAT is what must belong to the closed set (the named VO type is
		// unwrapped on write and reconstructed on read, so the driver never
		// sees it).
		f.isVO, f.isEnum, f.underlyingType = true, enum, u
		mustSupportedFieldType(table, goName, u)
		return
	}
	mustSupportedFieldType(table, goName, ft)
	switch ft {
	case idType:
		f.idKind = IDValue
	case idPtrType:
		f.idKind = IDPointer
	}
}

// appendField registers a resolved declaration in the three views the schema
// keeps of it (ordered slice, by Go name, by column).
func (s *TableSchema) appendField(fd schemaField) {
	s.fields = append(s.fields, fd)
	s.byGo[fd.goName] = fd
	s.byCol[fd.column] = fd
}

// DeletedAt enables the DeletedAt predicate on col (read scope-gate +
// archive/unarchive SQL). Omitting it disables DeletedAt: Archive/Unarchive
// are unavailable and the read gate is never applied.
func (s *TableSchema) DeletedAt(col string) *TableSchema {
	if s.secondary {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): a sibling declares NO DeletedAt — it has no lifecycle of its own; "+
				"the owner controls archive/delete for the whole 1:1 row. Drop this DeletedAt(%q) call.",
			s.table, col,
		))
	}
	s.mustNotRedeclare(s.deletedAt, "DeletedAt", col)
	mustNotReservedColumn(s.table, col)
	s.ensureColumnFree(col, "DeletedAt")
	s.deletedAt = col
	return s
}

// CreatedAt enables a framework-stamped created_at column: the framework writes
// col = the dialect's now expression on INSERT (it never relies on a DB DEFAULT
// it does not own).
func (s *TableSchema) CreatedAt(col string) *TableSchema {
	if s.secondary {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): a sibling declares NO managed timestamps — the sibling row is a "+
				"1:1 slice of the OWNER's row, and the owner's CreatedAt/UpdatedAt already date that row. "+
				"Drop this CreatedAt(%q) call (declare it on the owner).",
			s.table, col,
		))
	}
	s.mustNotRedeclare(s.createdAt, "CreatedAt", col)
	mustNotReservedColumn(s.table, col)
	s.ensureColumnFree(col, "CreatedAt")
	s.createdAt = col
	return s
}

// UpdatedAt enables a framework-stamped updated_at column: col = the dialect's
// now expression on INSERT and UPDATE.
func (s *TableSchema) UpdatedAt(col string) *TableSchema {
	if s.secondary {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): a sibling declares NO managed timestamps — the sibling row is a "+
				"1:1 slice of the OWNER's row, and the owner's CreatedAt/UpdatedAt already date that row. "+
				"Drop this UpdatedAt(%q) call (declare it on the owner).",
			s.table, col,
		))
	}
	s.mustNotRedeclare(s.updatedAt, "UpdatedAt", col)
	mustNotReservedColumn(s.table, col)
	s.ensureColumnFree(col, "UpdatedAt")
	s.updatedAt = col
	return s
}

// Child registers an aggregate child's schema, keyed by the child Go type name.
// An aggregate child MUST declare its foreign key to the root via .ParentID(col) — the
// persister injects the root id into that column on every child write — so a
// child without an ParentID is rejected here at construction.
func (s *TableSchema) Child(child *TableSchema) *TableSchema {
	s.refuseVertical("Child")
	if s.secondary {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): a sibling cannot own children — declare aggregate children on the "+
				"owner node, not on a sibling slice.", s.table))
	}
	if child == nil || child.typ == nil {
		panic(fmt.Sprintf("infra.TableSchema(%s): Child requires a type-anchored schema", s.table))
	}
	if child.secondary {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): Child(...) expects a NewTableSchema with ParentID; got a sibling "+
				"(NewSiblingSchema). A sibling is a 1:1 shared-ID secondary table, not a 1:N aggregate child.",
			s.table,
		))
	}
	if child.revisionCol != "" {
		// Revision() itself rejects a schema whose ParentID is already declared; this
		// closes the declaration-order hole (Revision before ParentID): a child's rows
		// are guarded by the OWNER's commit-order token, never their own.
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): aggregate child %q declares Revision(%q) — a child row is guarded by its "+
				"owner's revision; drop the call.", s.table, child.table, child.revisionCol))
	}
	if !child.hasPKDeclared() {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): aggregate child %q declares no primary key — declare .ID(column) "+
				"(there is no default; every schema must declare its ID)",
			s.table, child.typ.Name(),
		))
	}
	if child.parentIDColumn == "" {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): aggregate child %q declares no foreign key — declare .ParentID(col) on its "+
				"schema; the persister injects the root id into that column on every child write",
			s.table, child.typ.Name(),
		))
	}
	// An aggregate child may not itself reference a SharedBase. Only a root/role
	// schema may: the write and load paths resolve the shared base from the schema
	// they are given (the ROOT's SharedBaseRef), never from a child — so a
	// SharedBase declared on a child is silently ignored (never persisted, never
	// loaded). Reject the no-op at boot rather than accept it. (Applies to a root's
	// own child and to a base-child alike, since both attach through here.)
	if child.sharedBaseLink != nil {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): aggregate child %q references a SharedBase — only a root/role schema may "+
				"reference a shared base, never an aggregate child. Write and load resolve the shared base from the "+
				"ROOT schema only, so a child's SharedBase is silently ignored (never persisted or loaded). Declare "+
				"the shared identity on the root, or model the child as its own role aggregate.",
			s.table, child.typ.Name(),
		))
	}
	// A shared base's native child (base-child) is a leaf of the base: it carries
	// the base's deterministic id as its ParentID and may not itself nest. No
	// grandchildren and no sibling on a base-child — the recursive width allowed at
	// the role level is not part of the base side of the model.
	if s.isSharedBase {
		if len(child.children) > 0 {
			panic(fmt.Sprintf(
				"infra.TableSchema(%s): shared-base child %q declares its own Child(...) — no grandchildren "+
					"(the base is a root plus exactly one level of native children). Model it as a separate aggregate.",
				s.table, child.typ.Name()))
		}
		if len(child.siblings) > 0 {
			panic(fmt.Sprintf(
				"infra.TableSchema(%s): shared-base child %q declares a Sibling — a base-child carries no "+
					"sibling table. Declare the fields directly on the child.", s.table, child.typ.Name()))
		}
	}
	// Children are keyed by Go type name, so a second child of the SAME type would
	// silently overwrite the first (dropping a whole collection). Two collections
	// of one type are not supported — reject the duplicate at boot.
	if existing, dup := s.children[child.typ.Name()]; dup {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): aggregate child of type %q already declared (table %q) — a schema keys its "+
				"children by Go type name, so each child type is declared once; declaring another (table %q) would "+
				"silently drop the first.", s.table, child.typ.Name(), existing.table, child.table))
	}
	// Resolve the collection name the child's domain type declares — here, at boot,
	// so a child that cannot be named on the read side never reaches a running
	// process (resolution panics on a missing or malformed declaration). The name
	// belongs to the TYPE, not to this registration, so CollectionSegment() below
	// recomputes it rather than caching a copy here: every schema instance over
	// the same type answers identically, registered or not.
	segment := domain.CollectionSegmentOf(child.typ)
	for _, sibling := range s.children {
		if sibling.CollectionSegment() == segment {
			panic(fmt.Sprintf(
				"infra.TableSchema(%s): aggregate children %q (table %q) and %q (table %q) both declare "+
					"CollectionName() = %q — each collection occupies its own document segment, so the second would "+
					"overwrite the first. Give them distinct names.",
				s.table, sibling.typ.Name(), sibling.table, child.typ.Name(), child.table, segment))
		}
	}
	s.children[child.typ.Name()] = child
	return s
}

// Sibling registers a sibling table on this node (root or aggregate child): a
// private secondary table over the SAME Go type that shares this node's primary
// key (1:1) and carries a disjoint subset of the entity's fields. Width is
// unlimited (call it repeatedly); a sibling does not nest, declares no
// ParentID/ID/DeletedAt, and owns no children — the owner controls identity and
// lifecycle. The column/field partition (no overlap with the owner or another
// sibling) is checked at WithSchema via ValidateSiblings (order-independent).
func (s *TableSchema) Sibling(sib *TableSchema) *TableSchema {
	s.refuseVertical("Sibling")
	if s.secondary {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): a sibling cannot have a sibling — siblings are flat slices of ONE "+
				"row; declare every sibling table on the owner.", s.table))
	}
	if s.isSharedBase {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): a shared base is a flat identity with native children — it declares "+
				"Fields and Child(...), never a Sibling. A sibling is a 1:1 shared-ID slice of a SINGLE owner; "+
				"a shared base has many roles. Put the shared 1:1 fields directly on the base.", s.table))
	}
	if sib == nil || sib.typ == nil {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): Sibling requires a type-anchored schema built with NewSiblingSchema[T].",
			s.table))
	}
	if !sib.secondary {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): Sibling(...) expects a NewSiblingSchema(...); got a non-sibling schema "+
				"(use NewSiblingSchema[T] for a shared-ID secondary table).", s.table))
	}
	ownerName := s.table
	if s.typ != nil {
		ownerName = s.typ.Name()
	}
	if sib.typ != s.typ {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): sibling %q is over %s but its owner is over %s — a sibling carries a "+
				"subset of the SAME entity's fields.", s.table, sib.table, sib.typ.Name(), ownerName))
	}
	if sib.parentIDColumn != "" {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): sibling %q must not declare ParentID — it shares the owner's primary key, "+
				"not a foreign key.", s.table, sib.table))
	}
	if sib.hasPKDeclared() {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): sibling %q must not declare ID — it borrows the owner's primary key "+
				"(the shared 1:1 key).", s.table, sib.table))
	}
	if sib.deletedAt != "" {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): sibling %q must not declare DeletedAt — a sibling has no lifecycle of "+
				"its own; the owner controls archive/delete.", s.table, sib.table))
	}
	if sib.createdAt != "" || sib.updatedAt != "" {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): sibling %q must not declare CreatedAt/UpdatedAt — the owner's managed "+
				"timestamps already date the 1:1 row.", s.table, sib.table))
	}
	if len(sib.children) > 0 {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): sibling %q must not declare Child(...) — declare aggregate children on "+
				"the owner node, not on a sibling slice.", s.table, sib.table))
	}
	if len(sib.siblings) > 0 {
		panic(fmt.Sprintf("infra.TableSchema(%s): sibling %q must not itself declare siblings.", s.table, sib.table))
	}
	if len(sib.fields) == 0 {
		panic(fmt.Sprintf("infra.TableSchema(%s): sibling %q declares no fields.", s.table, sib.table))
	}
	if sib.table == s.table {
		panic(fmt.Sprintf("infra.TableSchema(%s): sibling table name %q duplicates the owner table.", s.table, sib.table))
	}
	for _, existing := range s.siblings {
		if existing.table == sib.table {
			panic(fmt.Sprintf("infra.TableSchema(%s): sibling table %q declared twice.", s.table, sib.table))
		}
	}
	s.siblings = append(s.siblings, sib)
	return s
}

// --- resolution -------------------------------------------------------------

func (s *TableSchema) Table() string          { return s.table }
func (s *TableSchema) IDColumn() string       { return s.idColumn }
func (s *TableSchema) ParentIDColumn() string { return s.parentIDColumn }

// ColumnOf returns the physical column for a Go field name (ID included).
func (s *TableSchema) ColumnOf(goName string) (string, bool) {
	if goName == s.idGo {
		return s.idColumn, true
	}
	f, ok := s.byGo[goName]
	return f.column, ok
}

// fieldIsNullable reports whether a Go field can hold an ABSENCE, and whether
// that could be determined at all.
//
// The Go type is the declaration, exactly as the read joins read it: a pointer
// holds NULL, a value does not. known is false when there is no struct to ask —
// a type-less schema, or a field that resolves to no struct position — and a
// caller then leaves the question unenforced rather than guessing, which is the
// stance the join validation already takes for a column its target's struct
// does not expose.
func (s *TableSchema) fieldIsNullable(goField string) (nullable, known bool) {
	if s == nil || s.typ == nil {
		return false, false
	}
	if goField == s.idGo {
		return s.IDKindOf(goField) == IDPointer, true
	}
	f, ok := s.byGo[goField]
	if !ok {
		return false, false
	}
	ft, ok := f.path.TypeIn(s.typ)
	if !ok {
		return false, false
	}
	return ft.Kind() == reflect.Pointer, true
}

// GoOf returns the Go field name for a physical column (ID included). The
// inverse of ColumnOf — lossless because the map is complete.
func (s *TableSchema) GoOf(column string) (string, bool) {
	if column == s.idColumn {
		return s.idGo, true
	}
	f, ok := s.byCol[column]
	return f.goName, ok
}

// WireFieldOf resolves a physical column to the wire-format field name a
// notification carries: the declared Go name rendered through the same
// acronym-aware lower-camel renderer the notification wire paths use ("ID" →
// "id", "ZipCode" → "zipCode"). Infra emitters that hold only a column — the
// not-found/concurrency probes, the shared-base duplicate guard — name the
// field through here so a physical column name never reaches the wire. A
// column the schema does not declare renders as itself: the only truthful
// name left.
func (s *TableSchema) WireFieldOf(column string) string {
	if g, ok := s.GoOf(column); ok && g != "" {
		return domain.WireFieldPath(g)
	}
	return column
}

// goNameForRead returns the logical Go field name for a physical column on the
// read path, including the managed columns under fixed logical names
// (created_at → "CreatedAt", updated_at → "UpdatedAt", DeletedAt → "DeletedAt")
// so a view can project them to the wire without a domain Go field. Returns
// ok=false for a column the schema does not own (e.g. _id, foreign keys).
func (s *TableSchema) goNameForRead(column string) (string, bool) {
	if g, ok := s.GoOf(column); ok {
		return g, true
	}
	switch column {
	case s.createdAt:
		if s.createdAt != "" {
			return "CreatedAt", true
		}
	case s.updatedAt:
		if s.updatedAt != "" {
			return "UpdatedAt", true
		}
	case s.deletedAt:
		if s.deletedAt != "" {
			return "DeletedAt", true
		}
	}
	// The foreign key is exposed read-only under the fixed logical name "ParentID" (the
	// write cascade owns the column). A schema carries at most one ParentID — the
	// aggregate-child ParentID or the SharedBase role ParentID, never both (boot-guarded) — so
	// this is unambiguous.
	if column != "" && (column == s.parentIDColumn ||
		(s.sharedBaseLink != nil && column == s.sharedBaseLink.parentIDColumn)) {
		return parentIDGoField, true
	}
	// A sibling's column is merged FLAT into the read document; translate it back
	// to the sibling's Go field so ToGoDoc keeps it (otherwise the merged column
	// would be dropped and never reach the response).
	for _, sib := range s.siblings {
		if g, ok := sib.GoOf(column); ok {
			return g, true
		}
	}
	// SharedBase columns are merged flat into the role document the same way.
	if s.sharedBaseLink != nil {
		if g, ok := s.sharedBaseLink.base.GoOf(column); ok {
			return g, true
		}
	}
	return "", false
}

// FieldOwner says WHERE a resolved column physically lives. A backing that
// stores one merged document ignores it; a backing that has to reach the column
// with a JOIN needs it, and deriving it from the resolution instead of
// re-walking the schema is what keeps the two from drifting.
type FieldOwner int

const (
	// OwnerAnchor — the schema's own row: a mapped business field, the id, a
	// managed slot, or the ParentID projection.
	OwnerAnchor FieldOwner = iota
	// OwnerSibling — a 1:1 shared-PK satellite, merged flat into the read
	// document and reached with a LEFT JOIN on the shared id.
	OwnerSibling
	// OwnerSharedBase — the shared base of a role schema, likewise merged flat
	// and reached by joining the role's foreign key to the base id.
	OwnerSharedBase
	// OwnerJoin — ANOTHER aggregate, reached across a declared read-only join.
	// Unlike the three above, its columns carry no uniqueness guarantee against
	// the anchor's: two aggregates may both have a "name". A field resolved here
	// therefore MUST be qualified, and ResolvedField.Qualifier says by what.
	OwnerJoin
)

// ResolvedField is the answer to the ONE question every read path asks about a
// logical Go field name: does it resolve on this schema, to which physical
// column, and on whose row.
type ResolvedField struct {
	Column string
	// Schema is the schema the column physically belongs to — the anchor
	// itself, a sibling, the shared base, or a joined aggregate.
	Schema *TableSchema
	Owner  FieldOwner
	// Qualifier is the table or alias the column MUST be prefixed with, empty
	// when it needs none. A schema resolves its own fields unqualified: the
	// anchor, its siblings and its shared base form one node whose column names
	// are unique by the schema's bijection. A joined aggregate is a different
	// node with no such guarantee, and two joins may even reach the SAME table
	// (bill_to and ship_to both to customers), so the resolver that admitted the
	// field names the alias it admitted it under.
	Qualifier string
}

// Resolve maps a logical Go field name to its physical column on the READ path,
// and reports whose row it lives on. It is the single resolution surface every
// read backing consults, so a Mongo view and its relational twin admit exactly
// the same names — a divergence here is a divergence in what the SAME Request
// DTO can express, which is never the backing's call.
//
// The surface, in order: the schema's own mapped fields and the id (ColumnOf);
// the three managed slots under their fixed logical names (CreatedAt,
// UpdatedAt, DeletedAt), which is how every layer above infra addresses them;
// the read-only ParentID projection; then the 1:1 satellites — each sibling and
// the shared base — whose fields sit FLAT at the owner's level in the read
// document because the composer merges them there.
//
// Resolving a name grants nothing on its own. What an end user may address is
// the Request DTO's `filter:` / `sort:` declaration, and what the service
// addresses is its Query's ToCriteria; both hand this layer a name some layer
// above deliberately chose.
func (s *TableSchema) Resolve(goName string) (ResolvedField, bool) {
	self := func(col string) (ResolvedField, bool) {
		return ResolvedField{Column: col, Schema: s, Owner: OwnerAnchor}, true
	}
	if c, ok := s.ColumnOf(goName); ok {
		return self(c)
	}
	switch goName {
	case "CreatedAt":
		if s.createdAt != "" {
			return self(s.createdAt)
		}
	case "UpdatedAt":
		if s.updatedAt != "" {
			return self(s.updatedAt)
		}
	case "DeletedAt":
		if s.deletedAt != "" {
			return self(s.deletedAt)
		}
	}
	if goName == parentIDGoField {
		if s.parentIDColumn != "" {
			return self(s.parentIDColumn)
		}
		if s.sharedBaseLink != nil && s.sharedBaseLink.parentIDColumn != "" {
			return self(s.sharedBaseLink.parentIDColumn)
		}
	}
	for _, sib := range s.siblings {
		if c, ok := sib.ColumnOf(goName); ok {
			return ResolvedField{Column: c, Schema: sib, Owner: OwnerSibling}, true
		}
	}
	if s.sharedBaseLink != nil {
		if c, ok := s.sharedBaseLink.base.ColumnOf(goName); ok {
			return ResolvedField{Column: c, Schema: s.sharedBaseLink.base, Owner: OwnerSharedBase}, true
		}
	}
	return ResolvedField{}, false
}

// isExternal reports whether the schema is type-less (built via
// NewExternalSchema, no Go struct anchor). A view embed's source kind is derived
// from this: an external schema describes an upstream Mongo collection
// (JoinUpstream → external Mongo collection), a type-anchored schema a local relational table.
func (s *TableSchema) isExternal() bool { return s.typ == nil }

// isUpstreamExternal narrows isExternal to the schema kind that names a table
// this connection CANNOT read: an upstream service's mirrored collection
// (NewExternalSchema).
//
// The distinction matters because type-less is not one thing. A shared base is
// type-less too — it carries no Go struct until a role resolves it — and it is a
// real table in this database, written and read by this service. A refusal that
// tests isExternal alone rejects it with the wrong reason, which is why the two
// declaration guards above spell out the same pair (typ == nil && !isSharedBase)
// rather than asking isExternal.
func (s *TableSchema) isUpstreamExternal() bool {
	return s != nil && s.typ == nil && !s.isSharedBase
}

// AsDirectSchema returns a COPY of this schema reduced to its own table: the
// anchor's columns, its id, its managed slots and its composite decompositions —
// with the vertical composition dropped (children, siblings, shared base, and the
// base's registry of referencing roles). The receiver is untouched.
//
// It exists because a read that reaches ACROSS into another schema — a declared
// read join's target, a criteria subquery's source — puts exactly one table in
// its FROM. Handed a whole node, such a read could only use a slice of it, and a
// slice taken in silence is the failure mode this framework refuses everywhere
// else. So those verbs demand a Direct schema, and this is how any schema becomes
// one: the reduction happens where the developer can see it, at the call site,
// instead of inside a translator.
//
// The result is an ordinary Direct schema, with everything that follows from
// that — including anchoring a DirectRepository. Writing an aggregate's table
// through the Direct path skips the outbox, the audit row, the revision guard and
// the cascade; that is a real escape hatch and it is deliberately left open, the
// same way core.Exec is. It is not the framework's business to forbid a decision
// it can carry out correctly.
//
// Two kinds do NOT convert, because the result would not be a readable table:
//
//   - a SIBLING has no identity of its own (it borrows the owner's primary key,
//     and declaring ID on it is refused), so on its own it is not a row source;
//   - an EXTERNAL schema names an upstream service's mirrored collection, which
//     does not exist on this connection at all.
//
// Both panic, naming the reason — a declaration mistake, reported like every
// other one on this type.
func (s *TableSchema) AsDirectSchema() *TableSchema {
	if s == nil {
		panic("infra.TableSchema: AsDirectSchema on a nil schema")
	}
	if s.secondary {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): AsDirectSchema on a SIBLING — a sibling borrows its owner's primary "+
				"key (declaring ID on it is refused), so it is not a row source on its own. Reducing the "+
				"OWNER would not help either: that produces the OWNER's table, and a sibling's columns "+
				"leave with the sibling. To read this table on its own, declare it as its own anchor with "+
				"core.NewDirectSchema, naming the shared id column in ID(...).", s.table))
	}
	if s.isUpstreamExternal() {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): AsDirectSchema on an EXTERNAL schema — its columns belong to an "+
				"upstream service and describe a locally materialized Mongo collection, so there is no "+
				"such table on this connection to read from.", s.table))
	}

	c := *s
	c.direct = true
	c.children = map[string]*TableSchema{}
	c.siblings = nil
	c.sharedBaseLink = nil
	c.referencingRoleLinks = nil
	c.isSharedBase = false
	c.naturalIDCol = ""
	c.orphanPolicy = OrphanPolicy(0)

	// The maps and slices are copied, not shared: a Direct schema is an ordinary
	// schema and may be declared onto further, and a Field(...) on the copy must
	// not appear on the original.
	c.fields = append([]schemaField(nil), s.fields...)
	c.byGo = make(map[string]schemaField, len(s.byGo))
	for k, v := range s.byGo {
		c.byGo[k] = v
	}
	c.byCol = make(map[string]schemaField, len(s.byCol))
	for k, v := range s.byCol {
		c.byCol[k] = v
	}
	c.composites = append([]*compositeDecl(nil), s.composites...)
	return &c
}

// typeName returns the schema's Go type name ("Address"), or "" for a type-less
// external schema. A local view embed derives its Go segment from this; an
// external embed has none, so it must declare the segment via .As(...).
func (s *TableSchema) typeName() string {
	if s.typ == nil {
		return ""
	}
	return s.typ.Name()
}

func (s *TableSchema) deletedAtColumn() (string, bool) { return s.deletedAt, s.deletedAt != "" }
func (s *TableSchema) createdAtColumn() (string, bool) { return s.createdAt, s.createdAt != "" }
func (s *TableSchema) updatedAtColumn() (string, bool) { return s.updatedAt, s.updatedAt != "" }

func (s *TableSchema) childSchema(typeName string) *TableSchema {
	if s == nil {
		return nil
	}
	return s.children[typeName]
}

// insertNowColumns returns the managed columns stamped with the write
// operation's app-clock instant on INSERT — created_at then updated_at, each
// when enabled. The stamp is minted in Go and bound as an ordinary argument
// (never a dialect NOW() expression), so it is known before COMMIT.
func (s *TableSchema) insertNowColumns() []string {
	var out []string
	if c, ok := s.createdAtColumn(); ok {
		out = append(out, c)
	}
	if c, ok := s.updatedAtColumn(); ok {
		out = append(out, c)
	}
	return out
}

// updateNowColumns returns the managed columns stamped with the write
// operation's app-clock instant on UPDATE.
func (s *TableSchema) updateNowColumns() []string {
	if c, ok := s.updatedAtColumn(); ok {
		return []string{c}
	}
	return nil
}

// writeFields returns the column → value map the INSERT/UPDATE binds, by reading
// each declared field's value via its resolved index. The ID is excluded
// (Go-minted + separate WHERE); managed timestamp columns are appended by the
// statement builders (bound to the operation stamp), not here.
func (s *TableSchema) writeFields(e any) domain.Fields {
	v := reflect.ValueOf(e)
	for v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	out := make(domain.Fields, len(s.fields))
	for _, f := range s.fields {
		// A stamped field is never written from the struct: the framework owns
		// the value, and a write that did not request it leaves the column out of
		// the statement entirely (StampColumns adds the requested ones).
		if f.stamped {
			continue
		}
		fv, ok := f.path.ValueIn(v)
		if !ok {
			// Either a type-less column, or a part of an ABSENT optional composite
			// (a nil *Address): the value object was never created, so every one of
			// its columns is SQL NULL — never a materialized zero.
			if f.path.resolved() {
				out[f.column] = nil
			}
			continue
		}
		// A value-object field binds as its underlying scalar (unwrapVO is a no-op
		// for a plain field); a nil nullable VO becomes SQL NULL.
		out[f.column] = unwrapVO(fv.Interface())
	}
	return out
}

// Exported write-path accessors. A relational engine living in its own package
// (the MySQL engine under its build tag) builds INSERT/UPDATE statements from a
// TableSchema; these thin wrappers expose the column → value map and the managed
// timestamp columns + DeletedAt column it needs, without widening the surface
// the in-package write path consumes (which keeps using the unexported forms).

// WriteFields is the exported form of writeFields — the column → value map an
// engine binds for INSERT/UPDATE (ID and managed timestamp columns excluded).
func (s *TableSchema) WriteFields(e any) domain.Fields { return s.writeFields(e) }

// StampColumns translates the stamped fields a write requested — Go field names
// from domain.RequestedStamps, or the keys a Direct write marked with
// write.Stamp — into the columns the statement binds to the operation's instant.
// Order follows DECLARATION order, not request order, so the generated SQL is
// stable however the domain happened to ask.
//
// A name this schema did not declare as stamped is refused, naming what went
// wrong: the domain asks by Go name and cannot see the schema, so this is the
// first moment the two meet. Both mistakes it catches are real — a typo, and a
// Stamp on a field that is mapped but plain (which would otherwise silently do
// nothing, since a plain field takes its value from the struct).
func (s *TableSchema) StampColumns(goFields []string) ([]string, error) {
	cols, unclaimed := s.ClaimStampColumns(goFields)
	if err := s.RefuseUnclaimedStamps(unclaimed); err != nil {
		return nil, err
	}
	return cols, nil
}

// ClaimStampColumns is StampColumns split in two, for the writes where ONE
// entity's requests are shared by MORE THAN ONE schema: a shared-base role
// carries the requests for its own columns AND for the base's, and neither
// schema can tell a name meant for the other from a typo on its own.
//
// So this half only CLAIMS: it returns the columns this schema declares stamped
// (in declaration order, so the SQL is stable) and hands back the names it does
// not recognize. The caller collects the leftovers from every schema the
// operation writes and refuses only what NOBODY claimed —
// RefuseUnclaimedStamps does that, keeping a typo as loud as it is on a
// single-schema write.
func (s *TableSchema) ClaimStampColumns(goFields []string) (cols, unclaimed []string) {
	if len(goFields) == 0 {
		return nil, nil
	}
	want := make(map[string]bool, len(goFields))
	for _, g := range goFields {
		want[g] = true
	}
	cols = make([]string, 0, len(goFields))
	for _, f := range s.fields {
		if f.stamped && want[f.goName] {
			cols = append(cols, f.column)
			delete(want, f.goName)
		}
	}
	for _, g := range goFields {
		if want[g] {
			unclaimed = append(unclaimed, g)
			delete(want, g) // a name repeated in the request is reported once
		}
	}
	return cols, unclaimed
}

// ClaimedStamp is one resolved stamp request: the column this schema claimed for
// it, WHAT the statement is to put there, and which kind of stamped column it is
// — everything the statement builders need, with the verb still attached to the
// column it belongs to.
type ClaimedStamp struct {
	Column  string
	Op      domain.StampOp
	Counter bool
}

// ClaimStampRequests is ClaimStampColumns with the verb carried through: it
// claims the requests this schema declares, in DECLARATION order (the order the
// statement's columns follow), and reports the names it does not know so a
// caller that can see every schema in the operation decides whether that is a
// mistake.
//
// It is also where the one type rule a verb can break is enforced: StampNull
// writes an absence, and a counter declared int64 has nowhere to put one. The
// refusal names StampEmpty, which is the verb that zeroes it — the schema knows
// the declared type, and this is the first moment the type and the verb meet.
func (s *TableSchema) ClaimStampRequests(reqs []domain.StampRequest) (claimed []ClaimedStamp, unclaimed []string, err error) {
	if len(reqs) == 0 {
		return nil, nil, nil
	}
	want := make(map[string]domain.StampOp, len(reqs))
	for _, r := range reqs {
		want[r.Field] = r.Op
	}
	claimed = make([]ClaimedStamp, 0, len(reqs))
	for _, f := range s.fields {
		op, asked := want[f.goName]
		if !f.stamped || !asked {
			continue
		}
		delete(want, f.goName)
		if op == domain.StampToNull && !s.StampedFieldAcceptsNull(f.goName) {
			return nil, nil, fmt.Errorf(
				"db: StampNull on %q (table %q) — the field is int64 and a plain int64 has no absence to "+
					"write. Declare it *int64 and the column nullable, or use StampEmpty to reset it to 0",
				f.goName, s.table)
		}
		claimed = append(claimed, ClaimedStamp{Column: f.column, Op: op, Counter: f.stampedCounter})
	}
	for _, r := range reqs {
		if _, still := want[r.Field]; still {
			unclaimed = append(unclaimed, r.Field)
			delete(want, r.Field) // a name repeated in the request is reported once
		}
	}
	return claimed, unclaimed, nil
}

// StampRequestColumns is ClaimStampRequests for a write where ONE schema owns
// the whole request: anything it did not claim is a mistake, since there is no
// sibling schema for the name to have belonged to.
func (s *TableSchema) StampRequestColumns(reqs []domain.StampRequest) ([]ClaimedStamp, error) {
	claimed, unclaimed, err := s.ClaimStampRequests(reqs)
	if err != nil {
		return nil, err
	}
	if err := s.RefuseUnclaimedStamps(unclaimed); err != nil {
		return nil, err
	}
	return claimed, nil
}

// RefuseUnclaimedStamps turns the names no schema claimed into the diagnostic
// the caller sees. Two mistakes reach here and each gets its own sentence: a
// name that is not a field at all (a typo), and one that IS mapped but plain —
// which would otherwise stamp nothing at all, silently.
func (s *TableSchema) RefuseUnclaimedStamps(goFields []string) error {
	for _, g := range goFields {
		if f, mapped := s.byGo[g]; mapped {
			return fmt.Errorf(
				"db: %s.Stamp(%q) — %q is a plain field on table %q, so its value comes from the entity and "+
					"stamping it would do nothing. Declare it with StampedTimeField(%q, %q) to hand its value "+
					"to the framework",
				s.table, g, g, s.table, g, f.column)
		}
		return fmt.Errorf(
			"db: %s.Stamp(%q) — table %q declares no stamped field %q. Stamped fields are declared with "+
				"StampedTimeField(goName, column); check the spelling of the Go field name",
			s.table, g, s.table, g)
	}
	return nil
}

// ApplyStamps writes the instant the statement bound back onto the entity, for
// each stamped field this write requested. It runs after the statement is built
// and before the operation's copies are made — which is the point: the audit
// event and the outbox payload read their values from the STRUCT
// (GoFieldValues), so without this the trail would report the field as still
// empty on the very write that filled it, and the caller would hold a stale
// entity until the next load.
//
// It is the same move the framework already makes with a minted child id
// (AssignAggregateItemID): the value is the framework's, and once decided it
// belongs on the entity too, so that everything downstream of one write agrees.
//
// The target must be a POINTER to be settable; an unaddressable or unresolvable
// field is skipped rather than reported, because the statement has already been
// built correctly — the row is right either way, and refusing here would fail a
// good write over a reporting detail.
func (s *TableSchema) ApplyStamps(e any, reqs []domain.StampRequest, now time.Time) {
	if len(reqs) == 0 {
		return
	}
	v := reflect.ValueOf(e)
	if v.Kind() != reflect.Pointer || v.IsNil() {
		return
	}
	v = v.Elem()
	for _, r := range reqs {
		f, ok := s.byGo[r.Field]
		if !ok || !f.stamped {
			continue
		}
		target := s.stampTargetIn(v, f)
		if !target.IsValid() || !target.CanSet() {
			continue
		}
		applyStampTo(target, r.Op, now)
	}
}

// stampTargetIn locates the struct field a stamped column writes back into.
//
// A type-anchored schema resolved that at declaration and carries the index
// path. A SHARED BASE has no struct of its own — its fields are resolved against
// each ROLE's type at .SharedBase(...) time, and the resolved path is stored on
// the ROLE's link, not here — so the base's own field carries no usable path and
// the write-back is done by GO NAME instead.
//
// By name is not a weaker rule: a base's non-composite field resolves by exactly
// that at declaration (exportedFieldIndex on the role's type), and a stamped
// field is never a composite value object — it is *time.Time or int64. It goes
// through the SAME index lookup so the two cannot drift: a PROMOTED field would
// satisfy a bare FieldByName and not the declaration, which rejects it (a role
// must carry every shared-base field as its own), and a write-back that reached
// further than the declaration would be writing somewhere nothing ever mapped.
func (s *TableSchema) stampTargetIn(v reflect.Value, f schemaField) reflect.Value {
	if !s.isSharedBase {
		return f.path.TargetIn(v)
	}
	if v.Kind() != reflect.Struct {
		return reflect.Value{}
	}
	idx := exportedFieldIndex(v.Type(), f.goName)
	if idx < 0 {
		return reflect.Value{}
	}
	return v.Field(idx)
}

// applyStampTo writes back onto the struct what the statement is about to write
// into the column, so the entity the caller keeps holding — and the audit event,
// which reads the STRUCT — report the same thing the row does.
//
// A counter is deliberately absent from the FILL case: its new value is the
// server's (`col = col + 1`) and the framework does not read it back, so there is
// nothing honest to write here. Null and empty are different — those values are
// the framework's own, known before the statement runs.
func applyStampTo(target reflect.Value, op domain.StampOp, now time.Time) {
	switch target.Type() {
	case stampedTimeType:
		switch op {
		case domain.StampToNull:
			target.Set(reflect.Zero(stampedTimeType))
		case domain.StampToEmpty:
			zero := time.Time{}
			target.Set(reflect.ValueOf(&zero))
		default:
			stamp := now
			target.Set(reflect.ValueOf(&stamp))
		}
	case stampedCounterType:
		if op == domain.StampToEmpty {
			target.SetInt(0)
		}
	case nullableStampedCounterType:
		switch op {
		case domain.StampToNull:
			target.Set(reflect.Zero(nullableStampedCounterType))
		case domain.StampToEmpty:
			zero := int64(0)
			target.Set(reflect.ValueOf(&zero))
		}
	}
}

// IsStampedField reports whether goName is declared on this schema as a stamped
// field — what a write path checks before it lets a caller bind a value to it.
func (s *TableSchema) IsStampedField(goName string) bool {
	f, ok := s.byGo[goName]
	return ok && f.stamped
}

// HasStampedFields reports whether this schema declares any stamped field —
// what a write path checks before it looks for requests.
func (s *TableSchema) HasStampedFields() bool {
	for _, f := range s.fields {
		if f.stamped {
			return true
		}
	}
	return false
}

// InsertNowColumns is the exported form of insertNowColumns.
func (s *TableSchema) InsertNowColumns() []string { return s.insertNowColumns() }

// UpdateNowColumns is the exported form of updateNowColumns.
func (s *TableSchema) UpdateNowColumns() []string { return s.updateNowColumns() }

// DeletedAtColumn is the exported form of deletedAtColumn — the DeletedAt
// column and whether it was declared (engines gate archive/unarchive on it).
func (s *TableSchema) DeletedAtColumn() (string, bool) { return s.deletedAtColumn() }

// ChildSchema is the exported form of childSchema — the declared child schema for
// an aggregate child type name (nil when undeclared). An out-of-package engine's
// aggregate persister resolves child tables/columns/ParentID through it.
func (s *TableSchema) ChildSchema(typeName string) *TableSchema { return s.childSchema(typeName) }

// ChildSchemas returns every declared aggregate child schema, ordered by table
// name so the emitted SQL is deterministic across runs and backends. The
// aggregate delete path uses it to remove each child table's rows by ParentID
// explicitly (in Go, owned by the framework) rather than relying on a database
// ON DELETE CASCADE the framework neither emits nor can validate at boot.
func (s *TableSchema) ChildSchemas() []*TableSchema {
	if s == nil || len(s.children) == 0 {
		return nil
	}
	out := make([]*TableSchema, 0, len(s.children))
	for _, c := range s.children {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].table < out[j].table })
	return out
}

// IsSecondary reports whether this schema is a sibling (built with
// NewSiblingSchema) — a shared-ID secondary table, never a repository root.
func (s *TableSchema) IsSecondary() bool { return s != nil && s.secondary }

// Siblings returns this node's declared sibling tables (shared-ID secondary
// tables over the same entity), in declaration order. The write and compose
// paths partition the entity's columns across the owner table and these.
func (s *TableSchema) Siblings() []*TableSchema {
	if s == nil || len(s.siblings) == 0 {
		return nil
	}
	out := make([]*TableSchema, len(s.siblings))
	copy(out, s.siblings)
	return out
}

// BoolColumns returns the set of physical columns whose mapped Go field is a bool
// (or *bool). The composer uses it to restore type fidelity on a backend that
// cannot carry a native boolean: MySQL stores BOOL/BOOLEAN as TINYINT(1) and the
// dynamic read yields int64(0/1), so a bool field would compose into Mongo as a
// number. Empty for an external/type-less schema (no Go struct to reflect).
func (s *TableSchema) BoolColumns() map[string]bool {
	if s.typ == nil {
		return nil
	}
	out := make(map[string]bool)
	for _, f := range s.fields {
		ft, ok := f.path.TypeIn(s.typ)
		if !ok {
			continue
		}
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Bool {
			out[f.column] = true
		}
	}
	return out
}

// ScanPlan returns the SELECT columns (in field order) + a column → FieldPath
// map for the scanner. The ID column is included only when the ID is an exported
// struct field (aggregate child); for the root (idIndex < 0) the ID is the
// leading key handled by ScanLeadingKey, not a struct field.
//
// The map carries a PATH, not a field position, because a part of a composite
// value object lives inside the entity rather than at its root (Person.Address.
// Street). A root field is a one-element path, so the depth-1 case is the same
// code, not a branch.
func (s *TableSchema) ScanPlan() (cols []string, byCol map[string]FieldPath) {
	byCol = make(map[string]FieldPath, len(s.fields)+1)
	if s.idIndex >= 0 {
		cols = append(cols, s.idColumn)
		byCol[s.idColumn] = FieldPath{s.idIndex}
	}
	for _, f := range s.fields {
		if !f.path.resolved() {
			continue
		}
		cols = append(cols, f.column)
		byCol[f.column] = f.path
	}
	return cols, byCol
}

// ReadColumns is the complete, deterministically ordered set of PHYSICAL columns
// this schema's table carries — the explicit column list a read issues instead of
// SELECT *. Order: ID, business fields (declaration order), the shared-base ParentID (a
// role's link to its base) and the aggregate-child ParentID, then the managed columns
// (created_at, updated_at, deleted_at, revision) — each included only when
// declared, deduplicated (ID==ParentID collapses to one). It names the columns of THIS
// ONE table only: siblings and the shared base are separate tables read through
// their own schema.
//
// Naming the columns (never SELECT *) keeps a prepared statement's result type
// stable across an online ADD COLUMN, so a blue-green view rebuild that adds a
// projected column cannot break an in-flight read on a pod still serving the old
// version ("cached plan must not change result type", SQLSTATE 0A000). Every
// column a read consumes lives in the schema by invariant, so the list is
// complete by construction; an undeclared physical column is never scanned.
func (s *TableSchema) ReadColumns() []string {
	seen := make(map[string]bool)
	cols := make([]string, 0, len(s.fields)+6)
	add := func(c string) {
		if c != "" && !seen[c] {
			seen[c] = true
			cols = append(cols, c)
		}
	}
	add(s.idColumn)
	for _, f := range s.fields {
		add(f.column)
	}
	if s.sharedBaseLink != nil {
		add(s.sharedBaseLink.parentIDColumn)
	}
	add(s.parentIDColumn)
	add(s.createdAt)
	add(s.updatedAt)
	add(s.deletedAt)
	add(s.revisionCol)
	return cols
}

// FieldResolver maps a Go field name to the RESOLVED field — its column, whose
// row it lives on, and how it must be qualified; ok=false for an unknown /
// non-persisted field → the translator fails fast (developer bug). The type
// lives here (the schema foundation) rather than with the translator so both
// the schema and the translator can name it without a cycle.
//
// It answers with a ResolvedField rather than a bare column on purpose. A column
// name alone cannot say whether it needs a table prefix, and a resolver that
// returned "customers.name" as if it were a column would be rendered as ONE
// quoted identifier by the dialect — invalid SQL. Whose row a field lives on is
// information the translator needs, and TableSchema.Resolve already knows it.
//
// Each caller BUILDS its own, because the right resolution surface is the
// caller's business: the aggregate loader resolves the anchor, then each
// sibling, the shared base and each declared join, recording the LEFT JOINs it
// will have to emit as it goes. A generic schema-in-a-closure resolver cannot do
// that bookkeeping and would answer with a column from a table the FROM never
// joined.
type FieldResolver func(goField string) (ResolvedField, bool)

// ValidateChildDepth panics when any declared aggregate child carries its own
// Child(...) — i.e. a grandchild. Aggregate persistence is root + exactly one
// level of children (insertChildren/applyChildChanges/the cascade all iterate
// root.AllAggregateItems() once and never recurse into a child's children), so a
// grandchild declared on the schema would silently never persist. Turn that
// no-op into a loud boot failure that names the alternative.
func (s *TableSchema) ValidateChildDepth() {
	for _, child := range s.children {
		if len(child.children) > 0 {
			panic(fmt.Sprintf(
				"infra.TableSchema(%s): aggregate child %q declares its own Child(...) — "+
					"grandchildren are NOT supported by aggregate persistence (an aggregate is its "+
					"root plus exactly one level of children, persisted in a single transaction). "+
					"Model the sub-collection as a SEPARATE aggregate with its own root table + ParentID.",
				s.table, child.typ.Name(),
			))
		}
	}
}

// ValidateSiblings panics when a sibling's partition overlaps the owner's (or
// another sibling's) — every mapped Go field and every physical column must
// belong to EXACTLY ONE table of the node. Runs at WithSchema (after the schema
// is fully assembled, so it is order-independent, like ValidateChildDepth), for
// the root and each aggregate child (a child may carry its own siblings). The
// shared ID and managed columns are NOT partitioned — they are the owner's
// identity/lifecycle, mapped only on the owner.
func (s *TableSchema) ValidateSiblings() {
	s.validateOwnSiblings()
	for _, child := range s.children {
		child.validateOwnSiblings()
	}
}

func (s *TableSchema) validateOwnSiblings() {
	if len(s.siblings) == 0 {
		return
	}
	colOwner := map[string]string{} // physical column → owning table
	goOwner := map[string]string{}  // Go field name → owning table
	claim := func(table, col, goName string) {
		if prev, dup := colOwner[col]; dup {
			panic(fmt.Sprintf(
				"infra.TableSchema(%s): column %q is mapped by both %q and %q — a column belongs to "+
					"exactly one table of the node.", s.table, col, prev, table))
		}
		colOwner[col] = table
		if prev, dup := goOwner[goName]; dup {
			panic(fmt.Sprintf(
				"infra.TableSchema(%s): Go field %q is mapped by both %q and %q — a field belongs to "+
					"exactly one table of the node.", s.table, goName, prev, table))
		}
		goOwner[goName] = table
	}
	for _, f := range s.fields {
		claim(s.table, f.column, f.goName)
	}
	for _, sib := range s.siblings {
		for _, f := range sib.fields {
			claim(sib.table, f.column, f.goName)
		}
	}
}

// ValidateAnchored panics when the schema is type-less (built via
// NewExternalSchema rather than NewTableSchema[T]). A schema that backs the write
// path MUST be anchored to a Go type: the persister reflects the entity to build
// INSERT/UPDATE, and the read-side composer reflects it (BoolColumns) to restore
// type fidelity when it materializes the Mongo view — neither is possible without
// a struct. A type-less schema describes an UPSTREAM service's Mongo collection
// and is only ever a view EMBED source (JoinUpstream), never a write-backed root.
// The composer routes by the view root TABLE NAME (the root schema's table), not
// by the schema's kind, so a type-less root that names a real local table would
// be composed relationally with an empty BoolColumns and silently lose boolean
// fidelity on a backend without a native bool (MySQL TINYINT(1) → number). This
// turns that latent divergence into a loud boot failure. Aggregate children are
// already guaranteed anchored: Child(...) rejects a type-less child at
// declaration, so the write-backed invariant (root + every child type-anchored)
// is complete with this root-side guard.
func (s *TableSchema) ValidateAnchored() {
	if s.typ != nil {
		return
	}
	panic(fmt.Sprintf(
		"infra.TableSchema(%s): a write-backed schema must be type-anchored — build it with "+
			"NewTableSchema[T], not NewExternalSchema. A type-less schema describes an upstream "+
			"Mongo collection and can only be a view embed source (JoinUpstream), never a repository root.",
		s.table,
	))
}

// ValidateModes panics when the entity declares an archive verb but DeletedAt
// is disabled — turning a runtime SQL error into a loud boot failure.
func (s *TableSchema) ValidateModes(modes []domain.EntityMode) {
	if _, ok := s.deletedAtColumn(); ok {
		return
	}
	for _, m := range modes {
		if m == domain.ModeArchive || m == domain.ModeUnarchive {
			name := s.table
			if s.typ != nil {
				name = s.typ.Name()
			}
			panic(fmt.Sprintf(
				"infra.TableSchema(%s): entity declares %s in Modes() but DeletedAt is not enabled — "+
					"declare DeletedAt(col) or drop the mode",
				name, modeName(m),
			))
		}
	}
}

func modeName(m domain.EntityMode) string {
	if m == domain.ModeArchive {
		return "ModeArchive"
	}
	return "ModeUnarchive"
}

// jsonMarshalerType / jsonUnmarshalerType anchor the interface probes of
// ValidateOldCloneSafety (reflect.Type.Implements needs the interface's type).
var (
	jsonMarshalerType   = reflect.TypeOf((*json.Marshaler)(nil)).Elem()
	jsonUnmarshalerType = reflect.TypeOf((*json.Unmarshaler)(nil)).Elem()
)

// ValidateOldCloneSafety panics when the anchored entity type would corrupt the
// domain.Old pre-write snapshot. The framework builds that snapshot (the
// read-only "ghost" consumed by BuildRules transition checks and the transition
// auditor) via an encoding/json round-trip of the ROOT entity, so two consumer
// choices poison it silently at runtime: a persisted field tagged `json:"-"`
// vanishes from the ghost (the prior state reads as the zero value), and a
// custom json.Marshaler/json.Unmarshaler on the entity type replaces the whole
// round-trip with a serialization the clone contract does not control. Both
// become loud boot failures here.
//
// Scope: every field persisted THROUGH the root struct — the root's own
// declared fields, each sibling's partition (same Go type), and a shared base's
// fields (the type-less base resolves its fields on this role's type).
// Aggregate children are exempt: the clone copies them by value, never through
// JSON. Ordinary renaming tags (`json:"name"`) stay allowed — the round-trip
// marshals and unmarshals the same type, so renames are symmetric.
func (s *TableSchema) ValidateOldCloneSafety() {
	if s.typ == nil {
		return
	}
	if pt := reflect.PointerTo(s.typ); pt.Implements(jsonMarshalerType) || pt.Implements(jsonUnmarshalerType) {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): %s implements json.Marshaler/json.Unmarshaler — the framework builds the "+
				"domain.Old() snapshot by cloning the entity through a JSON round-trip, and a custom (un)marshaler "+
				"takes that contract over. Keep the entity free of custom JSON methods; shape wire payloads on the "+
				"web-layer DTOs instead.",
			s.table, s.typ.Name(),
		))
	}
	reject := func(owner string, goName string) {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): persisted field %s.%s is tagged `json:\"-\"` — the framework builds the "+
				"domain.Old() snapshot by cloning the entity through a JSON round-trip, so this field would "+
				"silently vanish from the ghost (transition rules and the transition auditor would read the zero "+
				"value as the prior state). Drop the tag; wire names belong to the web-layer DTOs (a renaming "+
				"`json:\"name\"` tag is harmless).",
			owner, s.typ.Name(), goName,
		))
	}
	jsonDashed := func(t reflect.Type, p FieldPath) bool {
		sf, ok := p.StructFieldIn(t)
		return ok && sf.Tag.Get("json") == "-"
	}
	for _, f := range s.fields {
		if jsonDashed(s.typ, f.path) {
			reject(s.table, f.goName)
		}
	}
	for _, sib := range s.siblings {
		for _, f := range sib.fields {
			if jsonDashed(sib.typ, f.path) {
				reject(sib.table, f.goName)
			}
		}
	}
	if link := s.sharedBaseLink; link != nil {
		for _, f := range link.base.fields {
			if p, ok := link.scanByCol[f.column]; ok && jsonDashed(s.typ, p) {
				reject(link.base.table, f.goName)
			}
		}
	}
	s.validateCompositeCloneSafety()
	s.validateCompositeOnceRule()
}

// validateCompositeCloneSafety extends the Old()-snapshot guards above to the
// composite value objects this entity carries. The ghost is a JSON round-trip
// of the ROOT, so a value object with a custom (un)marshaler takes that contract
// over exactly as an entity would — and it is a far more common thing to write
// on a value object than on an entity, which is why the guard has to exist.
func (s *TableSchema) validateCompositeCloneSafety() {
	for _, sc := range s.selfAndPartitions() {
		for _, decl := range sc.composites {
			pt := reflect.PointerTo(decl.typ)
			if decl.typ.Implements(jsonMarshalerType) || decl.typ.Implements(jsonUnmarshalerType) ||
				pt.Implements(jsonMarshalerType) || pt.Implements(jsonUnmarshalerType) {
				panic(fmt.Sprintf(
					"infra.TableSchema(%s): the composite value object %s implements json.Marshaler/json.Unmarshaler — "+
						"the framework builds the domain.Old() snapshot by cloning the entity through a JSON round-trip, "+
						"and a custom (un)marshaler on a persisted value object takes that contract over (the ghost's "+
						"composite would not survive it). Keep the value object free of custom JSON methods; shape wire "+
						"payloads on the web-layer DTOs instead.",
					sc.table, decl.typ))
			}
		}
	}
}

// validateCompositeOnceRule enforces the cross-schema half of the once rule:
// each composite value object type appears EXACTLY ONCE in an entity's schema
// graph — root, every sibling, and the shared base. Splitting one across two of
// them is rejected because the READ side cannot honor it: a sibling is loaded by
// a separate SELECT (an absent row leaves its fields zeroed), so a split
// composite reconstructs half-built, and the "every part NULL ⇒ nil" decision of
// an optional composite cannot be taken by either scan alone.
//
// It runs at the boot checkpoint rather than at the declaration call so that the
// order between DecomposeValueObject and Sibling/SharedBase cannot matter.
func (s *TableSchema) validateCompositeOnceRule() {
	seen := map[reflect.Type]string{}
	for _, sc := range s.selfAndPartitions() {
		for _, decl := range sc.composites {
			if where, dup := seen[decl.typ]; dup {
				panic(fmt.Sprintf(
					"infra.TableSchema(%s): the composite value object %s is decomposed on BOTH %q and %q — a "+
						"composite is persisted by exactly ONE schema of an entity. Its parts are loaded by separate "+
						"statements (a sibling/base row that is absent leaves its half zeroed, and an optional "+
						"composite's \"every part NULL ⇒ nil\" cannot be decided by either half alone), so a split "+
						"value object reconstructs half-built. Move every Part(...) into one table.",
					s.table, decl.typ, where, sc.table))
			}
			seen[decl.typ] = sc.table
		}
	}
}

// selfAndPartitions lists the schemas that persist THIS entity's fields: the
// node itself, its siblings (same Go type, disjoint columns) and its shared
// base (type-less, resolved against this role's struct). It is the scope every
// entity-wide checkpoint walks.
func (s *TableSchema) selfAndPartitions() []*TableSchema {
	out := make([]*TableSchema, 0, len(s.siblings)+2)
	out = append(out, s)
	out = append(out, s.siblings...)
	if s.sharedBaseLink != nil {
		out = append(out, s.sharedBaseLink.base)
	}
	return out
}
