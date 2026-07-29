//go:build integration && mysql

package mysql

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	appaudit "github.com/ClaudioSchirmer/omnicore/application/audit"
	"github.com/ClaudioSchirmer/omnicore/application/persistence"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/audit"
	"github.com/ClaudioSchirmer/omnicore/infra/db/command/read"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/google/uuid"
)

// Phase 4 item 2 integration test: the MySQL engine writes the in-TX audit_events
// row and publishes domain events — the mirror of the Postgres path. Proves the
// dialect-specific audit INSERT (audit.InsertAuditEventMySQL, `?` placeholders,
// Go-generated row id) executes on a real MySQL and that the post-commit echo +
// publisher fire.
//
//	go test -tags=integration,mysql ./infra/db/mysql/ -run AuditAndEvents -count=1

// eventfulPerson maps to flat_persons (same columns) and registers a domain
// event in BuildRules so the publisher path is exercised.
type eventfulPerson struct {
	domain.BaseEntity
	Name  string
	Email string
}

func (e *eventfulPerson) Modes() []domain.EntityMode {
	return []domain.EntityMode{domain.ModeInsert}
}
func (e *eventfulPerson) BuildRules(_ string, _ domain.Service, _ *domain.Rules) {
	e.RegisterEvent(domain.DomainEvent{Type: domain.EventLog, Msg: "person.created"})
}

func eventfulSchema() *core.TableSchema {
	return core.NewTableSchema[*eventfulPerson]("flat_persons").
		ID("id").
		Field("Name", "name").
		Field("Email", "email").
		CreatedAt("created_at").
		UpdatedAt("updated_at")
}

type capturingPublisher struct{ events []domain.DomainEvent }

func (c *capturingPublisher) Publish(_ persistence.RequestContext, _ domain.Event) error { return nil }
func (c *capturingPublisher) PublishAll(_ persistence.RequestContext, evs []domain.DomainEvent) error {
	c.events = append(c.events, evs...)
	return nil
}

func createAuditTable(t *testing.T, raw *sql.DB) {
	t.Helper()
	ctx := context.Background()
	if _, err := raw.ExecContext(ctx, `DROP TABLE IF EXISTS audit_events`); err != nil {
		t.Fatalf("drop audit_events: %v", err)
	}
	// Mirrors the mysql framework migration's audit_events (the columns
	// InsertAuditEventMySQL writes). id has NO default — the Go path supplies it.
	if _, err := raw.ExecContext(ctx, `CREATE TABLE audit_events (
		id           BINARY(16)   NOT NULL,
		aggregate_id CHAR(36)     NOT NULL,
		entity_type  VARCHAR(255) NOT NULL,
		verb         VARCHAR(32)  NOT NULL,
		action_name  VARCHAR(64)  NOT NULL,
		kind         VARCHAR(16)  NOT NULL,
		actor        VARCHAR(255) NULL,
		actor_issuer VARCHAR(255) NULL,
		tenant_id    VARCHAR(255) NULL,
		thread_id    CHAR(36)     NOT NULL,
		trace_id     VARCHAR(32)  NULL,
		occurred_at  DATETIME     NOT NULL,
		created_at   DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
		payload      JSON         NOT NULL,
		PRIMARY KEY (id)
	)`); err != nil {
		t.Fatalf("create audit_events: %v", err)
	}
	t.Cleanup(func() { _, _ = raw.ExecContext(context.Background(), `DROP TABLE IF EXISTS audit_events`) })
}

func TestMySQLEngine_AuditAndEvents(t *testing.T) {
	eng, raw := setup(t)
	createAuditTable(t, raw)

	pub := &capturingPublisher{}
	eng.WithAudit(&audit.Config{Destinations: []audit.Destination{audit.DestinationDatabase, audit.DestinationSlog}}, nil, nil)
	eng.WithEventPublisher(pub)

	ctx := ctxFor()
	person := &eventfulPerson{Name: "Audrey", Email: "audrey@audit"}
	ins, err := domain.GetInsertable(person, nil, "GetInsertable")
	if err != nil {
		t.Fatalf("GetInsertable: %v", err)
	}
	res, err := eng.Insert(ctx, ins, eventfulSchema(), core.WriteHook{})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	// In-TX audit_events row landed.
	var n int
	var verb, entityType, kind string
	if err := raw.QueryRow(
		`SELECT COUNT(*), MAX(verb), MAX(entity_type), MAX(kind) FROM audit_events WHERE aggregate_id = ?`, res.ID.Value(),
	).Scan(&n, &verb, &entityType, &kind); err != nil {
		t.Fatalf("read audit row: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 audit row for the insert, got %d", n)
	}
	if verb != "insert" {
		t.Errorf("audit verb = %q, want insert", verb)
	}
	if kind != "snapshot" {
		t.Errorf("audit kind = %q, want snapshot", kind)
	}

	// Read it back through the backend-neutral audit reader — the same
	// read.NewAuditReader path a service mounts — proving FindByAggregate/FindByID
	// run on a real MySQL (CHAR(36) id bound as text, "?" placeholder, no-rows
	// → sentinel). The dialect twin of the Postgres read-back coverage.
	reader := read.NewAuditReader(eng)
	byAgg, err := reader.FindByAggregate(ctx, entityType, res.ID.Value())
	if err != nil {
		t.Fatalf("FindByAggregate: %v", err)
	}
	if len(byAgg) != 1 {
		t.Fatalf("FindByAggregate want 1 event, got %d", len(byAgg))
	}
	if byAgg[0].Verb != "insert" || byAgg[0].Kind != "snapshot" || byAgg[0].EntityID != res.ID.Value() {
		t.Errorf("read-back event drifted: %+v", byAgg[0])
	}

	// The audit row id is Go-generated and not surfaced by Insert; read it from
	// the table to drive FindByID.
	var rowIDRaw []byte
	if err := raw.QueryRow(`SELECT id FROM audit_events WHERE aggregate_id = ?`, res.ID.Value()).Scan(&rowIDRaw); err != nil {
		t.Fatalf("read audit row id: %v", err)
	}
	// The ID is BINARY(16) now (the framework id standard) — decode the 16
	// raw bytes back to the uuid the reader's codec-bound FindByID expects.
	rowUUID, err := uuid.FromBytes(rowIDRaw)
	if err != nil {
		t.Fatalf("audit row id is not a 16-byte uuid: %v", err)
	}
	byID, err := reader.FindByID(ctx, rowUUID)
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if byID.EntityID != res.ID.Value() || byID.EntityType != entityType {
		t.Errorf("FindByID drifted: %+v", byID)
	}
	if _, err := reader.FindByID(ctx, uuid.New()); !errors.Is(err, appaudit.ErrAuditNotFound) {
		t.Errorf("unknown id must map to ErrAuditNotFound, got %v", err)
	}

	// Domain event published post-commit.
	if len(pub.events) != 1 || pub.events[0].Msg != "person.created" {
		t.Fatalf("publisher events = %+v, want one person.created", pub.events)
	}

	// Audit OFF → no row, no publish (separate engine config).
	eng.WithAudit(nil, nil, nil)
	pub2 := &capturingPublisher{}
	eng.WithEventPublisher(pub2)
	p2 := &eventfulPerson{Name: "NoAudit", Email: "noaudit@x"}
	ins2, _ := domain.GetInsertable(p2, nil, "GetInsertable")
	res2, err := eng.Insert(ctx, ins2, eventfulSchema(), core.WriteHook{})
	if err != nil {
		t.Fatalf("Insert (audit off): %v", err)
	}
	if err := raw.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE aggregate_id = ?`, res2.ID.Value()).Scan(&n); err != nil {
		t.Fatalf("read audit row (off): %v", err)
	}
	if n != 0 {
		t.Errorf("audit disabled must write no row, got %d", n)
	}
	// Publisher still fires regardless of audit config (independent feature).
	if len(pub2.events) != 1 {
		t.Errorf("publisher should fire independent of audit, got %d events", len(pub2.events))
	}
}
