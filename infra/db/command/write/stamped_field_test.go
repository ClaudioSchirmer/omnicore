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
	cols, _, err := stampedCols(schema, o, schema.UpdateNowColumns(), testStamp())
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

	cols, _, err := stampedCols(schema, o, schema.UpdateNowColumns(), testStamp())
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

	cols, _, err := stampedCols(schema, o, nil, testStamp())
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
	_, _, err := stampedCols(schema, o, nil, testStamp())
	if err == nil || !strings.Contains(err.Error(), "no stamped field") {
		t.Fatalf("a misspelled stamp must be refused, got %v", err)
	}

	p := &stampedOrder{}
	p.Stamp("Status") // mapped, but plain
	_, _, err = stampedCols(schema, p, nil, testStamp())
	if err == nil || !strings.Contains(err.Error(), "plain field") {
		t.Fatalf("stamping a plain field must be refused, got %v", err)
	}
}

// A schema declaring no stamped field never reads requests at all.
func TestStampedField_SchemaWithoutStampsIgnoresRequests(t *testing.T) {
	if stampedOrderSchema().HasStampedFields() != true {
		t.Fatal("fixture must declare stamped fields")
	}
	plain := core.NewTableSchema[*stampedOrder]("orders").ID("id").Field("Status", "status")
	if plain.HasStampedFields() {
		t.Fatal("a schema with no StampedTimeField must report none")
	}
	o := &stampedOrder{}
	o.Stamp("PaidAt")
	cols, _, err := stampedCols(plain, o, []string{"updated_at"}, testStamp())
	if err != nil {
		t.Fatalf("a schema with no stamped field must not read requests: %v", err)
	}
	if len(cols) != 1 || cols[0] != "updated_at" {
		t.Fatalf("the managed columns must pass through untouched, got %v", cols)
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
	_, stamped, err := stampedCols(schema, o, schema.UpdateNowColumns(), now)
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
