package read

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/db/criteria"
)

// ─── fixtures ────────────────────────────────────────────────────────────────

// joinOrder is the declaring aggregate: two FKs — one mandatory (domain.ID), one
// optional (*domain.ID) — plus the Go fields the joins land on.
type joinOrder struct {
	domain.AggregateRoot
	Code       string
	CustomerID domain.ID
	CarrierID  *domain.ID

	CustomerName string  // inner join: always matches, non-nullable is honest
	CarrierCode  *string // left join: no counterpart → NULL
	unexported   string

	// One field per DOMAIN kind, so the guard can be proven to name each of them.
	// None is declarable as a join field; they exist only as rejection material.
	OwnerID    domain.ID  // identity, by value
	AltOwnerID *domain.ID // identity, nullable
	// The shapes an identity column of the TARGET is allowed to land in, plus one
	// that is not.
	TargetOwner    string
	TargetAltOwner *string
	TargetBadOwner int64
	TargetNick     *string
	TargetNickBad  string
	VendorCode     joinVOCode  // scalar value object
	VendorTier     joinVOTier  // enum value object
	VendorAddr     joinVOAddr  // composite value object
	VendorNote     *joinVOCode // scalar value object, nullable
}

// The three value-object kinds the persistence seam distinguishes. Each is
// declared by the SIGNAL the framework detects it by, nothing more.

// joinVOCode is a raw (scalar-backed) value object: Value() plus IsValid.
type joinVOCode string

func (c joinVOCode) Value() string                                    { return string(c) }
func (c joinVOCode) IsValid(string, *domain.NotificationContext) bool { return true }

// joinVOTier is an enum value object: its exclusive signal is UnknownNotification.
type joinVOTier int

func (t joinVOTier) Value() int           { return int(t) }
func (t joinVOTier) Values() []joinVOTier { return []joinVOTier{0, 1, 2} }
func (t joinVOTier) UnknownNotification() domain.Notification {
	return domain.RequiredFieldNotification{}
}

// joinVOAddr is a COMPOSITE value object: it owns its rule and declares no
// Value(), because its value spans more than one column.
type joinVOAddr struct {
	Street string
	City   string
}

func (a joinVOAddr) IsValid(string, *domain.NotificationContext) bool { return true }

func (e *joinOrder) Modes() []domain.EntityMode                       { return []domain.EntityMode{domain.ModeInsert} }
func (e *joinOrder) BuildRules(string, domain.Service, *domain.Rules) {}
func (e *joinOrder) GetAggregateRoot() *domain.AggregateRoot          { return &e.AggregateRoot }
func (e *joinOrder) AggregateChildren() []domain.AggregateValueObject {
	return []domain.AggregateValueObject{joinLine{}}
}

type joinLine struct {
	domain.Managed
	Label     string
	CityID    domain.ID
	CityName  string
	StateName *string
}

func (l joinLine) BuildRules(string, domain.Service, *domain.Rules) {}
func (l joinLine) CollectionName() string                           { return "Lines" }
func (l joinLine) IsSameBusinessIdentity(o domain.AggregateValueObject) bool {
	x, ok := o.(joinLine)
	return ok && x.Label == l.Label
}

type joinCustomer struct {
	domain.BaseEntity
	Name string
	// Two ordinary columns of the target that ARE identities — a foreign key of
	// the joined aggregate, which is exactly what a traversal reaches for.
	OwnerID    domain.ID
	AltOwnerID *domain.ID
	// An ordinary column the target declares NULLABLE — the general case of the
	// same rule the identity columns above are a special case of.
	Nickname *string
}

func (e *joinCustomer) Modes() []domain.EntityMode                       { return []domain.EntityMode{domain.ModeInsert} }
func (e *joinCustomer) BuildRules(string, domain.Service, *domain.Rules) {}

func joinLineSchema() *TableSchema {
	return NewTableSchema[joinLine]("order_lines").
		ID("id").ParentID("order_id").
		Field("Label", "label").
		Field("CityID", "city_id")
}

func joinOrderSchema() *TableSchema {
	return NewTableSchema[*joinOrder]("orders").
		ID("id").
		Revision("revision").
		Field("Code", "code").
		Field("CustomerID", "customer_id").
		Field("CarrierID", "carrier_id").
		Child(joinLineSchema())
}

func joinTargetSchema(table string) *TableSchema {
	return NewTableSchema[*joinCustomer](table).ID("id").Field("Name", "nome").
		Field("OwnerID", "owner_id").
		Field("AltOwnerID", "alt_owner_id").
		Field("Nickname", "nickname")
}

func joinLoader(schema *TableSchema) *AggregateLoader[*joinOrder] {
	return NewAggregateLoader[*joinOrder](nil, func() *joinOrder { return &joinOrder{} }).
		WithContextName("JoinOrder").
		WithSchema(schema)
}

// wantPanic runs fn and asserts it panics with a message containing want.
func wantPanic(t *testing.T, want string, fn func()) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("expected a construction panic mentioning %q, got none", want)
		}
		if msg := strings.Join(strings.Fields(toString(r)), " "); !strings.Contains(msg, want) {
			t.Errorf("panic must explain the violation.\n got: %s\nwant substring: %q", msg, want)
		}
	}()
	fn()
}

func toString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if e, ok := v.(error); ok {
		return e.Error()
	}
	return ""
}

// ─── the happy path ──────────────────────────────────────────────────────────

func TestWithJoins_AcceptsAWellFormedDeclaration(t *testing.T) {
	l := joinLoader(joinOrderSchema()).WithJoins(
		InnerJoin(joinTargetSchema("customers")).On("customer_id").Field("CustomerName", "nome"),
		LeftJoin(joinTargetSchema("carriers")).On("carrier_id").Field("CarrierCode", "nome"),
		LeftJoinInChild(joinLineSchema()).To(joinTargetSchema("cities")).On("city_id").Field("StateName", "nome"),
	)
	if got := len(l.Joins()); got != 3 {
		t.Fatalf("declared joins = %d, want 3", got)
	}
	if l.Joins()[0].Kind != JoinInner || l.Joins()[1].Kind != JoinLeft {
		t.Errorf("the declared kind must survive: %v", l.Joins())
	}
	if l.Joins()[2].Child == nil || l.Joins()[2].Child.Table() != "order_lines" {
		t.Errorf("a child join must carry its child: %+v", l.Joins()[2])
	}
}

func TestWithJoins_SkipsNilBindings(t *testing.T) {
	l := joinLoader(joinOrderSchema()).WithJoins(nil)
	if got := len(l.Joins()); got != 0 {
		t.Fatalf("a nil binding must be skipped, got %d joins", got)
	}
}

// Declaring a join leaves the TableSchema alone — that is what keeps a join off
// every write and out of a projected view of the same entity.
func TestWithJoins_LeavesTheSchemaUntouched(t *testing.T) {
	schema := joinOrderSchema()
	before := len(schema.ReadColumns())
	joinLoader(schema).WithJoins(
		InnerJoin(joinTargetSchema("customers")).On("customer_id").Field("CustomerName", "nome"),
	)
	if after := len(schema.ReadColumns()); after != before {
		t.Fatalf("a join must not add columns to the schema: %d → %d", before, after)
	}
}

// ─── the declaration-time guards ─────────────────────────────────────────────

func TestJoin_RejectsANilTarget(t *testing.T) {
	wantPanic(t, "the target schema is mandatory", func() { LeftJoin(nil) })
}

func TestJoin_RejectsANilChild(t *testing.T) {
	wantPanic(t, "the child schema is mandatory", func() {
		LeftJoinInChild(nil).To(joinTargetSchema("cities"))
	})
}

func TestJoin_RejectsAMissingOn(t *testing.T) {
	wantPanic(t, "no .On(...)", func() {
		joinLoader(joinOrderSchema()).WithJoins(
			LeftJoin(joinTargetSchema("carriers")).Field("CarrierCode", "nome"),
		)
	})
}

func TestJoin_RejectsAJoinThatMapsNothing(t *testing.T) {
	wantPanic(t, "no .Field(...)", func() {
		joinLoader(joinOrderSchema()).WithJoins(
			LeftJoin(joinTargetSchema("carriers")).On("carrier_id"),
		)
	})
}

func TestJoin_RejectsAnEmptyOn(t *testing.T) {
	wantPanic(t, "the foreign key column is mandatory", func() {
		LeftJoin(joinTargetSchema("carriers")).On("")
	})
}

func TestJoin_RejectsAnEmptyFieldMapping(t *testing.T) {
	wantPanic(t, "both the Go field and the column are mandatory", func() {
		LeftJoin(joinTargetSchema("carriers")).On("carrier_id").Field("", "nome")
	})
}

// ─── the schema-aware guards ─────────────────────────────────────────────────

func TestWithJoins_RequiresASchemaFirst(t *testing.T) {
	wantPanic(t, "call WithSchema(...) before WithJoins(...)", func() {
		NewAggregateLoader[*joinOrder](nil, func() *joinOrder { return &joinOrder{} }).WithJoins(
			LeftJoin(joinTargetSchema("carriers")).On("carrier_id").Field("CarrierCode", "nome"),
		)
	})
}

func TestWithJoins_RejectsAnFKThatIsNotAColumn(t *testing.T) {
	wantPanic(t, "is not a column of \"orders\"", func() {
		joinLoader(joinOrderSchema()).WithJoins(
			LeftJoin(joinTargetSchema("carriers")).On("nao_existe").Field("CarrierCode", "nome"),
		)
	})
}

func TestWithJoins_RejectsAColumnTheTargetDoesNotHave(t *testing.T) {
	wantPanic(t, "is not a column of \"carriers\"", func() {
		joinLoader(joinOrderSchema()).WithJoins(
			LeftJoin(joinTargetSchema("carriers")).On("carrier_id").Field("CarrierCode", "nao_existe"),
		)
	})
}

// The join lands its value on a Go field — it must be there, and reachable.
func TestWithJoins_RejectsAMissingGoField(t *testing.T) {
	wantPanic(t, "has no field \"Fantasma\"", func() {
		joinLoader(joinOrderSchema()).WithJoins(
			LeftJoin(joinTargetSchema("carriers")).On("carrier_id").Field("Fantasma", "nome"),
		)
	})
}

func TestWithJoins_RejectsAnUnexportedGoField(t *testing.T) {
	wantPanic(t, "is unexported", func() {
		joinLoader(joinOrderSchema()).WithJoins(
			LeftJoin(joinTargetSchema("carriers")).On("carrier_id").Field("unexported", "nome"),
		)
	})
}

// A join field must not shadow a name the schema already answers — the criteria
// would become ambiguous between the entity's own field and the joined one.
func TestWithJoins_RejectsAFieldThatShadowsTheSchema(t *testing.T) {
	wantPanic(t, "already resolves on \"orders\"", func() {
		joinLoader(joinOrderSchema()).WithJoins(
			LeftJoin(joinTargetSchema("carriers")).On("carrier_id").Field("Code", "nome"),
		)
	})
}

// An INNER join over a NULLABLE foreign key would drop roots in silence — from
// FindByID too, which the write-side handlers load through.
func TestWithJoins_RejectsInnerJoinOverANullableFK(t *testing.T) {
	wantPanic(t, "use LeftJoin instead", func() {
		joinLoader(joinOrderSchema()).WithJoins(
			InnerJoin(joinTargetSchema("carriers")).On("carrier_id").Field("CustomerName", "nome"),
		)
	})
}

func TestWithJoins_AcceptsInnerJoinOverAMandatoryFK(t *testing.T) {
	l := joinLoader(joinOrderSchema()).WithJoins(
		InnerJoin(joinTargetSchema("customers")).On("customer_id").Field("CustomerName", "nome"),
	)
	if len(l.Joins()) != 1 {
		t.Fatal("an inner join over a non-nullable FK must be accepted")
	}
}

// A LEFT join produces NULL when there is no counterpart: a non-nullable Go
// field would take its zero value and report a blank where the truth is absence.
// A join field carries no domain type. The value is another aggregate's, arrives
// read-only, and is never validated by this domain — a domain type here would be
// an instance no rule ever approved. The guard names WHICH kind it found, because
// the remedy differs between a value object (declare the scalar) and an identity
// (there is no honest scalar on every engine).
func TestWithJoins_RejectsADomainTypedField(t *testing.T) {
	for _, c := range []struct{ field, wantKind string }{
		{"OwnerID", "an identity (domain.ID)"},
		{"AltOwnerID", "an identity (domain.ID)"},
		{"VendorCode", "a scalar value object"},
		{"VendorNote", "a scalar value object"},
		{"VendorTier", "an enum value object"},
		{"VendorAddr", "a composite value object"},
	} {
		t.Run(c.field, func(t *testing.T) {
			wantPanic(t, "joinOrder."+c.field+" is "+c.wantKind+", and a join field carries no domain type", func() {
				joinLoader(joinOrderSchema()).WithJoins(
					InnerJoin(joinTargetSchema("customers")).On("customer_id").
						Field(c.field, "nome"))
			})
		})
	}
}

// Each kind must also say what to do instead, and the two remedies are not
// interchangeable: sending an identity at a string is what silently reads 16 raw
// bytes as if they were a uuid on three of the four engines.
func TestWithJoins_DomainTypeRejectionCarriesItsRemedy(t *testing.T) {
	wantPanic(t, "Declare the field as the scalar the column is stored as", func() {
		joinLoader(joinOrderSchema()).WithJoins(
			InnerJoin(joinTargetSchema("customers")).On("customer_id").
				Field("VendorCode", "nome"))
	})
	wantPanic(t, "Declare the field as a plain string: an identity column is read as its canonical text", func() {
		joinLoader(joinOrderSchema()).WithJoins(
			InnerJoin(joinTargetSchema("customers")).On("customer_id").
				Field("OwnerID", "nome"))
	})
	wantPanic(t, "spans SEVERAL columns and a join field maps exactly one", func() {
		joinLoader(joinOrderSchema()).WithJoins(
			InnerJoin(joinTargetSchema("customers")).On("customer_id").
				Field("VendorAddr", "nome"))
	})
}

// The domain-type complaint outranks the nullability one: a non-pointer value
// object on a LEFT join violates both, and the actionable answer is not "make it
// a pointer" — a *vos.Email is just as refused.
func TestWithJoins_DomainTypeOutranksNullability(t *testing.T) {
	wantPanic(t, "a join field carries no domain type", func() {
		joinLoader(joinOrderSchema()).WithJoins(
			LeftJoin(joinTargetSchema("carriers")).On("carrier_id").
				Field("VendorCode", "nome"))
	})
}

// The guard must not widen past domain types: an ordinary scalar, of either
// nullability, is what a join field is FOR.
func TestWithJoins_AcceptsOrdinaryScalarFields(t *testing.T) {
	joinLoader(joinOrderSchema()).WithJoins(
		InnerJoin(joinTargetSchema("customers")).On("customer_id").
			Field("CustomerName", "nome"),
		LeftJoin(joinTargetSchema("carriers")).On("carrier_id").
			Field("CarrierCode", "nome"))
}

// An identity column of the JOINED aggregate has exactly one shape on this side:
// the plain string the framework decodes it into. The declaration is checked at
// construction rather than trusted, because the alternative is a field that
// receives the dialect's stored form — 16 raw bytes on three of the four engines
// — without any error to show for it.
func TestWithJoins_IdentityColumnMustLandInAString(t *testing.T) {
	accept := func(t *testing.T, build func()) {
		t.Helper()
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("declaration must be accepted, got panic: %v", r)
			}
		}()
		build()
	}

	// string for a non-nullable identity under an inner join…
	accept(t, func() {
		joinLoader(joinOrderSchema()).WithJoins(
			InnerJoin(joinTargetSchema("customers")).On("customer_id").
				Field("TargetOwner", "owner_id"))
	})
	// …*string for a nullable one, and for anything under a left join.
	accept(t, func() {
		joinLoader(joinOrderSchema()).WithJoins(
			InnerJoin(joinTargetSchema("customers")).On("customer_id").
				Field("TargetAltOwner", "alt_owner_id"))
	})
	accept(t, func() {
		joinLoader(joinOrderSchema()).WithJoins(
			LeftJoin(joinTargetSchema("carriers")).On("carrier_id").
				Field("TargetAltOwner", "owner_id"))
	})

	// A non-string field is refused, naming what it must be…
	wantPanic(t, "must be string", func() {
		joinLoader(joinOrderSchema()).WithJoins(
			InnerJoin(joinTargetSchema("customers")).On("customer_id").
				Field("TargetBadOwner", "owner_id"))
	})
	// …a nullable identity refuses the non-pointer string…
	wantPanic(t, "must be *string — the column is nullable", func() {
		joinLoader(joinOrderSchema()).WithJoins(
			InnerJoin(joinTargetSchema("customers")).On("customer_id").
				Field("TargetOwner", "alt_owner_id"))
	})
	// …and so does a left join, for its own reason.
	wantPanic(t, "must be *string — a left join produces NULL", func() {
		joinLoader(joinOrderSchema()).WithJoins(
			LeftJoin(joinTargetSchema("carriers")).On("carrier_id").
				Field("TargetOwner", "owner_id"))
	})
}

// The identity column decodes on the way in: the dialect's stored form becomes
// the canonical uuid text, and SQL NULL an absence — reusing the decode domain.ID
// already owns and assigning what it yields.
func TestJoinScanTargets_IdentityColumnDecodesIntoTheString(t *testing.T) {
	const uid = "11111111-2222-3333-4444-555555555555"
	bin := uuid.MustParse(uid)

	order := &joinOrder{}
	js := []Join{{
		Target: joinTargetSchema("customers"),
		Fields: []JoinField{
			{GoField: "TargetOwner", Column: "owner_id"},
			{GoField: "TargetAltOwner", Column: "alt_owner_id"},
			{GoField: "CustomerName", Column: "nome"},
		},
	}}
	got, err := joinScanTargetsFor(order, js)
	if err != nil {
		t.Fatalf("joinScanTargetsFor: %v", err)
	}
	// An ordinary column keeps the raw address it always had.
	if _, ok := got[2].(*string); !ok {
		t.Errorf("CustomerName target = %T, want the plain *string address", got[2])
	}

	// BINARY(16) (mysql/sqlserver) into the required field.
	b := bin
	if err := got[0].(sql.Scanner).Scan(b[:]); err != nil {
		t.Fatalf("scanning a stored identity form: %v", err)
	}
	if order.TargetOwner != uid {
		t.Errorf("TargetOwner = %q, want the canonical text %q", order.TargetOwner, uid)
	}
	// Text form (postgres) into the nullable one.
	if err := got[1].(sql.Scanner).Scan(uid); err != nil {
		t.Fatalf("scanning a text identity: %v", err)
	}
	if order.TargetAltOwner == nil || *order.TargetAltOwner != uid {
		t.Errorf("TargetAltOwner = %v, want %q", order.TargetAltOwner, uid)
	}
	// NULL is an absence on the nullable field…
	if err := got[1].(sql.Scanner).Scan(nil); err != nil {
		t.Fatalf("NULL into a nullable identity join field: %v", err)
	}
	if order.TargetAltOwner != nil {
		t.Errorf("TargetAltOwner = %v, want nil", order.TargetAltOwner)
	}
	// …and a loud error on the required one, never a blank id.
	if err := got[0].(sql.Scanner).Scan(nil); err == nil {
		t.Error("NULL into a non-nullable identity join field must error")
	}
}

// The identity lookup answers for the two shapes a caller can hand it that the
// construction check has already ruled out — a join with no target (the child
// filter builds one), and a column the target does not declare (reported by the
// column check, so this must not speak for it). Both are IDNone: the field keeps
// the plain address it always had, rather than being decoded as an identity.
// Construction guarantees an identity column lands in a string, so the scan's
// own refusal is unreachable through the public surface — reached here directly,
// it must be an error naming the field rather than a panic inside a row loop.
func TestJoinScanTargets_RefusesAnIdentityColumnOnANonStringField(t *testing.T) {
	js := []Join{{
		Target: joinTargetSchema("customers"),
		Fields: []JoinField{{GoField: "TargetBadOwner", Column: "owner_id"}},
	}}
	_, err := joinScanTargetsFor(&joinOrder{}, js)
	if err == nil {
		t.Fatal("an identity column on an int64 field must error")
	}
	if !strings.Contains(err.Error(), "TargetBadOwner") || !strings.Contains(err.Error(), "not a string") {
		t.Errorf("the error must name the field and the reason, got: %v", err)
	}
}

func TestTargetIDKindOf_AnswersNoneOffTheDeclaredSurface(t *testing.T) {
	target := joinTargetSchema("customers")

	if got := targetIDKindOf(Join{}, JoinField{GoField: "X", Column: "owner_id"}); got != core.IDNone {
		t.Errorf("a join with no target = %v, want IDNone", got)
	}
	if got := targetIDKindOf(Join{Target: target}, JoinField{GoField: "X", Column: "nao_existe"}); got != core.IDNone {
		t.Errorf("a column the target does not declare = %v, want IDNone", got)
	}
	// …and the real answer, for both nullabilities.
	if got := targetIDKindOf(Join{Target: target}, JoinField{Column: "owner_id"}); got != core.IDValue {
		t.Errorf("owner_id = %v, want IDValue", got)
	}
	if got := targetIDKindOf(Join{Target: target}, JoinField{Column: "alt_owner_id"}); got != core.IDPointer {
		t.Errorf("alt_owner_id = %v, want IDPointer", got)
	}
	if got := targetIDKindOf(Join{Target: target}, JoinField{Column: "nome"}); got != core.IDNone {
		t.Errorf("an ordinary column = %v, want IDNone", got)
	}
}

// A criteria field is typed across anchor, siblings, shared base and then the
// declared ROOT joins. The join leg is what makes a probe on an identity join
// field bind in the form the target stores; a child join is load-only and must
// not answer, or a filter would be typed for a field no predicate can reach.
func TestIdKindResolver_TypesRootJoinFieldsAndNothingElse(t *testing.T) {
	l := joinLoader(joinOrderSchema()).WithJoins(
		InnerJoin(joinTargetSchema("customers")).On("customer_id").
			Field("TargetOwner", "owner_id").
			Field("CustomerName", "nome"),
		LeftJoinInChild(joinLineSchema()).To(joinTargetSchema("cities")).On("city_id").
			Field("StateName", "nome"),
	)
	idKind := l.idKindResolver()

	if got := idKind("TargetOwner"); got != core.IDValue {
		t.Errorf("a root join field over an identity column = %v, want IDValue", got)
	}
	if got := idKind("CustomerName"); got != core.IDNone {
		t.Errorf("a root join field over an ordinary column = %v, want IDNone", got)
	}
	if got := idKind("StateName"); got != core.IDNone {
		t.Errorf("a CHILD join field must not be typed by the root resolver, got %v", got)
	}
	if got := idKind("Nope"); got != core.IDNone {
		t.Errorf("an unknown field = %v, want IDNone", got)
	}
	// The anchor still answers first, for its own fields and the managed slot.
	if got := idKind("CustomerID"); got != core.IDValue {
		t.Errorf("the anchor's own identity field = %v, want IDValue", got)
	}
	if got := idKind("ID"); got != core.IDValue {
		t.Errorf("the managed ID slot = %v, want IDValue", got)
	}
}

// The value's own nullability, independent of the join kind. An inner join
// proves the joined ROW exists — never that every column of it is filled — so a
// column the TARGET declares nullable must land in a field that can say
// "absent". The identity rule above is the special case; this is the general one.
// The nullability lookup only speaks where the target's own struct can back the
// answer. Off that surface — no target, a column the target does not declare, a
// type-less external schema, or a managed slot that is not a struct field — it
// reports NOT nullable rather than guess, so the rule never invents a constraint
// it cannot point at. The managed ID slot is the one exception: the schema
// recorded that typing itself.
func TestTargetColumnNullability_OnlySpeaksWhereTheTargetCanBackIt(t *testing.T) {
	target := joinTargetSchema("customers")

	for _, c := range []struct {
		name string
		j    Join
		f    JoinField
		want bool
	}{
		{"no target", Join{}, JoinField{Column: "nickname"}, false},
		{"column the target does not declare", Join{Target: target}, JoinField{Column: "nao_existe"}, false},
		{"type-less external schema, column undeclared", Join{Target: NewExternalSchema("upstream")}, JoinField{Column: "nickname"}, false},
		// Declared, but on a schema with no Go type behind it: there is no field to
		// read a pointer off, so the rule stays silent instead of guessing.
		{"type-less external schema, column declared",
			Join{Target: NewExternalSchema("upstream").Field("Nick", "nickname")},
			JoinField{Column: "nickname"}, false},
		{"a non-nullable column", Join{Target: target}, JoinField{Column: "nome"}, false},
		{"a nullable column", Join{Target: target}, JoinField{Column: "nickname"}, true},
		{"a nullable identity", Join{Target: target}, JoinField{Column: "alt_owner_id"}, true},
		// A MANAGED slot resolves as a column but is not a struct field (the
		// carrier's slots are unexported), so there is nothing to read a pointer
		// off — not nullable, rather than a guess from the column's meaning.
		{"a managed slot", Join{Target: collidingTargetSchema("customers")}, JoinField{Column: "deleted_at"}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, _, got := targetColumnNullability(c.j, c.f); got != c.want {
				t.Errorf("nullable = %v, want %v", got, c.want)
			}
		})
	}

	// The message fragment is empty wherever the declaration cannot be rendered,
	// so the error never trails a dangling "( . is <nil>)".
	if got := targetDeclaration(Join{}, "X", nil); got != "" {
		t.Errorf("targetDeclaration with no target = %q, want empty", got)
	}
	if got := targetDeclaration(Join{Target: NewExternalSchema("upstream")}, "X", nil); got != "" {
		t.Errorf("targetDeclaration on a type-less schema = %q, want empty", got)
	}
}

func TestWithJoins_ANullableTargetColumnNeedsANullableField(t *testing.T) {
	// Accepted: the pointer can hold the absence.
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("a nullable column into a pointer must be accepted, got: %v", r)
			}
		}()
		joinLoader(joinOrderSchema()).WithJoins(
			InnerJoin(joinTargetSchema("customers")).On("customer_id").
				Field("TargetNick", "nickname"))
	}()

	// Refused: the message must name BOTH sides — the field that cannot hold it
	// and the table/column that produces it, with the target's own declaration.
	wantPanic(t, `joinOrder.TargetNickBad is string and cannot hold NULL, but "nickname" is nullable on "customers" (joinCustomer.Nickname is *string)`, func() {
		joinLoader(joinOrderSchema()).WithJoins(
			InnerJoin(joinTargetSchema("customers")).On("customer_id").
				Field("TargetNickBad", "nickname"))
	})

	// A NON-nullable column keeps taking a plain field: the rule adds nothing
	// where the value cannot be absent.
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("a non-nullable column into a plain field must be accepted, got: %v", r)
			}
		}()
		joinLoader(joinOrderSchema()).WithJoins(
			InnerJoin(joinTargetSchema("customers")).On("customer_id").
				Field("CustomerName", "nome"))
	}()
}

func TestWithJoins_RejectsALeftJoinOntoANonNullableField(t *testing.T) {
	wantPanic(t, "cannot hold the NULL a left join", func() {
		joinLoader(joinOrderSchema()).WithJoins(
			LeftJoin(joinTargetSchema("carriers")).On("carrier_id").Field("CustomerName", "nome"),
		)
	})
}

// ─── the child guard ─────────────────────────────────────────────────────────

func TestWithJoins_RejectsAChildTheRootDoesNotDeclare(t *testing.T) {
	stranger := NewTableSchema[joinLine]("outra_tabela").ID("id").ParentID("x_id").Field("Label", "label")
	wantPanic(t, "declare the join on the base's own repository", func() {
		joinLoader(joinOrderSchema()).WithJoins(
			LeftJoinInChild(stranger).To(joinTargetSchema("cities")).On("city_id").Field("StateName", "nome"),
		)
	})
}

// A child join validates its Go field against the CHILD's struct, not the root's.
func TestWithJoins_ChildFieldIsCheckedAgainstTheChild(t *testing.T) {
	wantPanic(t, "has no field \"CustomerName\"", func() {
		joinLoader(joinOrderSchema()).WithJoins(
			LeftJoinInChild(joinLineSchema()).To(joinTargetSchema("cities")).
				On("city_id").Field("CustomerName", "nome"),
		)
	})
}

func TestWithJoins_ChildFKIsCheckedAgainstTheChild(t *testing.T) {
	wantPanic(t, "is not a column of \"order_lines\"", func() {
		joinLoader(joinOrderSchema()).WithJoins(
			LeftJoinInChild(joinLineSchema()).To(joinTargetSchema("cities")).
				On("customer_id").Field("StateName", "nome"),
		)
	})
}

func TestWithJoins_ChildInnerJoinOverAMandatoryFKIsAccepted(t *testing.T) {
	l := joinLoader(joinOrderSchema()).WithJoins(
		InnerJoinInChild(joinLineSchema()).To(joinTargetSchema("cities")).
			On("city_id").Field("CityName", "nome"),
	)
	if len(l.Joins()) != 1 {
		t.Fatal("a child inner join over a non-nullable FK must be accepted")
	}
}

// ─── the repository seat ─────────────────────────────────────────────────────

func TestRepositoryWithJoins_ThreadsIntoTheLoader(t *testing.T) {
	r := NewBaseAggregateRepository[*joinOrder](nil, func() *joinOrder { return &joinOrder{} })
	r.WithSchema(joinOrderSchema())
	r.WithJoins(
		InnerJoin(joinTargetSchema("customers")).On("customer_id").Field("CustomerName", "nome"),
	)
	if got := len(r.Loader.Joins()); got != 1 {
		t.Fatalf("the repository must thread the join into the loader, got %d", got)
	}
}

func TestJoinKind_String(t *testing.T) {
	if JoinInner.String() != "InnerJoin" || JoinLeft.String() != "LeftJoin" {
		t.Errorf("kinds = %q / %q", JoinInner, JoinLeft)
	}
}

// ─── the SQL a declared join produces ────────────────────────────────────────

// capturedSQL runs a FindAll against a fake engine that records the statement and
// returns no rows, so the emitted SQL is assertable without a database.
func capturedSQL(t *testing.T, l *AggregateLoader[*joinOrder], q *criteria.Query) string {
	t.Helper()
	var seen string
	eng := fakeEngine(func(sql string, _ []any) (Rows, error) {
		if seen == "" {
			seen = sql
		}
		return &fakeDBRows{}, nil
	})
	l.eng = eng
	if _, err := l.FindAll(context.Background(), q); err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	return seen
}

func joinedLoader(t *testing.T) *AggregateLoader[*joinOrder] {
	t.Helper()
	return joinLoader(joinOrderSchema()).WithJoins(
		InnerJoin(joinTargetSchema("customers")).On("customer_id").Field("CustomerName", "nome"),
		LeftJoin(joinTargetSchema("carriers")).On("carrier_id").Field("CarrierCode", "nome"),
	)
}

// A declared root join is ALWAYS in the FROM, and its columns ride the ROOT
// SELECT — no second round trip. Each traversal gets its own alias, because two
// of them may reach the same table.
func TestDeclaredJoin_IsAlwaysInTheFromAndSelectsItsFields(t *testing.T) {
	sql := capturedSQL(t, joinedLoader(t), criteria.Where(nil))

	if !strings.Contains(sql, "INNER JOIN customers j_customer_id ON j_customer_id.id = orders.customer_id") {
		t.Errorf("the inner join must be emitted under its alias:\n%s", sql)
	}
	if !strings.Contains(sql, "LEFT JOIN carriers j_carrier_id ON j_carrier_id.id = orders.carrier_id") {
		t.Errorf("the left join must be emitted under its alias:\n%s", sql)
	}
	if !strings.Contains(sql, "j_customer_id.nome") || !strings.Contains(sql, "j_carrier_id.nome") {
		t.Errorf("both join columns must ride the root SELECT:\n%s", sql)
	}
}

// A table alias is written BARE, never with "AS". The keyword is optional there
// in standard SQL and Oracle rejects it outright (ORA-02000: missing ON or USING
// keyword), so emitting it makes every read through a loader that declares a join
// fail on one of the four supported backends — including FindByID, which the
// write-side handlers load through.
func TestDeclaredJoin_TableAliasCarriesNoASKeyword(t *testing.T) {
	sql := capturedSQL(t, joinedLoader(t), criteria.Where(nil))
	if strings.Contains(sql, " AS ") {
		t.Errorf("a table alias must not be introduced with AS (Oracle rejects it):\n%s", sql)
	}
}

func TestChildJoin_TableAliasCarriesNoASKeyword(t *testing.T) {
	l := joinLoader(joinOrderSchema()).WithJoins(
		InnerJoinInChild(joinLineSchema()).To(joinTargetSchema("cities")).On("city_id").
			Field("CityName", "nome"),
	)
	if sql := childCapturedSQL(t, l); strings.Contains(sql, " AS ") {
		t.Errorf("a child join's table alias must not be introduced with AS:\n%s", sql)
	}
}

// The declared kind survives into the statement: LEFT preserves the root, INNER
// requires the match. Rendering one as the other would silently change the
// result set.
func TestDeclaredJoin_KindReachesTheStatement(t *testing.T) {
	sql := capturedSQL(t, joinedLoader(t), criteria.Where(nil))
	inner := strings.Index(sql, "INNER JOIN customers")
	left := strings.Index(sql, "LEFT JOIN carriers")
	if inner < 0 || left < 0 {
		t.Fatalf("both kinds must appear:\n%s", sql)
	}
}

// A joined column is addressable in a filter, and comes back QUALIFIED — an
// unqualified "nome" would be ambiguous between the two joined tables.
func TestDeclaredJoin_FilterResolvesQualified(t *testing.T) {
	sql := capturedSQL(t, joinedLoader(t), criteria.Where(criteria.Eq("CustomerName", "ana")))
	if !strings.Contains(sql, "j_customer_id.nome = $1") {
		t.Errorf("the filter must resolve to the alias-qualified column:\n%s", sql)
	}
}

func TestDeclaredJoin_OrderResolvesQualified(t *testing.T) {
	sql := capturedSQL(t, joinedLoader(t), criteria.Where(nil).OrderByDesc("CarrierCode"))
	if !strings.Contains(sql, "ORDER BY j_carrier_id.nome DESC") {
		t.Errorf("the order must resolve to the alias-qualified column:\n%s", sql)
	}
}

// Under a join every table has an "id", so the anchor's must be qualified —
// otherwise a by-id read or the ordering tiebreak is ambiguous.
func TestDeclaredJoin_QualifiesTheAnchorID(t *testing.T) {
	sql := capturedSQL(t, joinedLoader(t), criteria.ByID(domain.NewID("11111111-1111-1111-1111-111111111111")))
	if !strings.Contains(sql, "orders.id = $1") {
		t.Errorf("the anchor id must be qualified under a join:\n%s", sql)
	}
}

// The SCHEMA always wins: declaring a join can never change the meaning of a
// name the entity already answered.
func TestDeclaredJoin_SchemaFieldsStillResolveToTheAnchor(t *testing.T) {
	sql := capturedSQL(t, joinedLoader(t), criteria.Where(criteria.Eq("Code", "X")))
	if strings.Contains(sql, "j_customer_id.code") || strings.Contains(sql, "j_carrier_id.code") {
		t.Errorf("an anchor field must not resolve through a join:\n%s", sql)
	}
	if !strings.Contains(sql, "orders.code = $1") {
		t.Errorf("the anchor field must resolve on the anchor, qualified to it:\n%s", sql)
	}
}

// A loader with no joins declared emits exactly what it emitted before: no JOIN,
// no extra column, no qualification.
func TestNoDeclaredJoins_LeavesTheStatementUntouched(t *testing.T) {
	sql := capturedSQL(t, joinLoader(joinOrderSchema()), criteria.Where(criteria.Eq("Code", "X")))
	if strings.Contains(sql, "JOIN") {
		t.Errorf("no join declared must emit no JOIN:\n%s", sql)
	}
	if !strings.Contains(sql, "SELECT id, code, customer_id, carrier_id, revision FROM orders") {
		t.Errorf("the SELECT must be anchor-only:\n%s", sql)
	}
}

// A criteria naming neither the schema nor any declared join still fails — a
// join does not widen the vocabulary beyond what it declared.
func TestDeclaredJoin_UnknownFieldStillFails(t *testing.T) {
	l := joinedLoader(t)
	l.eng = fakeEngine(func(string, []any) (Rows, error) { return &fakeDBRows{}, nil })
	_, err := l.FindAll(context.Background(), criteria.Where(criteria.Eq("Fantasma", "x")))
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("an undeclared name must still fail, got %v", err)
	}
}

// Two joins on the same owner cannot share a foreign key: the alias is derived
// from it, and a column pointing at two tables is a modelling mistake.
func TestWithJoins_RejectsTwoJoinsOnTheSameFK(t *testing.T) {
	wantPanic(t, "one foreign key reaches ONE table", func() {
		joinLoader(joinOrderSchema()).WithJoins(
			LeftJoin(joinTargetSchema("carriers")).On("carrier_id").Field("CarrierCode", "nome"),
			LeftJoin(joinTargetSchema("outros")).On("carrier_id").Field("CarrierCode", "nome"),
		)
	})
}

// ─── the aggregate DSL over a joined column ──────────────────────────────────

// aggCapturedSQL records the single statement an Aggregate/AggregateBy call
// issues against a fake engine.
func aggCapturedSQL(t *testing.T, l *AggregateLoader[*joinOrder], run func(*AggregateLoader[*joinOrder]) error) string {
	t.Helper()
	var seen string
	l.eng = fakeEngine(func(sql string, _ []any) (Rows, error) {
		if seen == "" {
			seen = sql
		}
		return &fakeDBRows{}, nil
	})
	if err := run(l); err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	return seen
}

// An aggregate OVER a joined column must be alias-qualified like every other
// resolution. The fixture's two joins both map a column called "nome", so an
// unqualified MAX(nome) is ambiguous in a FROM that holds both — the backend
// rejects the statement outright.
func TestDeclaredJoin_AggregateResolvesQualified(t *testing.T) {
	sql := aggCapturedSQL(t, joinedLoader(t), func(l *AggregateLoader[*joinOrder]) error {
		return l.Aggregate(context.Background(), criteria.Where(nil), MaxInt("CustomerName"))
	})
	if !strings.Contains(sql, "MAX(j_customer_id.nome)") {
		t.Errorf("the aggregate expression must resolve to the alias-qualified column:\n%s", sql)
	}
}

// The grouping key and the aggregate expression of the SAME call resolve through
// one rule — a statement qualifying one and not the other is the drift this
// pins.
func TestDeclaredJoin_AggregateByResolvesBothHalvesQualified(t *testing.T) {
	sql := aggCapturedSQL(t, joinedLoader(t), func(l *AggregateLoader[*joinOrder]) error {
		_, err := l.AggregateBy(context.Background(), criteria.Where(nil), By("Code"), MaxInt("CarrierCode"))
		return err
	})
	if !strings.Contains(sql, "MAX(j_carrier_id.nome)") {
		t.Errorf("the aggregate expression must be alias-qualified:\n%s", sql)
	}
	if !strings.Contains(sql, "GROUP BY orders.code") {
		t.Errorf("the anchor grouping key must carry its table under a join:\n%s", sql)
	}
}

// An anchor column is qualified TOO once a join is in the FROM. The bijection
// that lets a sibling column stay bare covers the anchor's own NODE and nothing
// else: a joined aggregate is a foreign namespace, free to carry a "code" of its
// own, and the backend rejects the ambiguous reference outright.
func TestAggregate_AnchorColumnIsQualifiedUnderAJoin(t *testing.T) {
	sql := aggCapturedSQL(t, joinedLoader(t), func(l *AggregateLoader[*joinOrder]) error {
		return l.Aggregate(context.Background(), criteria.Where(nil), MaxInt("Code"))
	})
	if !strings.Contains(sql, "MAX(orders.code)") {
		t.Errorf("an anchor column must carry its table under a join:\n%s", sql)
	}
}

// …and it stays bare with no join declared: the qualification is triggered by
// what is in the FROM, so a loader without joins emits exactly what it always did.
func TestAggregate_AnchorColumnStaysBareWithoutAJoin(t *testing.T) {
	sql := aggCapturedSQL(t, joinLoader(joinOrderSchema()), func(l *AggregateLoader[*joinOrder]) error {
		return l.Aggregate(context.Background(), criteria.Where(nil), MaxInt("Code"))
	})
	if !strings.Contains(sql, "MAX(code)") {
		t.Errorf("with no join declared the anchor column needs no qualifier:\n%s", sql)
	}
}

// ─── the child SELECT ────────────────────────────────────────────────────────

// childCapturedSQL returns the statement the CHILD hydration issued: the second
// query a FindAll runs when the aggregate declares children.
func childCapturedSQL(t *testing.T, l *AggregateLoader[*joinOrder]) string {
	t.Helper()
	var childSQL string
	l.eng = fakeEngine(func(sql string, _ []any) (Rows, error) {
		if strings.Contains(sql, "order_lines") {
			childSQL = sql
			return &fakeDBRows{}, nil
		}
		// One root row, so the child hydration runs at all.
		return &fakeDBRows{rows: 1, scan: func(_ int, dest []any) error {
			if len(dest) > 0 {
				if p, ok := dest[0].(*string); ok {
					*p = "11111111-1111-1111-1111-111111111111"
				}
			}
			return nil
		}}, nil
	})
	if _, err := l.FindAll(context.Background(), criteria.Where(nil)); err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	return childSQL
}

// A child join rides the CHILD's SELECT under its own alias — never the root's.
func TestChildJoin_RidesTheChildSelect(t *testing.T) {
	l := joinLoader(joinOrderSchema()).WithJoins(
		InnerJoinInChild(joinLineSchema()).To(joinTargetSchema("cities")).On("city_id").Field("CityName", "nome"),
	)
	sql := childCapturedSQL(t, l)
	if !strings.Contains(sql, "INNER JOIN cities j_city_id ON j_city_id.id = order_lines.city_id") {
		t.Errorf("the child join must be emitted on the child statement:\n%s", sql)
	}
	if !strings.Contains(sql, "j_city_id.nome") {
		t.Errorf("the child join column must ride the child SELECT:\n%s", sql)
	}
	if !strings.Contains(sql, "order_lines.label") {
		t.Errorf("under a join the child's own columns must be qualified:\n%s", sql)
	}
}

// A child join must NOT reach the root statement: the root's vocabulary is
// unchanged by it, which is what keeps a child field unfilterable at the root.
func TestChildJoin_DoesNotTouchTheRootStatement(t *testing.T) {
	l := joinLoader(joinOrderSchema()).WithJoins(
		InnerJoinInChild(joinLineSchema()).To(joinTargetSchema("cities")).On("city_id").Field("CityName", "nome"),
	)
	sql := capturedSQL(t, l, criteria.Where(nil))
	if strings.Contains(sql, "cities") {
		t.Errorf("a child join must not appear in the root statement:\n%s", sql)
	}
}

func TestChildJoin_FieldIsNotAddressableAtTheRoot(t *testing.T) {
	l := joinLoader(joinOrderSchema()).WithJoins(
		InnerJoinInChild(joinLineSchema()).To(joinTargetSchema("cities")).On("city_id").Field("CityName", "nome"),
	)
	l.eng = fakeEngine(func(string, []any) (Rows, error) { return &fakeDBRows{}, nil })
	_, err := l.FindAll(context.Background(), criteria.Where(criteria.Eq("CityName", "porto")))
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("a child join field must not be addressable at the root, got %v", err)
	}
}

// ─── the port the read models consult ────────────────────────────────────────

// JoinFields is how a read model learns what a loader can serve beyond the
// schema: keyed by the table the fields land on, so the root's and each child's
// stay apart.
func TestJoinFields_GroupsByTheTableTheyLandOn(t *testing.T) {
	l := joinLoader(joinOrderSchema()).WithJoins(
		InnerJoin(joinTargetSchema("customers")).On("customer_id").Field("CustomerName", "nome"),
		LeftJoin(joinTargetSchema("carriers")).On("carrier_id").Field("CarrierCode", "nome"),
		InnerJoinInChild(joinLineSchema()).To(joinTargetSchema("cities")).On("city_id").Field("CityName", "nome"),
	)
	got := l.JoinFields()
	if len(got["orders"]) != 2 {
		t.Errorf("the root's fields = %v, want two", got["orders"])
	}
	if len(got["order_lines"]) != 1 || got["order_lines"][0] != "CityName" {
		t.Errorf("the child's fields = %v, want [CityName]", got["order_lines"])
	}
}

func TestJoinFields_NilWhenNothingIsDeclared(t *testing.T) {
	if got := joinLoader(joinOrderSchema()).JoinFields(); got != nil {
		t.Errorf("no joins declared must report nil, got %v", got)
	}
}

// The scan destinations are built by reflection at row time. The declaration was
// proven at construction, so a failure here is a framework bug — it surfaces as
// an error rather than a panic inside a row loop.
func TestJoinScanTargets_RefusesAnImpossibleDestination(t *testing.T) {
	js := []Join{{Fields: []JoinField{{GoField: "CustomerName", Column: "nome"}}}}

	if _, err := joinScanTargetsFor("not a struct", js); err == nil {
		t.Error("a non-struct destination must error")
	}
	if _, err := joinScanTargetsFor(&struct{ Other string }{}, js); err == nil {
		t.Error("a destination without the field must error")
	}
	if _, err := joinScanTargetsFor(&joinOrder{}, js); err != nil {
		t.Errorf("a valid destination must build targets: %v", err)
	}
}

func TestJoinScanTargets_EmptyWithoutRootJoins(t *testing.T) {
	got, err := joinScanTargets(&joinOrder{}, []Join{{Child: joinLineSchema()}})
	if err != nil {
		t.Fatalf("joinScanTargets: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("a child join contributes no ROOT scan target, got %d", len(got))
	}
}

// ─── a join target that looks like a real entity ─────────────────────────────

// The joined aggregate the tests above use is deliberately thin. A REAL one
// shares column names with the anchor (both have a "name") and carries the
// framework's own managed columns (every archivable entity has deleted_at), so
// nothing about the anchor side may be emitted bare while it is in the FROM.
func collidingTargetSchema(table string) *TableSchema {
	return NewTableSchema[*joinCustomer](table).ID("id").
		Field("Name", "code"). // the SAME column the anchor has
		DeletedAt("deleted_at")
}

func collidingOrderSchema() *TableSchema {
	return NewTableSchema[*joinOrder]("orders").
		ID("id").
		Revision("revision").
		DeletedAt("deleted_at").
		Field("Code", "code").
		Field("CustomerID", "customer_id").
		Field("CarrierID", "carrier_id").
		Child(collidingLineSchema())
}

func collidingLineSchema() *TableSchema {
	return NewTableSchema[joinLine]("order_lines").
		ID("id").ParentID("order_id").
		Field("Label", "label").
		Field("CityID", "city_id").
		DeletedAt("deleted_at")
}

// Every anchor column in the root SELECT is qualified once a join is in the FROM
// — not just the id and the managed columns. An anchor "code" next to the
// target's "code" is ambiguous, and it breaks the PLAIN listing, with no filter
// in sight.
func TestDeclaredJoin_RootSelectQualifiesEveryAnchorColumn(t *testing.T) {
	l := joinLoader(collidingOrderSchema()).WithJoins(
		InnerJoin(collidingTargetSchema("customers")).On("customer_id").Field("CustomerName", "code"),
	)
	sql := capturedSQL(t, l, criteria.Where(nil))
	for _, want := range []string{"orders.code", "orders.customer_id", "orders.carrier_id"} {
		if !strings.Contains(sql, want) {
			t.Errorf("the root SELECT must qualify %q under a join:\n%s", want, sql)
		}
	}
	if strings.Contains(sql, ", code,") || strings.HasSuffix(sql, ", code") {
		t.Errorf("no anchor column may ride the SELECT bare under a join:\n%s", sql)
	}
}

// The anchor's soft-delete gate under a join target that also has deleted_at.
func TestDeclaredJoin_RootScopeGateIsQualified(t *testing.T) {
	l := joinLoader(collidingOrderSchema()).WithJoins(
		InnerJoin(collidingTargetSchema("customers")).On("customer_id").Field("CustomerName", "code"),
	)
	sql := capturedSQL(t, l, criteria.Where(nil))
	if !strings.Contains(sql, "orders.deleted_at IS NULL") {
		t.Errorf("the scope gate must be qualified under a join:\n%s", sql)
	}
}

// The CHILD's soft-delete gate, same story: a child join target with a
// deleted_at of its own makes the bare gate ambiguous. This one broke every
// read of an aggregate whose child joined an ordinary archivable entity.
func TestChildJoin_ChildScopeGateIsQualified(t *testing.T) {
	l := joinLoader(collidingOrderSchema()).WithJoins(
		InnerJoinInChild(collidingLineSchema()).To(collidingTargetSchema("cities")).
			On("city_id").Field("CityName", "code"),
	)
	sql := childCapturedSQL(t, l)
	if !strings.Contains(sql, "order_lines.deleted_at IS NULL") {
		t.Errorf("the child scope gate must be qualified under a child join:\n%s", sql)
	}
}

// The filter and the order over an anchor column that the target also has.
func TestDeclaredJoin_AnchorFilterAndOrderAreQualified(t *testing.T) {
	l := joinLoader(collidingOrderSchema()).WithJoins(
		InnerJoin(collidingTargetSchema("customers")).On("customer_id").Field("CustomerName", "code"),
	)
	sql := capturedSQL(t, l, criteria.Where(criteria.Eq("Code", "X")).OrderByDesc("Code"))
	if !strings.Contains(sql, "orders.code = $1") {
		t.Errorf("the anchor predicate must be qualified under a join:\n%s", sql)
	}
	if !strings.Contains(sql, "ORDER BY orders.code DESC") {
		t.Errorf("the anchor order must be qualified under a join:\n%s", sql)
	}
}
