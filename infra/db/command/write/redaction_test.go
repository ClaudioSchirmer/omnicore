package write

import (
	"testing"

	"github.com/google/uuid"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// The two invariants the RedactedField feature rests on, exercised through the
// real builders rather than through the redactors themselves (those are covered
// in infra/db/core):
//
//  1. the payload is redacted on the COPY — the map the DML binds still carries
//     the real value, because one WriteFields call feeds both;
//  2. the audit delta is redacted AFTER the diff — the entry survives (so the
//     trail still says the field changed) with both sides masked.

// redactedTestSchema mirrors builderTestSchema but declares Email as redacted:
// partially masked in the derived copies, fully masked in the trail.
var redactedTestSchema = NewTableSchema[*builderTestEntity]("builder_test_entities").
	ID("id").
	Revision("revision").
	Field("Name", "name").
	RedactedField("Email", "email",
		core.InSync(core.RedactKeepLast(5)),
		core.InAudit(core.RedactWith("***")),
	).
	DeletedAt("deleted_at").
	CreatedAt("created_at").
	UpdatedAt("updated_at")

func TestBuildWritePayload_RedactsOnTheCopyOnly(t *testing.T) {
	e := &builderTestEntity{Name: "alice", Email: "alice@x.com"}
	bound := redactedTestSchema.WriteFields(e)

	p := buildWritePayload(redactedTestSchema, e, nil, "INSERTED", testNow, bound,
		outboxMeta{ID: uuid.NewString()})

	// The payload — and therefore the outbox row, the topic, every consuming
	// service, the failure ledgers and the projected document — carries the mask.
	if p["email"] != "******x.com" {
		t.Errorf("payload email = %v, want ******x.com", p["email"])
	}
	// The map the INSERT binds is the SAME map that was handed in. If the
	// redaction had been applied upstream of the copy, the column that exists to
	// hold the real value would receive the mask instead.
	if bound["email"] != "alice@x.com" {
		t.Fatalf("the bound write map must keep the real value, got %v", bound["email"])
	}
	// A field with no declaration travels untouched.
	if p["name"] != "alice" {
		t.Errorf("payload name = %v, want alice", p["name"])
	}
}

func TestBuildWritePayload_UndeclaredSchemaIsUnaffected(t *testing.T) {
	e := &builderTestEntity{Name: "alice", Email: "alice@x.com"}
	p := buildWritePayload(builderTestSchema, e, nil, "INSERTED", testNow,
		builderTestSchema.WriteFields(e), outboxMeta{ID: uuid.NewString()})
	if p["email"] != "alice@x.com" {
		t.Fatalf("a schema declaring no RedactedField must be untouched, got %v", p["email"])
	}
}

// The delta is the case a naive implementation breaks: computeChanges drops
// every key whose two sides compare equal, so redacting the inputs first would
// turn a real change into no change at all and the trail would lose the one fact
// a masked field still owes it.
func TestBuildUpdateEvent_RedactedChangeKeepsItsEntry(t *testing.T) {
	e := &builderTestEntity{Name: "alice", Email: "alice@x.com"}
	e.SetID(domain.NewID(uuid.NewString()))

	apply := func(x *builderTestEntity) error { x.Email = "bob@y.com"; return nil }
	u, err := domain.GetUpdatable(e, apply, nil, "GetUpdatable")
	if err != nil {
		t.Fatalf("GetUpdatable: %v", err)
	}
	ev := BuildUpdateEvent(newBuilderCtx(), u, redactedTestSchema, nil)

	var found bool
	for _, c := range ev.Changes {
		if c.Field != "Email" {
			continue
		}
		found = true
		if c.From != "***" || c.To != "***" {
			t.Errorf("both sides must be masked, got from=%v to=%v", c.From, c.To)
		}
	}
	if !found {
		t.Fatal("the change entry must SURVIVE the redaction — its presence is what records that the field changed")
	}
}

// A field that did NOT change must stay absent, exactly as before: the mask
// applies to the values, never to the diff decision.
func TestBuildUpdateEvent_RedactedFieldUnchangedStaysAbsent(t *testing.T) {
	e := &builderTestEntity{Name: "alice", Email: "alice@x.com"}
	e.SetID(domain.NewID(uuid.NewString()))

	apply := func(x *builderTestEntity) error { x.Name = "bob"; return nil }
	u, err := domain.GetUpdatable(e, apply, nil, "GetUpdatable")
	if err != nil {
		t.Fatalf("GetUpdatable: %v", err)
	}
	ev := BuildUpdateEvent(newBuilderCtx(), u, redactedTestSchema, nil)

	for _, c := range ev.Changes {
		if c.Field == "Email" {
			t.Fatalf("an untouched field must not appear in the delta, got %+v", c)
		}
	}
}

func TestBuildInsertEvent_SnapshotIsRedacted(t *testing.T) {
	e := &builderTestEntity{Name: "alice", Email: "alice@x.com"}
	i, err := domain.GetInsertable(e, nil, "GetInsertable")
	if err != nil {
		t.Fatalf("GetInsertable: %v", err)
	}
	ev := BuildInsertEvent(newBuilderCtx(), i, domain.NewID(uuid.NewString()), redactedTestSchema, nil)

	if ev.Snapshot["Email"] != "***" {
		t.Errorf("snapshot Email = %v, want ***", ev.Snapshot["Email"])
	}
	// The key is PRESENT, masked — which is why the audit needs no "restricted"
	// marker: there is no ambiguity between "hidden by policy" and "absent".
	if _, has := ev.Snapshot["Email"]; !has {
		t.Error("the snapshot key must be present, masked — not dropped")
	}
	if ev.Snapshot["Name"] != "alice" {
		t.Errorf("snapshot Name = %v, want alice", ev.Snapshot["Name"])
	}
}

// The two axes are independent: the same field is partially masked in the copies
// and fully masked in the trail, from one declaration.
func TestAxesAreIndependent(t *testing.T) {
	e := &builderTestEntity{Name: "alice", Email: "alice@x.com"}
	p := buildWritePayload(redactedTestSchema, e, nil, "INSERTED", testNow,
		redactedTestSchema.WriteFields(e), outboxMeta{ID: uuid.NewString()})
	i, err := domain.GetInsertable(e, nil, "GetInsertable")
	if err != nil {
		t.Fatalf("GetInsertable: %v", err)
	}
	ev := BuildInsertEvent(newBuilderCtx(), i, domain.NewID(uuid.NewString()), redactedTestSchema, nil)

	if p["email"] == ev.Snapshot["Email"] {
		t.Fatalf("InSync and InAudit must be able to differ, both produced %v", p["email"])
	}
}
