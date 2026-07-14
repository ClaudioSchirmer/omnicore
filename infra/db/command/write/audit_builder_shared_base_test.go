package write

import (
	"testing"

	"github.com/google/uuid"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// Audit composition over a SharedBase role: the entity is one flat Go struct
// whose fields infra partitions across the base table (shared identity), the
// role's own table, a sibling (1:1 shared PK), and a base-child collection
// (1:N owned by the base). The audit timeline must speak the WHOLE domain
// object — role ∪ base ∪ sibling fields in the snapshot/delta, and populated
// base-child snapshots — not just the role's own column. Before the composition
// fix the auditor only saw the role field (user_name) and emitted op-only child
// events with empty snapshots.

type sbAuditAddr struct {
	ID     string
	Street string
	City   string
}

func (c sbAuditAddr) GetID() domain.ID                                 { return domain.NewID(c.ID) }
func (c sbAuditAddr) BuildRules(string, domain.Service, *domain.Rules) {}

type sbAuditRole struct {
	domain.AggregateRoot
	Name     string `labelKey:"PersonNameLabel"` // shared (base) — label lives on the flat struct
	Email    string // shared (base)
	Document string // shared + natural key (base)
	UserName string // role-own
	SmsOptIn *bool  // sibling (user_configurations)
}

func (e *sbAuditRole) Modes() []domain.EntityMode {
	return []domain.EntityMode{domain.ModeInsert, domain.ModeUpdate, domain.ModeDelete, domain.ModeArchive, domain.ModeUnarchive}
}
func (e *sbAuditRole) BuildRules(string, domain.Service, *domain.Rules) {}
func (e *sbAuditRole) GetAggregateRoot() *domain.AggregateRoot          { return &e.AggregateRoot }
func (e *sbAuditRole) AggregateChildren() []domain.AggregateValueObject {
	return []domain.AggregateValueObject{sbAuditAddr{}}
}

// sbAuditSchema: role "users" over persons (SharedBase, natural key document)
// with an addresses base-child and a user_configurations sibling.
func sbAuditSchema() *TableSchema {
	base := NewSharedBase("persons").PK("id").
		Field("Name", "name").Field("Email", "email").Field("Document", "document").
		NaturalKey("document").SoftDelete("deleted_at")
	addr := NewTableSchema[sbAuditAddr]("addresses").PK("id").FK("person_id").
		Field("Street", "street").Field("City", "city").SoftDelete("deleted_at")
	base = base.Child(addr)
	role := NewTableSchema[*sbAuditRole]("users").PK("id").Field("UserName", "user_name").SoftDelete("deleted_at")
	sib := NewSiblingSchema[*sbAuditRole]("user_configurations").Field("SmsOptIn", "sms_notification")
	return role.SharedBase(base, "person_id").Sibling(sib)
}

func boolPtr(b bool) *bool { return &b }

func TestBuildInsertEvent_SharedBaseComposesSnapshotAndBaseChildren(t *testing.T) {
	e := &sbAuditRole{Name: "Jane", Email: "jane@x.com", Document: "D1", UserName: "jane", SmsOptIn: boolPtr(true)}
	domain.AddAggregateChild(e, sbAuditAddr{Street: "1 Main St", City: "Metropolis"})
	i, err := domain.GetInsertable(e, nil, "GetInsertable")
	if err != nil {
		t.Fatalf("GetInsertable: %v", err)
	}
	ev := BuildInsertEvent(newBuilderCtx(), i, domain.NewID(uuid.NewString()), sbAuditSchema(), nil)

	// Snapshot must union role + base + sibling fields.
	want := map[string]any{
		"UserName": "jane",       // role-own
		"Name":     "Jane",       // base
		"Email":    "jane@x.com", // base
		"Document": "D1",         // base natural key
	}
	for k, v := range want {
		if ev.Snapshot[k] != v {
			t.Errorf("snapshot[%q] = %v, want %v (snapshot: %+v)", k, ev.Snapshot[k], v, ev.Snapshot)
		}
	}
	if got, ok := ev.Snapshot["SmsOptIn"].(*bool); !ok || got == nil || *got != true {
		t.Errorf("snapshot[SmsOptIn] = %v, want *bool(true) — sibling field missing from audit", ev.Snapshot["SmsOptIn"])
	}

	// Base-child must carry a populated snapshot, not an op-only stub.
	addrs := ev.Children["sbAuditAddr"]
	if len(addrs) != 1 {
		t.Fatalf("children[sbAuditAddr] len = %d, want 1: %+v", len(addrs), ev.Children)
	}
	if addrs[0].Op != "inserted" {
		t.Errorf("child op = %q, want inserted", addrs[0].Op)
	}
	if addrs[0].Snapshot["Street"] != "1 Main St" || addrs[0].Snapshot["City"] != "Metropolis" {
		t.Errorf("base-child snapshot not populated: %+v", addrs[0].Snapshot)
	}
}

func TestBuildUpdateEvent_SharedBaseTracksBaseFieldDelta(t *testing.T) {
	e := &sbAuditRole{Name: "Jane", Email: "jane@x.com", Document: "D1", UserName: "jane"}
	e.SetID(domain.NewID(uuid.NewString()))

	// Mutate a BASE field (Name) — the delta must observe it even though it
	// lives on persons, not users.
	apply := func(x *sbAuditRole) error { x.Name = "Janet"; return nil }
	u, err := domain.GetUpdatable(e, apply, nil, "GetUpdatable")
	if err != nil {
		t.Fatalf("GetUpdatable: %v", err)
	}
	ev := BuildUpdateEvent(newBuilderCtx(), u, sbAuditSchema(), nil)

	if len(ev.Changes) != 1 {
		t.Fatalf("Changes len = %d, want 1 (base field Name mutated): %+v", len(ev.Changes), ev.Changes)
	}
	c := ev.Changes[0]
	if c.Field != "Name" || c.From != "Jane" || c.To != "Janet" {
		t.Errorf("delta = %+v, want Name Jane→Janet", c)
	}
	// The base field's label lives on the flat struct tag; the type-less base
	// cannot reflect it, so composition must recover it off the role type.
	if c.FieldLabelKey != "PersonNameLabel" {
		t.Errorf("delta fieldLabelKey = %q, want PersonNameLabel (base label composed off role type)", c.FieldLabelKey)
	}
}

func TestBuildDeleteEvent_SharedBaseComposesForensicSnapshot(t *testing.T) {
	e := &sbAuditRole{Name: "Jane", Email: "jane@x.com", Document: "D1", UserName: "jane"}
	e.SetID(domain.NewID(uuid.NewString()))
	e.AggregateConstructor([]domain.AggregateValueObject{sbAuditAddr{ID: "a1", Street: "1 Main St", City: "Metropolis"}})
	d, err := domain.GetDeletable(e, nil, "GetDeletable")
	if err != nil {
		t.Fatalf("GetDeletable: %v", err)
	}
	ev := BuildDeleteEvent(newBuilderCtx(), d, sbAuditSchema(), nil)

	// Forensic snapshot must retain the base identity that is about to vanish.
	if ev.Snapshot["Name"] != "Jane" || ev.Snapshot["Email"] != "jane@x.com" || ev.Snapshot["Document"] != "D1" {
		t.Errorf("delete snapshot missing base identity: %+v", ev.Snapshot)
	}
	addrs := ev.Children["sbAuditAddr"]
	if len(addrs) != 1 || addrs[0].Op != "deleted" || addrs[0].Snapshot["Street"] != "1 Main St" {
		t.Errorf("base-child delete snapshot not populated: %+v", ev.Children)
	}
}

func TestBuildSharedBasePurgeEvent_SpeaksBaseIdentity(t *testing.T) {
	e := &sbAuditRole{Name: "Jane", Email: "jane@x.com", Document: "D1", UserName: "jane"}
	e.SetID(domain.NewID(uuid.NewString()))
	d, err := domain.GetDeletable(e, nil, "GetDeletable")
	if err != nil {
		t.Fatalf("GetDeletable: %v", err)
	}
	baseID := uuid.NewString()
	ev := BuildSharedBasePurgeEvent(newBuilderCtx(), d, sbAuditSchema(), baseID, nil)

	// The purge event speaks the BASE identity — its table as EntityType, its
	// deterministic id as EntityID — never the role's.
	if ev.EntityType != "persons" || ev.EntityID != baseID {
		t.Errorf("purge event must identify the base (persons/%s), got %s/%s", baseID, ev.EntityType, ev.EntityID)
	}
	if ev.Verb != "delete" || ev.Kind != "snapshot" {
		t.Errorf("purge event must be a delete snapshot, got verb=%s kind=%s", ev.Verb, ev.Kind)
	}
	// The snapshot carries only the shared fields (read off the role entity by
	// Go name), not the role's own columns.
	if ev.Snapshot["Name"] != "Jane" || ev.Snapshot["Email"] != "jane@x.com" || ev.Snapshot["Document"] != "D1" {
		t.Errorf("purge snapshot missing base identity: %+v", ev.Snapshot)
	}
	if _, hasRoleField := ev.Snapshot["UserName"]; hasRoleField {
		t.Errorf("purge snapshot must not carry role-own fields: %+v", ev.Snapshot)
	}
}
