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
	if len(stamps) != 1 || stamps[0] != "paid_at" {
		t.Fatalf("want the stamped column requested, got %v", stamps)
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
	stamped := plan.payload
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
	payload := withStamps(schema.WriteFields(o), stamped, now)
	if got, ok := payload["paid_at"]; !ok || got != now {
		t.Fatalf("the payload must carry the stamped column, got %v (present=%v)", got, ok)
	}
}

// withStamps must never reach back into the map the DML binds — the redaction
// pass has the same rule, for the same reason.
func TestStampedField_PayloadCopyDoesNotTouchTheBoundMap(t *testing.T) {
	fields := domain.Fields{"status": "PAID"}
	out := withStamps(fields, []string{"paid_at"}, testStamp())
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
	if len(got) != 1 || got[0] != "ShippedAt" {
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

// A counter is int64, not a pointer: a row that was just created has counted one
// thing, so there is no absence for nil to describe.
func TestStampedCounter_RequiresInt64(t *testing.T) {
	type bad struct {
		ID    domain.ID
		Count *int64
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
		domain.Fields{"label": "x"}, nil, []string{"total_count"}, testStamp(), "")
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
		nil, []string{"total_count"}, testStamp(), "")
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
