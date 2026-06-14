//go:build integration

package infra

import (
	"context"
	"errors"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// --- SyncEngine.process bypasses Kafka (callable on its own) -------------

func TestSyncEngine_Process_InsertEventUpsertsDoc(t *testing.T) {
	pg, cleanupPG := newTestPG(t)
	defer cleanupPG()
	m, cleanupMongo := newTestMongo(t)
	defer cleanupMongo()

	createTable(t, pg, `CREATE TABLE syn_persons (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		name TEXT NOT NULL,
		deleted_at TIMESTAMP,
		created_at TIMESTAMP NOT NULL DEFAULT NOW(),
		updated_at TIMESTAMP NOT NULL DEFAULT NOW()
	)`)
	var id string
	pg.Pool().QueryRow(context.Background(),
		`INSERT INTO syn_persons (name) VALUES ('Alice') RETURNING id`).Scan(&id)

	view := View("syn_persons").Root("syn_persons").Version(1)
	engine := NewSyncEngine(pg, m, nil, "", []*ViewDefinition{view}, 1)

	err := engine.process(context.Background(), kafkaEvent{
		AggregateType: "syn_persons",
		EventType:     "INSERTED",
		AggregateID:   id,
	})
	if err != nil {
		t.Fatalf("process INSERTED: %v", err)
	}
	if doc := mongoDoc(t, m, "syn_persons", id); doc == nil || doc["name"] != "Alice" {
		t.Errorf("expected doc after INSERTED, got %+v", doc)
	}
}

func TestSyncEngine_Process_DeletedRemovesDoc(t *testing.T) {
	pg, cleanupPG := newTestPG(t)
	defer cleanupPG()
	m, cleanupMongo := newTestMongo(t)
	defer cleanupMongo()

	createTable(t, pg, `CREATE TABLE syn_x (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		name TEXT NOT NULL,
		deleted_at TIMESTAMP,
		created_at TIMESTAMP NOT NULL DEFAULT NOW(),
		updated_at TIMESTAMP NOT NULL DEFAULT NOW()
	)`)
	var id string
	pg.Pool().QueryRow(context.Background(),
		`INSERT INTO syn_x (name) VALUES ('Bob') RETURNING id`).Scan(&id)

	// Seed Mongo with the doc, then DELETE in PG, then process.
	m.Upsert(context.Background(), "syn_x", id, bson.M{"_id": id, "name": "Bob"})
	pg.Pool().Exec(context.Background(), `DELETE FROM syn_x WHERE id = $1`, id)

	view := View("syn_x").Root("syn_x").Version(1)
	engine := NewSyncEngine(pg, m, nil, "", []*ViewDefinition{view}, 1)
	err := engine.process(context.Background(), kafkaEvent{
		AggregateType: "syn_x",
		EventType:     "DELETED",
		AggregateID:   id,
	})
	if err != nil {
		t.Fatalf("process DELETED: %v", err)
	}
	if mongoDoc(t, m, "syn_x", id) != nil {
		t.Error("expected doc removed after DELETED")
	}
}

func TestSyncEngine_Process_ArchivedDefault_KeepsDoc(t *testing.T) {
	pg, cleanupPG := newTestPG(t)
	defer cleanupPG()
	m, cleanupMongo := newTestMongo(t)
	defer cleanupMongo()

	createTable(t, pg, `CREATE TABLE syn_kept (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		name TEXT NOT NULL,
		deleted_at TIMESTAMP,
		created_at TIMESTAMP NOT NULL DEFAULT NOW(),
		updated_at TIMESTAMP NOT NULL DEFAULT NOW()
	)`)
	var id string
	pg.Pool().QueryRow(context.Background(),
		`INSERT INTO syn_kept (name, deleted_at) VALUES ('C', NOW()) RETURNING id`).Scan(&id)

	view := View("syn_kept").Root("syn_kept").Version(1) // default keep-on-archive
	engine := NewSyncEngine(pg, m, nil, "", []*ViewDefinition{view}, 1)
	if err := engine.process(context.Background(), kafkaEvent{
		AggregateType: "syn_kept",
		EventType:     "ARCHIVED",
		AggregateID:   id,
	}); err != nil {
		t.Fatalf("process ARCHIVED: %v", err)
	}
	if mongoDoc(t, m, "syn_kept", id) == nil {
		t.Error("default ARCHIVED should KEEP doc")
	}
}

func TestSyncEngine_Process_ArchivedDeleteOnArchive_RemovesDoc(t *testing.T) {
	pg, cleanupPG := newTestPG(t)
	defer cleanupPG()
	m, cleanupMongo := newTestMongo(t)
	defer cleanupMongo()

	createTable(t, pg, `CREATE TABLE syn_hot (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		name TEXT NOT NULL,
		deleted_at TIMESTAMP,
		created_at TIMESTAMP NOT NULL DEFAULT NOW(),
		updated_at TIMESTAMP NOT NULL DEFAULT NOW()
	)`)
	var id string
	pg.Pool().QueryRow(context.Background(),
		`INSERT INTO syn_hot (name, deleted_at) VALUES ('D', NOW()) RETURNING id`).Scan(&id)
	m.Upsert(context.Background(), "syn_hot", id, bson.M{"_id": id, "name": "D"})

	view := View("syn_hot").Root("syn_hot").DeleteOnArchive().Version(1)
	engine := NewSyncEngine(pg, m, nil, "", []*ViewDefinition{view}, 1)
	if err := engine.process(context.Background(), kafkaEvent{
		AggregateType: "syn_hot",
		EventType:     "ARCHIVED",
		AggregateID:   id,
	}); err != nil {
		t.Fatalf("process ARCHIVED: %v", err)
	}
	if mongoDoc(t, m, "syn_hot", id) != nil {
		t.Error("DeleteOnArchive view should REMOVE doc on ARCHIVED")
	}
}

func TestSyncEngine_Process_UnknownAggregateTypeIsSilentNoop(t *testing.T) {
	pg, cleanupPG := newTestPG(t)
	defer cleanupPG()
	m, cleanupMongo := newTestMongo(t)
	defer cleanupMongo()

	view := View("known").Root("known").Version(1)
	engine := NewSyncEngine(pg, m, nil, "", []*ViewDefinition{view}, 1)
	err := engine.process(context.Background(), kafkaEvent{
		AggregateType: "ghost",
		EventType:     "INSERTED",
		AggregateID:   "abc",
	})
	if err != nil {
		t.Errorf("unknown aggregate_type should not fail, got %v", err)
	}
}

func TestSyncEngine_Process_AbsentRootSkipsUpsert(t *testing.T) {
	pg, cleanupPG := newTestPG(t)
	defer cleanupPG()
	m, cleanupMongo := newTestMongo(t)
	defer cleanupMongo()

	createTable(t, pg, `CREATE TABLE syn_absent (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		name TEXT NOT NULL,
		deleted_at TIMESTAMP
	)`)
	view := View("syn_absent").Root("syn_absent").Version(1)
	engine := NewSyncEngine(pg, m, nil, "", []*ViewDefinition{view}, 1)

	// Event for an id that does not exist in PG → composer returns nil → no upsert.
	err := engine.process(context.Background(), kafkaEvent{
		AggregateType: "syn_absent",
		EventType:     "INSERTED",
		AggregateID:   "00000000-0000-0000-0000-000000000000",
	})
	if err != nil {
		t.Errorf("absent root should not fail process(), got %v", err)
	}
	if mongoDoc(t, m, "syn_absent", "00000000-0000-0000-0000-000000000000") != nil {
		t.Error("no doc should have been written")
	}
}

func TestSyncEngine_TopicFromTableHelper(t *testing.T) {
	if got := topicFromTable("users"); got != "users.events" {
		t.Errorf("topicFromTable = %q, want users.events", got)
	}
}

// --- BaseAggregateRepository.FindByID / FindArchivedByID ------------------

func TestBaseAggregateRepository_FindByID(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createLoaderTables(t, pg)

	var id string
	pg.Pool().QueryRow(context.Background(),
		`INSERT INTO loader_roots (name, email) VALUES ('Y', 'y@x') RETURNING id`).Scan(&id)

	bar := NewBaseAggregateRepository[*loaderRoot](pg, func() *loaderRoot { return &loaderRoot{} })
	WithChild[loaderTagVO](bar.Loader)

	root, err := bar.FindByID(domain.NewID(id))
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if root.Name != "Y" {
		t.Errorf("root = %+v", root)
	}
}

func TestBaseAggregateRepository_FindArchivedByID(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createLoaderTables(t, pg)

	var id string
	pg.Pool().QueryRow(context.Background(),
		`INSERT INTO loader_roots (name, email, deleted_at) VALUES ('A', 'a@x', NOW()) RETURNING id`).Scan(&id)

	bar := NewBaseAggregateRepository[*loaderRoot](pg, func() *loaderRoot { return &loaderRoot{} })
	root, err := bar.FindArchivedByID(domain.NewID(id))
	if err != nil {
		t.Fatalf("FindArchivedByID: %v", err)
	}
	if root.Name != "A" {
		t.Errorf("root = %+v", root)
	}
}

// --- InfrastructureError helpers ----------------------------------------

func TestNewInfrastructureError(t *testing.T) {
	ctxA := domain.NewNotificationContext("A")
	e := NewInfrastructureError([]*domain.NotificationContext{ctxA})
	if e == nil || len(e.Contexts) != 1 {
		t.Errorf("NewInfrastructureError = %+v", e)
	}
}

func TestInfrastructureError_ErrorMessage(t *testing.T) {
	e := NewInfrastructureError([]*domain.NotificationContext{
		domain.NewNotificationContext("A"),
		domain.NewNotificationContext("B"),
	})
	if e.Error() != "infrastructure error: 2 context(s)" {
		t.Errorf("Error() = %q", e.Error())
	}
}

func TestInfrastructureError_NotificationContexts(t *testing.T) {
	ctxA := domain.NewNotificationContext("A")
	e := NewInfrastructureError([]*domain.NotificationContext{ctxA})
	if got := e.NotificationContexts(); len(got) != 1 || got[0] != ctxA {
		t.Errorf("NotificationContexts() did not preserve identity")
	}
}

func TestInfrastructureError_SatisfiesCarrier(t *testing.T) {
	var carrier domain.NotificationCarrier = NewInfrastructureError(nil)
	if carrier == nil {
		t.Error("expected non-nil carrier interface value")
	}
}

func TestInfrastructureSingleNotificationError(t *testing.T) {
	e := SingleNotificationError("X", "f", domain.RequiredFieldNotification{})
	if e == nil || len(e.Contexts) != 1 {
		t.Fatalf("%+v", e)
	}
	msgs := e.Contexts[0].Messages()
	if len(msgs) != 1 || msgs[0].FieldName != "f" {
		t.Errorf("msg = %+v", msgs)
	}
}

func TestInfrastructureFieldErrorWithCause(t *testing.T) {
	cause := errors.New("down")
	e := FieldErrorWithCause("X", "f", cause, domain.RequiredFieldNotification{})
	if e == nil || e.Contexts[0].Messages()[0].Err != cause {
		t.Errorf("cause not propagated: %+v", e)
	}
}

// --- View / Source builder methods (Embed / FromMongo) ---

func TestView_EmbedAddsOneToOneSource(t *testing.T) {
	v := View("v").Root("v").Embed("child", From("child").On("v_id")).Version(1)
	if len(v.Embeds()) != 1 || v.Embeds()[0].many {
		t.Errorf("Embed should mark many=false, got %+v", v.Embeds())
	}
}

func TestFrom_MarksIsMongoFalse(t *testing.T) {
	s := From("t").On("v_id")
	if s.IsMongo() {
		t.Error("From should mark IsMongo=false")
	}
	if s.JoinKey() != "v_id" {
		t.Errorf("JoinKey() = %q", s.JoinKey())
	}
}

func TestFromMongo_MarksIsMongoTrue(t *testing.T) {
	s := FromMongo("c").On("v_id")
	if !s.IsMongo() {
		t.Error("FromMongo should mark IsMongo=true")
	}
	if s.Collection() != "c" {
		t.Errorf("Collection() = %q", s.Collection())
	}
	if s.JoinKey() != "v_id" {
		t.Errorf("JoinKey() = %q", s.JoinKey())
	}
}

func TestSource_EmbedAndEmbedManyAppend(t *testing.T) {
	s := From("a").Embed("b", From("b")).EmbedMany("c", From("c"))
	if len(s.Embeds()) != 2 {
		t.Errorf("Source embeds len = %d", len(s.Embeds()))
	}
	if s.Embeds()[0].many != false || s.Embeds()[1].many != true {
		t.Errorf("Source embed many flags wrong: %+v", s.Embeds())
	}
}

// --- ColumnsOnly + small helpers ---

func TestColumnsOnly(t *testing.T) {
	specs := []ColumnSpec{
		{Column: "name", FieldIndex: 1},
		{Column: "email", FieldIndex: 2},
	}
	got := ColumnsOnly(specs)
	if len(got) != 2 || got[0] != "name" || got[1] != "email" {
		t.Errorf("ColumnsOnly = %v", got)
	}
}
