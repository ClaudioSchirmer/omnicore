package read

import (
	"context"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
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
}

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
	return NewTableSchema[*joinCustomer](table).ID("id").Field("Name", "nome")
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
	if !strings.Contains(sql, "code = $1") {
		t.Errorf("the anchor field must resolve on the anchor:\n%s", sql)
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
	if !strings.Contains(sql, "GROUP BY code") {
		t.Errorf("an anchor grouping key stays unqualified:\n%s", sql)
	}
}

// An anchor column keeps resolving unqualified — the sibling/base bijection makes
// it unambiguous, and qualifying it would be noise.
func TestAggregate_AnchorColumnStaysUnqualified(t *testing.T) {
	sql := aggCapturedSQL(t, joinedLoader(t), func(l *AggregateLoader[*joinOrder]) error {
		return l.Aggregate(context.Background(), criteria.Where(nil), MaxInt("Code"))
	})
	if !strings.Contains(sql, "MAX(code)") {
		t.Errorf("an anchor column needs no qualifier:\n%s", sql)
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
