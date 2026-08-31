package write

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/db/criteria"
)

// A stamped field splits one timestamp in two: the DOMAIN owns the moment (a
// rule decides an order was just paid), the FRAMEWORK owns the value (the write
// operation's own instant). These tests pin both halves and, above all, the
// silence in between — a write that did not ask for the stamp must not mention
// the column at all.

type stampedOrder struct {
	domain.BaseEntity
	Status     string
	PaidAt     *time.Time
	CanceledAt *time.Time
}

func (o *stampedOrder) EntityName() string { return "Order" }

func stampedOrderSchema() *TableSchema {
	return core.NewTableSchema[*stampedOrder]("orders").
		ID("id").
		Field("Status", "status").
		StampedTimeField("PaidAt", "paid_at").
		StampedTimeField("CanceledAt", "canceled_at").
		UpdatedAt("updated_at")
}

// The value never comes from the struct — that is what makes the column
// trustworthy. A field set by hand is simply not written.
func TestStampedField_NeverWrittenFromTheStruct(t *testing.T) {
	lie := time.Date(1999, 1, 1, 0, 0, 0, 0, time.UTC)
	o := &stampedOrder{Status: "NEW", PaidAt: &lie}
	fields := stampedOrderSchema().WriteFields(o)
	if _, present := fields["paid_at"]; present {
		t.Fatal("a stamped column must never be bound from the entity's own value")
	}
	if fields["status"] != "NEW" {
		t.Fatalf("plain fields must still be written, got %v", fields["status"])
	}
}

// Not asking is not the same as asking for NULL: an untouched stamped column
// stays out of the statement, which is what lets an already-stamped row keep its
// value with no caller having to preserve it.
func TestStampedField_UnrequestedColumnIsAbsentFromTheStatement(t *testing.T) {
	schema := stampedOrderSchema()
	o := &stampedOrder{Status: "NEW"}
	plan, err := stampedCols(schema, o, schema.UpdateNowColumns(), testStamp())
	cols := plan.nowCols
	if err != nil {
		t.Fatalf("stampedCols: %v", err)
	}
	sql, _, err := buildUpdate(testPGDialect{}, schemaTarget(schema),
		criteria.Eq(idGoField, domain.NewID("o1")), schema.WriteFields(o), cols, testStamp(), "", 0)
	if err != nil {
		t.Fatalf("buildUpdate: %v", err)
	}
	if strings.Contains(sql, "paid_at") || strings.Contains(sql, "canceled_at") {
		t.Fatalf("an unrequested stamped column must not appear in the statement: %s", sql)
	}
	if !strings.Contains(sql, "updated_at") {
		t.Fatalf("the managed timestamps are unaffected: %s", sql)
	}
}

// A requested stamp binds the operation's instant — the SAME value the managed
// timestamps carry, so the row, the audit event and the payload agree.
func TestStampedField_RequestedBindsTheOperationInstant(t *testing.T) {
	schema := stampedOrderSchema()
	o := &stampedOrder{Status: "PAID"}
	o.Stamp("PaidAt")

	plan, err := stampedCols(schema, o, schema.UpdateNowColumns(), testStamp())
	cols := plan.nowCols
	if err != nil {
		t.Fatalf("stampedCols: %v", err)
	}
	now := testStamp()
	sql, args, err := buildUpdate(testPGDialect{}, schemaTarget(schema),
		criteria.Eq(idGoField, domain.NewID("o1")), schema.WriteFields(o), cols, now, "", 0)
	if err != nil {
		t.Fatalf("buildUpdate: %v", err)
	}
	if !strings.Contains(sql, "paid_at") {
		t.Fatalf("the requested stamp must be in the SET list: %s", sql)
	}
	if strings.Contains(sql, "canceled_at") {
		t.Fatalf("only the REQUESTED stamp is written: %s", sql)
	}
	var seen int
	for _, a := range args {
		if ts, ok := a.(time.Time); ok && ts.Equal(now) {
			seen++
		}
	}
	// updated_at + paid_at, both bound to the one operation instant.
	if seen != 2 {
		t.Fatalf("stamped and managed columns must bind the same instant; matched %d args of %v", seen, args)
	}
}

// The request is per-write intent, not entity state: asking twice is asking
// once, and the order of the emitted columns follows the DECLARATION so the SQL
// is stable however the rules happened to fire.
func TestStampedField_RequestsAreDeduplicatedAndDeclarationOrdered(t *testing.T) {
	schema := stampedOrderSchema()
	o := &stampedOrder{}
	o.Stamp("CanceledAt")
	o.Stamp("PaidAt")
	o.Stamp("CanceledAt")

	plan, err := stampedCols(schema, o, nil, testStamp())
	cols := plan.nowCols
	if err != nil {
		t.Fatalf("stampedCols: %v", err)
	}
	if len(cols) != 2 || cols[0] != "paid_at" || cols[1] != "canceled_at" {
		t.Fatalf("want declaration order [paid_at canceled_at], got %v", cols)
	}
}

// The domain cannot see the schema, so a bad name has no boot moment to be
// caught at — it must be a loud write-time error, never a silent no-op.
func TestStampedField_UnknownAndPlainRequestsAreRefused(t *testing.T) {
	schema := stampedOrderSchema()

	o := &stampedOrder{}
	o.Stamp("PadiAt") // typo
	_, err := stampedCols(schema, o, nil, testStamp())
	if err == nil || !strings.Contains(err.Error(), "no stamped field") {
		t.Fatalf("a misspelled stamp must be refused, got %v", err)
	}

	p := &stampedOrder{}
	p.Stamp("Status") // mapped, but plain
	_, err = stampedCols(schema, p, nil, testStamp())
	if err == nil || !strings.Contains(err.Error(), "plain field") {
		t.Fatalf("stamping a plain field must be refused, got %v", err)
	}
}

// A schema declaring no stamped field is not a licence to ignore the request:
// asking for something that cannot happen is a mistake, and the framework refuses
// silent no-ops everywhere else. With NO request the schema costs nothing.
func TestStampedField_SchemaWithoutStampsRefusesARequest(t *testing.T) {
	plain := core.NewTableSchema[*stampedOrder]("orders").ID("id").Field("Status", "status")
	if plain.HasStampedFields() {
		t.Fatal("a schema with no StampedTimeField must report none")
	}

	// Nothing asked → the managed columns pass through untouched.
	plan, err := stampedCols(plain, &stampedOrder{}, []string{"updated_at"}, testStamp())
	if err != nil {
		t.Fatalf("an unasked write must not fail: %v", err)
	}
	if len(plan.nowCols) != 1 || plan.nowCols[0] != "updated_at" {
		t.Fatalf("the managed columns must pass through untouched, got %v", plan.nowCols)
	}

	// Asked → refused, naming the field.
	o := &stampedOrder{}
	o.Stamp("PaidAt")
	if _, err := stampedCols(plain, o, []string{"updated_at"}, testStamp()); err == nil ||
		!strings.Contains(err.Error(), "no stamped field") {
		t.Fatalf("a request against a schema that declares none must be refused, got %v", err)
	}
}

// A shared-base ROLE carries the requests for two tables at once, so neither
// schema may refuse on its own: claiming and refusing are separate steps, and the
// refusal only fires on what NEITHER claimed.
func TestStampedField_ClaimSplitsRequestsBetweenSchemas(t *testing.T) {
	role := core.NewTableSchema[*stampedOrder]("orders").
		ID("id").
		Field("Status", "status").
		StampedTimeField("PaidAt", "paid_at")

	cols, unclaimed := role.ClaimStampColumns([]string{"PaidAt", "VerifiedAt", "Nope"})
	if len(cols) != 1 || cols[0] != "paid_at" {
		t.Fatalf("the schema must claim its own, got %v", cols)
	}
	if len(unclaimed) != 2 || unclaimed[0] != "VerifiedAt" || unclaimed[1] != "Nope" {
		t.Fatalf("what it does not declare must come back for someone else, got %v", unclaimed)
	}
	if err := role.RefuseUnclaimedStamps(nil); err != nil {
		t.Fatalf("no leftovers, no refusal: %v", err)
	}
	if err := role.RefuseUnclaimedStamps([]string{"Nope"}); err == nil {
		t.Fatal("a name nobody claimed must be refused")
	}
}

// ---------------------------------------------------------------------------
// The Direct side — same contract, marker instead of an entity
// ---------------------------------------------------------------------------

type stampedJobRow struct {
	ID     domain.ID
	Status string
	PaidAt *time.Time
}

func stampedJobSchema() *TableSchema {
	return core.NewDirectSchema[stampedJobRow]("job_queue").
		ID("id").
		Field("Status", "status").
		StampedTimeField("PaidAt", "paid_at").
		UpdatedAt("updated_at")
}

func TestDirectStamp_MarkerAsksForTheColumn(t *testing.T) {
	fields, stamps, err := resolveValues(stampedJobSchema(), Values{"Status": "PAID", "PaidAt": Stamp})
	if err != nil {
		t.Fatalf("resolveValues: %v", err)
	}
	if _, bound := fields["paid_at"]; bound {
		t.Fatal("the marker must never become a bound value")
	}
	if len(stamps.requestedTimes) != 1 || stamps.requestedTimes[0] != "paid_at" {
		t.Fatalf("want the stamped column requested, got %v", stamps.requestedTimes)
	}
	if fields["status"] != "PAID" {
		t.Fatalf("ordinary values still bind, got %v", fields["status"])
	}
}

// The caller may ask for the column, never dictate it.
func TestDirectStamp_BindingAValueToAStampedFieldIsRefused(t *testing.T) {
	_, _, err := resolveValues(stampedJobSchema(), Values{"PaidAt": time.Now()})
	if err == nil || !strings.Contains(err.Error(), "write.Stamp") {
		t.Fatalf("binding a value to a stamped field must be refused and teach the marker, got %v", err)
	}
}

func TestDirectStamp_MarkerOnAPlainFieldIsRefused(t *testing.T) {
	_, _, err := resolveValues(stampedJobSchema(), Values{"Status": Stamp})
	if err == nil || !strings.Contains(err.Error(), "plain field") {
		t.Fatalf("stamping a plain field must be refused, got %v", err)
	}
}

// A stamp records when something happened; it is not the something. A write
// carrying nothing else has no change to date.
func TestDirectStamp_StampOnlyWriteIsRefused(t *testing.T) {
	_, _, err := resolveValues(stampedJobSchema(), Values{"PaidAt": Stamp})
	if err == nil || !strings.Contains(err.Error(), "only for stamps") {
		t.Fatalf("a stamp-only write must be refused, got %v", err)
	}
}

// End to end through the verb: the emitted UPDATE carries the stamped column.
func TestDirectStamp_UpdateEmitsTheColumn(t *testing.T) {
	tx := &fakeWriteTx{n: 1}
	w := NewDirectWriter(&directTestEngine{tx: tx}, stampedJobSchema(), "Job")

	if _, err := w.Update(context.Background(),
		Values{"Status": "PAID", "PaidAt": Stamp},
		criteria.Where(criteria.Eq("Status", "NEW"))); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !strings.Contains(tx.lastSQL, "paid_at") {
		t.Fatalf("the requested stamp must reach the statement: %s", tx.lastSQL)
	}
}

// The value the statement binds belongs on the ENTITY too. The audit event reads
// its values from the struct (GoFieldValues), the outbox payload carries the
// written columns, and the caller keeps holding this entity after the write — so
// without the write-back all three would report the field as still empty on the
// very write that filled it.
func TestStampedField_ValueIsWrittenBackOntoTheEntity(t *testing.T) {
	schema := stampedOrderSchema()
	o := &stampedOrder{Status: "PAID"}
	o.Stamp("PaidAt")
	now := testStamp()

	if o.PaidAt != nil {
		t.Fatal("the entity starts with no stamp")
	}
	plan, err := stampedCols(schema, o, schema.UpdateNowColumns(), now)
	if err != nil {
		t.Fatalf("stampedCols: %v", err)
	}
	if o.PaidAt == nil || !o.PaidAt.Equal(now) {
		t.Fatalf("the entity must carry the instant the statement bound, got %v", o.PaidAt)
	}
	if o.CanceledAt != nil {
		t.Fatal("an unrequested stamp must stay empty on the entity")
	}
	// And the payload sees it, even though WriteFields deliberately never emits
	// a stamped column.
	payload := withStamps(schema.WriteFields(o), plan, now)
	if got, ok := payload["paid_at"]; !ok || got != now {
		t.Fatalf("the payload must carry the stamped column, got %v (present=%v)", got, ok)
	}
}

// withStamps must never reach back into the map the DML binds — the redaction
// pass has the same rule, for the same reason.
func TestStampedField_PayloadCopyDoesNotTouchTheBoundMap(t *testing.T) {
	fields := domain.Fields{"status": "PAID"}
	out := withStamps(fields, stampPlan{requestedTimes: []string{"paid_at"}, payload: []string{"paid_at"}}, testStamp())
	if _, leaked := fields["paid_at"]; leaked {
		t.Fatal("the payload view must be a copy, never the bound map")
	}
	if _, ok := out["paid_at"]; !ok {
		t.Fatal("the copy must carry the stamp")
	}
}

// ---------------------------------------------------------------------------
// Aggregate children — the case whose whole difficulty is the write-back
// ---------------------------------------------------------------------------

type stampedLine struct {
	domain.Managed
	Label     string
	ShippedAt *time.Time
	PickCount int64
}

func (l stampedLine) IsSameBusinessIdentity(other domain.AggregateValueObject) bool {
	return domain.IsSameByBusinessFields(l, other)
}
func (stampedLine) CollectionName() string                           { return "Lines" }
func (stampedLine) BuildRules(string, domain.Service, *domain.Rules) {}

func stampedLineSchema() *TableSchema {
	return core.NewTableSchema[stampedLine]("order_lines").
		ID("id").
		ParentID("order_id").
		Field("Label", "label").
		StampedTimeField("ShippedAt", "shipped_at")
}

// A child travels as an interface holding a struct VALUE. Reading its requests
// therefore cannot go through a pointer-receiver method — which is exactly why
// the carrier's read seam takes a value receiver.
func TestStampedChild_RequestSurvivesTheValueCopy(t *testing.T) {
	line := stampedLine{Label: "widget"}
	line.Stamp("ShippedAt")

	var avo domain.AggregateValueObject = line // the way writeChildren receives it
	got := domain.RequestedStamps(avo)
	if len(got) != 1 || got[0].Field != "ShippedAt" {
		t.Fatalf("a child's request must be readable through the interface, got %v", got)
	}
}

// The statement carries the stamp, and the value lands back in the AGGREGATE MAP
// — the copy the audit event's children block and the outbox snapshot read.
// In-place is impossible here, which is the whole reason the child path differs.
func TestStampedChild_WritesBackIntoTheAggregateMap(t *testing.T) {
	schema := stampedLineSchema()
	root := &domain.AggregateRoot{}
	line := stampedLine{Label: "widget"}
	line.Stamp("ShippedAt")
	root.AggregateConstructor([]domain.AggregateValueObject{line})

	now := testStamp()
	plan, err := stampedChildCols(schema, root, line, schema.InsertNowColumns(), now)
	if err != nil {
		t.Fatalf("stampedChildCols: %v", err)
	}
	if len(plan.nowCols) != 1 || plan.nowCols[0] != "shipped_at" {
		t.Fatalf("the child's stamped column must reach the statement, got %v", plan.nowCols)
	}

	// The tracked copy — not the caller's local value — is what post-write
	// readers see, so that is where the instant has to be.
	var seen int
	for _, items := range root.AllAggregateItems() {
		for _, it := range items {
			got, ok := it.Item.(stampedLine)
			if !ok {
				t.Fatalf("unexpected item type %T", it.Item)
			}
			if got.ShippedAt == nil || !got.ShippedAt.Equal(now) {
				t.Fatalf("the tracked child must carry the instant, got %v", got.ShippedAt)
			}
			seen++
		}
	}
	if seen != 1 {
		t.Fatalf("expected one tracked child, saw %d", seen)
	}
}

// A child written outside an aggregate root still gets a correct statement; only
// the write-back has nowhere to land.
func TestStampedChild_NilRootStillStampsTheStatement(t *testing.T) {
	line := stampedLine{Label: "widget"}
	line.Stamp("ShippedAt")

	plan, err := stampedChildCols(stampedLineSchema(), nil, line, nil, testStamp())
	if err != nil {
		t.Fatalf("stampedChildCols: %v", err)
	}
	if len(plan.nowCols) != 1 || plan.nowCols[0] != "shipped_at" {
		t.Fatalf("the statement must still be stamped, got %v", plan.nowCols)
	}
}

// Same refusals as everywhere else — the schema is the authority, whatever node
// it describes.
func TestStampedChild_UnknownRequestIsRefused(t *testing.T) {
	line := stampedLine{}
	line.Stamp("ShipedAt") // typo
	if _, err := stampedChildCols(stampedLineSchema(), nil, line, nil, testStamp()); err == nil ||
		!strings.Contains(err.Error(), "no stamped field") {
		t.Fatalf("a misspelled child stamp must be refused, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Counters — the second member of the family
// ---------------------------------------------------------------------------

type countedRow struct {
	ID         domain.ID
	Label      string
	TotalCount int64
}

func countedSchema() *TableSchema {
	return core.NewDirectSchema[countedRow]("hits").
		ID("id").
		Field("Label", "label").
		StampedCounterField("TotalCount", "total_count")
}

// A counter is int64 — or *int64 when it must also be able to say "no count at
// all", which is the only thing StampNull can land in. Any other width is
// refused: int64 is what the framework increments on every engine.
func TestStampedCounter_AcceptsBothInt64Forms(t *testing.T) {
	type plain struct {
		ID    domain.ID
		Count int64
	}
	type nullable struct {
		ID    domain.ID
		Count *int64
	}
	core.NewDirectSchema[plain]("c1").ID("id").StampedCounterField("Count", "count")
	core.NewDirectSchema[nullable]("c2").ID("id").StampedCounterField("Count", "count")
}

func TestStampedCounter_RequiresInt64(t *testing.T) {
	type bad struct {
		ID    domain.ID
		Count int32
	}
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("a non-int64 counter must be refused at declaration")
		}
		if msg, _ := r.(string); !strings.Contains(msg, "int64") {
			t.Fatalf("the panic must teach the declaration, got %v", r)
		}
	}()
	core.NewDirectSchema[bad]("hits").ID("id").StampedCounterField("Count", "count")
}

// A fresh row starts at 1 — bound as an ordinary argument, since there is no
// existing value to add to yet.
func TestStampedCounter_InsertBindsOne(t *testing.T) {
	schema := countedSchema()
	sql, args := buildInsertWithCounters(testPGDialect{}, schema.Table(), schema.IDColumn(), "018f0000-0000-7000-8000-000000000001",
		domain.Fields{"label": "x"}, stampPlan{counters: []string{"total_count"}}, testStamp(), "")
	if !strings.Contains(sql, "total_count") {
		t.Fatalf("the counter must be in the INSERT: %s", sql)
	}
	var found bool
	for _, a := range args {
		if n, ok := a.(int64); ok && n == 1 {
			found = true
		}
	}
	if !found {
		t.Fatalf("a fresh row starts its counter at 1, args = %v", args)
	}
}

// On an UPDATE the increment is computed by the SERVER — never read into Go and
// written back, which would lose one of two concurrent increments silently.
func TestStampedCounter_UpdateIncrementsServerSide(t *testing.T) {
	sets, args := buildSetWithCounters(testPGDialect{}, domain.Fields{"label": "x"},
		stampPlan{counters: []string{"total_count"}}, testStamp(), "")
	joined := strings.Join(sets, ", ")
	if !strings.Contains(joined, "total_count = total_count + 1") {
		t.Fatalf("the counter must be a server-side increment, got %s", joined)
	}
	// It binds no argument — the value is not the caller's to state.
	if len(args) != 1 {
		t.Fatalf("only the plain field binds an argument, got %v", args)
	}
}

// The two kinds split by declaration, and the split is what decides how each
// column is rendered.
func TestStampedCounter_SplitsFromStampedTime(t *testing.T) {
	schema := core.NewDirectSchema[struct {
		ID         domain.ID
		Label      string
		TotalCount int64
		LastAt     *time.Time
	}]("hits").
		ID("id").
		Field("Label", "label").
		StampedCounterField("TotalCount", "total_count").
		StampedTimeField("LastAt", "last_at")

	if !schema.IsStampedCounter("TotalCount") || schema.IsStampedCounter("LastAt") {
		t.Fatal("IsStampedCounter must tell the two kinds apart")
	}
	times, counters := schema.StampedCounterColumns([]string{"total_count", "last_at"})
	if len(times) != 1 || times[0] != "last_at" {
		t.Fatalf("times = %v", times)
	}
	if len(counters) != 1 || counters[0] != "total_count" {
		t.Fatalf("counters = %v", counters)
	}
}

// The last branch of the single-schema helper: a schema that DECLARES stamped
// fields but is handed no request pays nothing and emits nothing.
func TestStampedField_DeclaredButUnrequestedCostsNothing(t *testing.T) {
	schema := stampedOrderSchema()
	plan, err := stampedCols(schema, &stampedOrder{}, schema.UpdateNowColumns(), testStamp())
	if err != nil {
		t.Fatalf("stampedCols: %v", err)
	}
	if len(plan.counters) != 0 || len(plan.payload) != 0 {
		t.Fatalf("nothing requested means nothing emitted, got %+v", plan)
	}
	if len(plan.nowCols) != 1 || plan.nowCols[0] != "updated_at" {
		t.Fatalf("the managed columns pass through untouched, got %v", plan.nowCols)
	}
}

// A child schema with no stamped field at all takes the same early exit.
func TestStampedChild_SchemaWithoutStampsPassesThrough(t *testing.T) {
	plain := core.NewTableSchema[stampedLine]("order_lines").
		ID("id").ParentID("order_id").Field("Label", "label")
	plan, err := stampedChildCols(plain, nil, stampedLine{}, []string{"updated_at"}, testStamp())
	if err != nil {
		t.Fatalf("stampedChildCols: %v", err)
	}
	if len(plan.nowCols) != 1 || plan.nowCols[0] != "updated_at" {
		t.Fatalf("the managed columns pass through untouched, got %v", plan.nowCols)
	}
}

// A counter on a CHILD is emitted as a counter, not as an instant.
func TestStampedChild_CounterIsSeparatedFromTheInstant(t *testing.T) {
	schema := core.NewTableSchema[stampedLine]("order_lines").
		ID("id").ParentID("order_id").
		Field("Label", "label").
		StampedTimeField("ShippedAt", "shipped_at").
		StampedCounterField("PickCount", "pick_count")

	line := stampedLine{Label: "w"}
	line.Stamp("ShippedAt")
	line.Stamp("PickCount")
	plan, err := stampedChildCols(schema, nil, line, nil, testStamp())
	if err != nil {
		t.Fatalf("stampedChildCols: %v", err)
	}
	if len(plan.nowCols) != 1 || plan.nowCols[0] != "shipped_at" {
		t.Fatalf("the instant column, got %v", plan.nowCols)
	}
	if len(plan.counters) != 1 || plan.counters[0] != "pick_count" {
		t.Fatalf("the counter column, got %v", plan.counters)
	}
}

// ─── the clearing verbs: StampNull and StampEmpty ────────────────────────────

// clearedSchema declares one of each stamped kind, with the counter in its
// NULLABLE form so both verbs reach both kinds.
type clearedRow struct {
	ID    domain.ID
	Label string
	PaidAt *time.Time
	Count  *int64
}

func clearedSchema() *core.TableSchema {
	return core.NewDirectSchema[clearedRow]("cleared").
		ID("id").
		Field("Label", "label").
		StampedTimeField("PaidAt", "paid_at").
		StampedCounterField("Count", "count")
}

// StampNull is a literal in the statement: nothing is bound for it, so there is
// no argument order to get wrong.
func TestStampNull_EmitsTheLiteralOnBothKinds(t *testing.T) {
	_, plan, err := resolveValues(clearedSchema(), Values{
		"Label": "x", "PaidAt": StampNull, "Count": StampNull,
	})
	if err != nil {
		t.Fatalf("resolveValues: %v", err)
	}
	sets, args := buildSetWithCounters(testPGDialect{}, domain.Fields{"label": "x"}, plan, testStamp(), "")
	joined := strings.Join(sets, ", ")
	if !strings.Contains(joined, "paid_at = NULL") || !strings.Contains(joined, "count = NULL") {
		t.Fatalf("both kinds must be nulled by literal, got %s", joined)
	}
	if len(args) != 1 {
		t.Fatalf("a NULL binds nothing, got args %v", args)
	}
}

// StampEmpty says it differently per kind, and deliberately: a counter's zero is
// a literal, a time's is bound because how an instant is written is the
// dialect's.
func TestStampEmpty_ZeroesEachKindItsOwnWay(t *testing.T) {
	_, plan, err := resolveValues(clearedSchema(), Values{
		"Label": "x", "PaidAt": StampEmpty, "Count": StampEmpty,
	})
	if err != nil {
		t.Fatalf("resolveValues: %v", err)
	}
	sets, args := buildSetWithCounters(testPGDialect{}, domain.Fields{"label": "x"}, plan, testStamp(), "")
	joined := strings.Join(sets, ", ")
	if !strings.Contains(joined, "count = 0") {
		t.Fatalf("a counter's zero is a literal, got %s", joined)
	}
	if !strings.Contains(joined, "paid_at = $") {
		t.Fatalf("a time's zero binds, got %s", joined)
	}
	var zeroBound bool
	for _, a := range args {
		if tv, ok := a.(time.Time); ok && tv.IsZero() {
			zeroBound = true
		}
	}
	if !zeroBound {
		t.Fatalf("the zero instant must be the bound value, got %v", args)
	}
}

// The three verbs are distinct requests on one column, and the LAST one wins:
// a rule that stamps and a later rule that clears describe one outcome.
func TestStampVerbs_LastRequestWins(t *testing.T) {
	e := &clearedEntity{}
	e.Stamp("PaidAt")
	e.StampNull("PaidAt")
	got := domain.RequestedStamps(e)
	if len(got) != 1 || got[0].Op != domain.StampToNull {
		t.Fatalf("the last verb must win on one request, got %v", got)
	}
	e.StampEmpty("PaidAt")
	if got = domain.RequestedStamps(e); len(got) != 1 || got[0].Op != domain.StampToEmpty {
		t.Fatalf("and again, got %v", got)
	}
}

type clearedEntity struct {
	domain.BaseEntity
	PaidAt *time.Time
}

func (e *clearedEntity) Modes() []domain.EntityMode { return []domain.EntityMode{domain.ModeUpdate} }
func (e *clearedEntity) BuildRules(string, domain.Service, *domain.Rules) {}

// A plain int64 counter has no absence to write, and the refusal says which verb
// does what the caller meant.
func TestStampNull_RefusedOnANonNullableCounter(t *testing.T) {
	type plain struct {
		ID    domain.ID
		Label string
		Count int64
	}
	schema := core.NewDirectSchema[plain]("plainc").ID("id").
		Field("Label", "label").StampedCounterField("Count", "count")

	_, _, err := resolveValues(schema, Values{"Label": "x", "Count": StampNull})
	if err == nil || !strings.Contains(err.Error(), "StampEmpty") {
		t.Fatalf("StampNull on an int64 counter must be refused and name StampEmpty, got %v", err)
	}
	// …and StampEmpty on the very same field is fine.
	if _, _, err := resolveValues(schema, Values{"Label": "x", "Count": StampEmpty}); err != nil {
		t.Fatalf("StampEmpty must reach a plain counter: %v", err)
	}
}

// On an INSERT a clearing verb is honoured too: the statement a reader sees
// matches what was asked, rather than relying on the column's DEFAULT.
func TestStampClearing_ReachesTheInsert(t *testing.T) {
	_, plan, err := resolveValues(clearedSchema(), Values{
		"Label": "x", "PaidAt": StampNull, "Count": StampEmpty,
	})
	if err != nil {
		t.Fatalf("resolveValues: %v", err)
	}
	sql, _ := buildInsertWithCounters(testPGDialect{}, "cleared", "id",
		"018f0000-0000-7000-8000-000000000001", domain.Fields{"label": "x"}, plan, testStamp(), "")
	if !strings.Contains(sql, "NULL") || !strings.Contains(sql, "0") {
		t.Fatalf("the INSERT must state both clears: %s", sql)
	}
}

// The write-back is what the audit event and the caller's own entity read, so a
// cleared column has to read as cleared on the struct too.
func TestStampClearing_WritesBackOntoTheStruct(t *testing.T) {
	schema := clearedSchema()
	now := testStamp()
	paid := now
	count := int64(7)
	row := &clearedRow{PaidAt: &paid, Count: &count}

	schema.ApplyStamps(row, []domain.StampRequest{
		{Field: "PaidAt", Op: domain.StampToNull},
		{Field: "Count", Op: domain.StampToNull},
	}, now)
	if row.PaidAt != nil || row.Count != nil {
		t.Fatalf("StampNull must clear both on the struct, got %v / %v", row.PaidAt, row.Count)
	}

	schema.ApplyStamps(row, []domain.StampRequest{
		{Field: "PaidAt", Op: domain.StampToEmpty},
		{Field: "Count", Op: domain.StampToEmpty},
	}, now)
	if row.PaidAt == nil || !row.PaidAt.IsZero() {
		t.Fatalf("StampEmpty must land the zero instant, got %v", row.PaidAt)
	}
	if row.Count == nil || *row.Count != 0 {
		t.Fatalf("StampEmpty must land 0, got %v", row.Count)
	}
}

// A stamped COUNTER on a Direct write reaches the statement as a counter on
// every verb, not only on Upsert.
//
// It used to reach Insert/Update/UpdateAll as a TIME: those paths appended every
// resolved stamp column to the "bind the operation's instant" list, so a counter
// column was bound a time.Time and the statement failed at the database. Only
// Upsert built its own plan and split the two kinds, which is why the defect
// stayed invisible — nothing asked a counter of the other three verbs.
func TestStampedCounter_DirectInsertAndUpdateTreatItAsACounter(t *testing.T) {
	type row struct {
		ID    domain.ID
		Label string
		Count int64
	}
	schema := core.NewDirectSchema[row]("counted_direct").ID("id").
		Field("Label", "label").StampedCounterField("Count", "count")

	_, plan, err := resolveValues(schema, Values{"Label": "x", "Count": Stamp})
	if err != nil {
		t.Fatalf("resolveValues: %v", err)
	}
	if len(plan.counters) != 1 || plan.counters[0] != "count" {
		t.Fatalf("the counter must land in the counter bucket, got %+v", plan)
	}
	if len(plan.requestedTimes) != 0 {
		t.Fatalf("a counter is never bound to the instant, got %v", plan.requestedTimes)
	}

	sql, args := buildInsertWithCounters(testPGDialect{}, "counted_direct", "id",
		"018f0000-0000-7000-8000-000000000001", domain.Fields{"label": "x"}, plan, testStamp(), "")
	if !strings.Contains(sql, "count") {
		t.Fatalf("the counter must be in the INSERT: %s", sql)
	}
	for _, a := range args {
		if _, isTime := a.(time.Time); isTime {
			t.Fatalf("a counter must never be bound an instant, args = %v", args)
		}
	}

	sets, _ := buildSetWithCounters(testPGDialect{}, domain.Fields{"label": "x"}, plan, testStamp(), "")
	if joined := strings.Join(sets, ", "); !strings.Contains(joined, "count = count + 1") {
		t.Fatalf("the UPDATE must increment server-side, got %s", joined)
	}
}

// The payload states what each bucket actually wrote. A cleared column reporting
// the operation's instant would tell the projection the opposite of the row.
func TestStampClearing_PayloadStatesEachValue(t *testing.T) {
	plan := stampPlan{
		requestedTimes: []string{"paid_at"},
		nullCols:       []string{"canceled_at"},
		zeroTimes:      []string{"reset_at"},
		zeroCounters:   []string{"count"},
	}
	plan.payload = []string{"paid_at", "canceled_at", "reset_at", "count"}
	now := testStamp()
	out := withStamps(domain.Fields{"status": "X"}, plan, now)

	if out["paid_at"] != now {
		t.Errorf("a filled time states the instant, got %v", out["paid_at"])
	}
	if v, ok := out["canceled_at"]; !ok || v != nil {
		t.Errorf("a cleared column states the absence, got %v (present=%v)", v, ok)
	}
	if tv, ok := out["reset_at"].(time.Time); !ok || !tv.IsZero() {
		t.Errorf("a reset time states the zero instant, got %v", out["reset_at"])
	}
	if out["count"] != int64(0) {
		t.Errorf("a reset counter states 0, got %v", out["count"])
	}
}

// ─── the clearing verbs on EVERY seat the family can be declared on ──────────
//
// StampedTimeField/StampedCounterField are declarable on five kinds of writing
// schema: the flat/aggregate ROOT, an aggregate CHILD, a shared-base ROLE, the
// shared BASE itself, and a Direct schema. The verbs are resolved by a different
// function on three of those paths — stampedCols, stampedChildCols and
// claimStampedCols — so "it works on Direct" proves nothing about the rest.

// clearedOrder is a ROOT carrying one of each stamped kind, the counter in its
// nullable form so both verbs reach both kinds.
type clearedOrder struct {
	domain.BaseEntity
	Status  string
	PaidAt  *time.Time
	Touches *int64
}

func (o *clearedOrder) EntityName() string { return "Order" }

func clearedOrderSchema() *TableSchema {
	return core.NewTableSchema[*clearedOrder]("orders").
		ID("id").
		Field("Status", "status").
		StampedTimeField("PaidAt", "paid_at").
		StampedCounterField("Touches", "touches").
		UpdatedAt("updated_at")
}

// SEAT 1 — the ROOT, through stampedCols.
func TestStampClearing_OnTheRoot(t *testing.T) {
	schema := clearedOrderSchema()
	now := testStamp()
	paid, touches := now, int64(7)
	o := &clearedOrder{Status: "PAID", PaidAt: &paid, Touches: &touches}
	o.StampNull("PaidAt")
	o.StampEmpty("Touches")

	plan, err := stampedCols(schema, o, schema.UpdateNowColumns(), now)
	if err != nil {
		t.Fatalf("stampedCols: %v", err)
	}
	if len(plan.nullCols) != 1 || plan.nullCols[0] != "paid_at" {
		t.Fatalf("the cleared time must reach the null bucket, got %+v", plan)
	}
	if len(plan.zeroCounters) != 1 || plan.zeroCounters[0] != "touches" {
		t.Fatalf("the reset counter must reach the zero bucket, got %+v", plan)
	}
	// The write-back: the entity the caller keeps holding, and the audit event
	// that reads the struct, must agree with the row.
	if o.PaidAt != nil {
		t.Errorf("StampNull must clear the field on the root, got %v", o.PaidAt)
	}
	if o.Touches == nil || *o.Touches != 0 {
		t.Errorf("StampEmpty must zero the field on the root, got %v", o.Touches)
	}
	sets, _ := buildSetWithCounters(testPGDialect{}, domain.Fields{"status": "PAID"}, plan, now, "")
	joined := strings.Join(sets, ", ")
	if !strings.Contains(joined, "paid_at = NULL") || !strings.Contains(joined, "touches = 0") {
		t.Fatalf("the root UPDATE must carry both clears, got %s", joined)
	}
}

// SEAT 2 — an aggregate CHILD, through stampedChildCols. Its whole difficulty is
// the write-back: a child travels as an interface holding a struct VALUE, so it
// happens through the aggregate map.
type clearedLine struct {
	domain.Managed
	Label     string
	ShippedAt *time.Time
	Picks     *int64
}

func (l clearedLine) IsSameBusinessIdentity(other domain.AggregateValueObject) bool {
	return domain.IsSameByBusinessFields(l, other)
}
func (clearedLine) CollectionName() string                            { return "Lines" }
func (clearedLine) BuildRules(string, domain.Service, *domain.Rules) {}

func clearedLineSchema() *TableSchema {
	return core.NewTableSchema[clearedLine]("order_lines").
		ID("id").
		ParentID("order_id").
		Field("Label", "label").
		StampedTimeField("ShippedAt", "shipped_at").
		StampedCounterField("Picks", "picks")
}

func TestStampClearing_OnAnAggregateChild(t *testing.T) {
	schema := clearedLineSchema()
	now := testStamp()
	shipped, picks := now, int64(4)
	line := clearedLine{Label: "widget", ShippedAt: &shipped, Picks: &picks}
	line.StampNull("ShippedAt")
	line.StampEmpty("Picks")

	root := &domain.AggregateRoot{}
	root.AggregateConstructor([]domain.AggregateValueObject{line})

	plan, err := stampedChildCols(schema, root, line, nil, now)
	if err != nil {
		t.Fatalf("stampedChildCols: %v", err)
	}
	if len(plan.nullCols) != 1 || len(plan.zeroCounters) != 1 {
		t.Fatalf("both verbs must reach the child's plan, got %+v", plan)
	}
	// The write-back lands in the aggregate MAP — the copy the audit event's
	// children block and the outbox snapshot read.
	got, ok := trackedClearedLine(t, root)
	if !ok {
		t.Fatal("the child must still be tracked in the aggregate map")
	}
	if got.ShippedAt != nil {
		t.Errorf("StampNull must clear the child's field in the map, got %v", got.ShippedAt)
	}
	if got.Picks == nil || *got.Picks != 0 {
		t.Errorf("StampEmpty must zero the child's field in the map, got %v", got.Picks)
	}
}

// A child that only CLEARS still writes back. The old condition was "there is a
// time column bound to the instant", which a clear-only request does not
// satisfy — the write-back would have been skipped and the map kept the stale
// value while the row was cleared.
func TestStampClearing_ChildWithNoFilledTimeStillWritesBack(t *testing.T) {
	schema := clearedLineSchema()
	now := testStamp()
	shipped := now
	line := clearedLine{Label: "only-clear", ShippedAt: &shipped}
	line.StampNull("ShippedAt")

	root := &domain.AggregateRoot{}
	root.AggregateConstructor([]domain.AggregateValueObject{line})
	if _, err := stampedChildCols(schema, root, line, nil, now); err != nil {
		t.Fatalf("stampedChildCols: %v", err)
	}
	got, _ := trackedClearedLine(t, root)
	if got.ShippedAt != nil {
		t.Fatalf("a clear-only child request must still write back, got %v", got.ShippedAt)
	}
}

// SEATS 3 and 4 — a shared-base ROLE and the BASE itself, through
// claimStampedCols. One entity's requests are split across two schemas, and a
// name meant for one is not a mistake to the other.
type clearedMember struct {
	domain.BaseEntity
	Name      string
	Document  string
	MemberRef string
	JoinedAt  *time.Time // the ROLE's stamped time
	Visits    *int64     // the ROLE's stamped counter
	VerifiedAt *time.Time // the BASE's stamped time
	Seen       *int64     // the BASE's stamped counter
}

func (e *clearedMember) EntityName() string { return "Member" }

func clearedMemberSchemas() (role, base *TableSchema) {
	base = core.NewSharedBaseSchema("people").
		Revision("revision").
		ID("id").
		Field("Name", "name").
		Field("Document", "document").
		NaturalID("document").
		StampedTimeField("VerifiedAt", "verified_at").
		StampedCounterField("Seen", "seen")
	role = core.NewTableSchema[*clearedMember]("members").
		ID("id").
		Field("MemberRef", "member_ref").
		StampedTimeField("JoinedAt", "joined_at").
		StampedCounterField("Visits", "visits").
		SharedBase(base, "person_id")
	return role, base
}

func TestStampClearing_SplitsAcrossRoleAndBase(t *testing.T) {
	role, base := clearedMemberSchemas()
	now := testStamp()
	joined, verified := now, now
	visits, seen := int64(3), int64(9)
	m := &clearedMember{
		Name: "Ana", Document: "D1", MemberRef: "R1",
		JoinedAt: &joined, Visits: &visits, VerifiedAt: &verified, Seen: &seen,
	}
	m.StampNull("JoinedAt")     // role, time
	m.StampEmpty("Visits")      // role, counter
	m.StampNull("VerifiedAt")   // base, time
	m.StampEmpty("Seen")        // base, counter

	// The ROLE claims its two and hands the base's back — unclaimed here is not a
	// mistake, it is the other schema's business.
	rolePlan, leftover, err := claimStampedCols(role, m, role.UpdateNowColumns(), now)
	if err != nil {
		t.Fatalf("claimStampedCols(role): %v", err)
	}
	if len(rolePlan.nullCols) != 1 || rolePlan.nullCols[0] != "joined_at" {
		t.Fatalf("the role must claim its cleared time, got %+v", rolePlan)
	}
	if len(rolePlan.zeroCounters) != 1 || rolePlan.zeroCounters[0] != "visits" {
		t.Fatalf("the role must claim its reset counter, got %+v", rolePlan)
	}
	if len(leftover) != 2 {
		t.Fatalf("the base's two names must come back for the base, got %v", leftover)
	}

	// The BASE claims the rest, and nothing is left over.
	basePlan, stillLeft, err := claimStampedCols(base, m, base.UpdateNowColumns(), now)
	if err != nil {
		t.Fatalf("claimStampedCols(base): %v", err)
	}
	if len(basePlan.nullCols) != 1 || basePlan.nullCols[0] != "verified_at" {
		t.Fatalf("the base must claim its cleared time, got %+v", basePlan)
	}
	if len(basePlan.zeroCounters) != 1 || basePlan.zeroCounters[0] != "seen" {
		t.Fatalf("the base must claim its reset counter, got %+v", basePlan)
	}
	if len(stillLeft) != 2 {
		t.Fatalf("the role's two names must come back for the role, got %v", stillLeft)
	}

	// Between them the whole entity is cleared — both schemas wrote back onto the
	// SAME struct, each for its own fields.
	if m.JoinedAt != nil || m.VerifiedAt != nil {
		t.Errorf("both cleared times must be nil, got %v / %v", m.JoinedAt, m.VerifiedAt)
	}
	if m.Visits == nil || *m.Visits != 0 || m.Seen == nil || *m.Seen != 0 {
		t.Errorf("both reset counters must be 0, got %v / %v", m.Visits, m.Seen)
	}
}

// The int64 refusal has to fire on every seat, not only on Direct — the check
// lives on the schema, and each seat is a different schema.
func TestStampNull_RefusedOnAPlainCounterOnEverySeat(t *testing.T) {
	type plainRoot struct {
		domain.BaseEntity
		Status string
		Count  int64
	}
	rootSchema := core.NewTableSchema[*plainRoot]("proot").ID("id").
		Field("Status", "status").StampedCounterField("Count", "count")
	r := &plainRoot{}
	r.StampNull("Count")
	if _, err := stampedCols(rootSchema, r, nil, testStamp()); err == nil ||
		!strings.Contains(err.Error(), "StampEmpty") {
		t.Fatalf("the root seat must refuse and name StampEmpty, got %v", err)
	}

	childSchema := core.NewTableSchema[plainCountLine]("plines").ID("id").ParentID("root_id").
		Field("Label", "label").StampedCounterField("Count", "count")
	line := plainCountLine{Label: "x"}
	line.StampNull("Count")
	if _, err := stampedChildCols(childSchema, nil, line, nil, testStamp()); err == nil ||
		!strings.Contains(err.Error(), "StampEmpty") {
		t.Fatalf("the child seat must refuse and name StampEmpty, got %v", err)
	}
}

// trackedClearedLine reads the ONE child the aggregate map tracks — the copy
// post-write readers see, which is where a child's write-back has to land.
func trackedClearedLine(t *testing.T, root *domain.AggregateRoot) (clearedLine, bool) {
	t.Helper()
	for _, items := range root.AllAggregateItems() {
		for _, it := range items {
			got, ok := it.Item.(clearedLine)
			if !ok {
				t.Fatalf("unexpected item type %T", it.Item)
			}
			return got, true
		}
	}
	return clearedLine{}, false
}

// plainCountLine is a CHILD whose counter is the plain int64 form — the one
// StampNull has nowhere to land in.
type plainCountLine struct {
	domain.Managed
	Label string
	Count int64
}

func (l plainCountLine) IsSameBusinessIdentity(other domain.AggregateValueObject) bool {
	return domain.IsSameByBusinessFields(l, other)
}
func (plainCountLine) CollectionName() string                            { return "Plines" }
func (plainCountLine) BuildRules(string, domain.Service, *domain.Rules) {}

// A SHARED BASE's stamped column writes back onto the role's struct, with EVERY
// verb — the original Stamp included.
//
// It used to write the column and skip the struct. ApplyStamps walked the
// schema's own resolved index path, and a base has no struct of its own: its
// fields are resolved against each ROLE's type at .SharedBase(...) time and the
// path is stored on the ROLE's link. So the base's field carried no usable path,
// the write-back was silently skipped, and the entity the caller kept holding —
// and the audit event, which reads the struct — reported the OLD value while the
// row held the new one. Nothing caught it because no test covered the base seat.
func TestStampedBase_WritesBackOntoTheRoleStruct(t *testing.T) {
	_, base := clearedMemberSchemas()
	now := testStamp()
	m := &clearedMember{Name: "Ana", Document: "D1", MemberRef: "R1"}
	m.Stamp("VerifiedAt")

	plan, _, err := claimStampedCols(base, m, base.UpdateNowColumns(), now)
	if err != nil {
		t.Fatalf("claimStampedCols: %v", err)
	}
	if len(plan.requestedTimes) != 1 || plan.requestedTimes[0] != "verified_at" {
		t.Fatalf("the base must claim its own column, got %+v", plan)
	}
	if m.VerifiedAt == nil || !m.VerifiedAt.Equal(now) {
		t.Fatalf("the base's stamp must land on the role's struct, got %v", m.VerifiedAt)
	}
}
