package infra

import (
	"errors"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/persistence"
	"github.com/ClaudioSchirmer/omnicore/domain"
)

// capturingPublisher is a fake events.Publisher recording every PublishAll
// call so the wiring can be asserted without a real slog/Kafka transport.
type capturingPublisher struct {
	calls [][]domain.DomainEvent
	err   error
}

func (c *capturingPublisher) Publish(_ persistence.RequestContext, _ domain.Event) error {
	return c.err
}

func (c *capturingPublisher) PublishAll(_ persistence.RequestContext, evs []domain.DomainEvent) error {
	c.calls = append(c.calls, evs)
	return c.err
}

// eventEmittingEntity registers a domain event during BuildRules — the only
// point that survives resetEntity at the start of GetInsertable, so the event
// is carried onto the resulting ValidEntity and reaches the persister.
type eventEmittingEntity struct {
	domain.BaseEntity
	Name string
}

func (e *eventEmittingEntity) Modes() []domain.EntityMode {
	return []domain.EntityMode{
		domain.ModeInsert, domain.ModeUpdate, domain.ModeDelete,
		domain.ModeArchive, domain.ModeUnarchive,
	}
}

func (e *eventEmittingEntity) BuildRules(_ string, _ domain.Service, _ *domain.Rules) {
	e.RegisterEvent(domain.DomainEvent{Type: domain.EventLog, Class: "EventEmitting", Msg: "created"})
}

var eventEmittingSchema = NewTableSchema[*eventEmittingEntity]("event_entities").
	PK("id").
	Field("Name", "name").
	SoftDelete("deleted_at").
	CreatedAt("created_at").
	UpdatedAt("updated_at")

func TestPostgres_WithEventPublisher_SetsField(t *testing.T) {
	pub := &capturingPublisher{}
	pg := newFakePostgres(newFakePool()).WithEventPublisher(pub)
	if pg.publisher != pub {
		t.Fatal("WithEventPublisher did not set the publisher")
	}
}

func TestPostgres_publishEvents_Branches(t *testing.T) {
	ctx := newBuilderCtx()
	evs := []domain.DomainEvent{{Type: domain.EventLog, Msg: "x"}}

	// nil publisher → no-op, no panic.
	(&Postgres{}).publishEvents(ctx, evs)

	// empty events → publisher not called.
	pub := &capturingPublisher{}
	pg := &Postgres{publisher: pub}
	pg.publishEvents(ctx, nil)
	if len(pub.calls) != 0 {
		t.Fatalf("empty events should not call the publisher, got %d calls", len(pub.calls))
	}

	// with events → PublishAll called once with those events.
	pg.publishEvents(ctx, evs)
	if len(pub.calls) != 1 || len(pub.calls[0]) != 1 {
		t.Fatalf("expected 1 PublishAll with 1 event, got %v", pub.calls)
	}

	// publisher error + nil logger → swallowed via slog.Default(), no panic.
	errPub := &capturingPublisher{err: errors.New("boom")}
	(&Postgres{publisher: errPub}).publishEvents(ctx, evs)
}

func TestPostgres_Insert_PublishesDomainEvents(t *testing.T) {
	pool := newFakePool()
	pub := &capturingPublisher{}
	pg := newFakePostgres(pool).WithEventPublisher(pub)

	ins, err := domain.GetInsertable(&eventEmittingEntity{Name: "alice"}, nil, "GetInsertable")
	if err != nil {
		t.Fatalf("GetInsertable: %v", err)
	}
	if _, err := pg.Insert(newBuilderCtx(), ins, eventEmittingSchema, writeHook{}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if !pool.tx.committed {
		t.Fatal("expected the transaction to commit")
	}
	if len(pub.calls) != 1 || len(pub.calls[0]) != 1 {
		t.Fatalf("committed write should publish 1 event, got %v", pub.calls)
	}
	if pub.calls[0][0].Msg != "created" {
		t.Errorf("published event Msg = %q, want %q", pub.calls[0][0].Msg, "created")
	}
}

func TestPostgres_Insert_NoPublisher_NoPanic(t *testing.T) {
	pool := newFakePool()
	pg := newFakePostgres(pool) // no publisher wired
	ins, err := domain.GetInsertable(&eventEmittingEntity{Name: "bob"}, nil, "GetInsertable")
	if err != nil {
		t.Fatalf("GetInsertable: %v", err)
	}
	if _, err := pg.Insert(newBuilderCtx(), ins, eventEmittingSchema, writeHook{}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
}
