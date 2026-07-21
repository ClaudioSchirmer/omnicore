package write

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// White-box coverage for the bodyless-verb outbox payloads (lifecycle_payload.go):
// ARCHIVED/UNARCHIVED carry the full field map + the soft-delete column,
// DELETED carries the structural keys (PK + shared-base FK). Payloads are read
// back off the recording fake's captured args and decoded as JSON — the same
// bytes a CDC consumer would see.

// outboxPayloadFor returns the decoded JSON payload of the recorded outbox row
// matching (table, eventType), failing the test when absent or malformed.
func outboxPayloadFor(t *testing.T, tx *recTx, table, eventType string) map[string]any {
	t.Helper()
	for i, sql := range tx.execs {
		if !strings.HasPrefix(sql, "INSERT INTO outbox") {
			continue
		}
		args := tx.execArgs[i]
		// args[0] is the Go-minted outbox row id; table/eventType/payload follow.
		if len(args) < 5 || args[1] != table || args[2] != eventType {
			continue
		}
		// The payload binds as TEXT (a string arg): the JSON column is
		// text-shaped on every dialect, and SQL Server refuses the implicit
		// varbinary→NVARCHAR conversion a []byte bind would need.
		data, ok := args[4].(string)
		if !ok {
			t.Fatalf("outbox payload arg must bind as string (JSON text), got %T", args[3])
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(data), &m); err != nil {
			t.Fatalf("outbox payload for %s/%s is not a JSON object: %v (%s)", table, eventType, err, data)
		}
		return m
	}
	t.Fatalf("no outbox row for %s/%s recorded: %v", table, eventType, tx.execs)
	return nil
}

func flatArchivableWithID(t *testing.T) (domain.Archivable, string) {
	t.Helper()
	e := &builderTestEntity{Name: "alice", Email: "a@x.com"}
	id := uuid.NewString()
	e.SetID(domain.NewID(id))
	a, err := domain.GetArchivable(e, nil, "GetArchivable")
	if err != nil {
		t.Fatalf("GetArchivable: %v", err)
	}
	return a, id
}

func TestArchive_OutboxPayloadCarriesFieldsAndDeletedAt(t *testing.T) {
	a, _ := flatArchivableWithID(t)
	tx := &recTx{}
	be := newFlatBE(&recBeginner{tx: tx})
	if err := be.Archive(newBuilderCtx(), a, builderTestSchema, firingHook); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	p := outboxPayloadFor(t, tx, "builder_test_entities", "ARCHIVED")
	if p["name"] != "alice" || p["email"] != "a@x.com" {
		t.Errorf("ARCHIVED payload must carry the field map, got %v", p)
	}
	if v, ok := p["deleted_at"]; !ok || v == nil {
		t.Errorf("ARCHIVED payload must carry a populated soft-delete column, got %v", p)
	}
}

func TestUnarchive_OutboxPayloadCarriesFieldsAndNullDeletedAt(t *testing.T) {
	e := &builderTestEntity{Name: "alice", Email: "a@x.com"}
	e.SetID(domain.NewID(uuid.NewString()))
	u, err := domain.GetUnarchivable(e, nil, "GetUnarchivable")
	if err != nil {
		t.Fatalf("GetUnarchivable: %v", err)
	}
	tx := &recTx{}
	be := newFlatBE(&recBeginner{tx: tx})
	if err := be.Unarchive(newBuilderCtx(), u, builderTestSchema, firingHook); err != nil {
		t.Fatalf("Unarchive: %v", err)
	}
	p := outboxPayloadFor(t, tx, "builder_test_entities", "UNARCHIVED")
	if p["name"] != "alice" {
		t.Errorf("UNARCHIVED payload must carry the field map, got %v", p)
	}
	if v, ok := p["deleted_at"]; !ok || v != nil {
		t.Errorf("UNARCHIVED payload must carry an explicit null soft-delete column, got %v", p)
	}
}

func TestDelete_OutboxPayloadIsPKOnly(t *testing.T) {
	e := &builderTestEntity{Name: "alice", Email: "a@x.com"}
	id := uuid.NewString()
	e.SetID(domain.NewID(id))
	d, err := domain.GetDeletable(e, nil, "GetDeletable")
	if err != nil {
		t.Fatalf("GetDeletable: %v", err)
	}
	tx := &recTx{}
	be := newFlatBE(&recBeginner{tx: tx})
	if err := be.Delete(newBuilderCtx(), d, builderTestSchema, firingHook); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	p := outboxPayloadFor(t, tx, "builder_test_entities", "DELETED")
	if len(p) != 2 || p["id"] != id {
		t.Errorf("DELETED payload must be the PK + the _ids block, got %v", p)
	}
	ids, ok := p["_ids"].(map[string]any)
	if !ok || ids["id"] != id {
		t.Errorf("DELETED payload must carry _ids with the aggregate PK, got %v", p)
	}
}

func TestDeleteRoleWithBase_OutboxPayloadCarriesPKAndBaseFK(t *testing.T) {
	tx := &recTx{queryFn: rowsNone()}
	be := newFlatBE(&recBeginner{tx: tx})
	del := roleTestDeletable(t)
	if err := be.Delete(newBuilderCtx(), del, roleTestSchema(), firingHook); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	p := outboxPayloadFor(t, tx, "aluno", "DELETED")
	if p["id"] != del.ID().Value() {
		t.Errorf("role DELETED payload must carry the role PK, got %v", p)
	}
	if p["pessoa_id"] != deterministicBaseID("D1") {
		t.Errorf("role DELETED payload must carry the shared-base FK, got %v", p)
	}
	if len(p) != 3 {
		t.Errorf("role DELETED payload must be the structural keys + the _ids block, got %v", p)
	}
	ids, ok := p["_ids"].(map[string]any)
	if !ok || ids["base_id"] != deterministicBaseID("D1") {
		t.Errorf("role DELETED payload must carry _ids with the base id, got %v", p)
	}
}

func TestDeleteRoleWithBase_PurgedBase_OutboxPayloadIsBasePK(t *testing.T) {
	tx := &recTx{queryFn: func(string, []any) (Rows, error) { return &fakeRows{remaining: 0}, nil }}
	be := newFlatBE(&recBeginner{tx: tx})
	if err := be.Delete(newBuilderCtx(), roleTestDeletable(t), roleTestSchemaPurge(), firingHook); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	baseID := deterministicBaseID("D1")
	p := outboxPayloadFor(t, tx, "pessoa", "DELETED")
	if len(p) != 2 || p["id"] != baseID {
		t.Errorf("base purge DELETED payload must be the base PK + _ids, got %v", p)
	}
	if ids, ok := p["_ids"].(map[string]any); !ok || ids["id"] != baseID || ids["base_purged"] != true {
		t.Errorf("the purge row must carry _ids with the purge flag (every framework event is v2), got %v", p)
	}
}

// roleArchTestEntity mirrors roleTestEntity with the archive/unarchive modes
// enabled, so the role soft-write payloads can be exercised.
type roleArchTestEntity struct {
	domain.BaseEntity
	Name      string // shared (lives on the base)
	Document  string // shared + natural key
	Matricula string // role-own
}

func (e *roleArchTestEntity) Modes() []domain.EntityMode {
	return []domain.EntityMode{
		domain.ModeInsert, domain.ModeUpdate, domain.ModeDelete,
		domain.ModeArchive, domain.ModeUnarchive,
	}
}
func (e *roleArchTestEntity) BuildRules(string, domain.Service, *domain.Rules) {}

func roleArchTestSchema() *TableSchema {
	base := NewSharedBase("pessoa").Revision("revision").
		PK("id").
		Field("Name", "name").
		Field("Document", "document").
		NaturalKey("document")
	return NewTableSchema[*roleArchTestEntity]("aluno").
		PK("id").
		Field("Matricula", "matricula").
		SoftDelete("deleted_at").
		SharedBase(base, "pessoa_id")
}

func TestArchiveRoleWithBase_OutboxPayloadCarriesBaseFK(t *testing.T) {
	e := &roleArchTestEntity{Name: "Ana", Document: "D1", Matricula: "M1"}
	e.SetID(domain.NewID(uuid.NewString()))
	a, err := domain.GetArchivable(e, nil, "GetArchivable")
	if err != nil {
		t.Fatalf("GetArchivable: %v", err)
	}
	tx := &recTx{queryFn: rowsNone()}
	be := newFlatBE(&recBeginner{tx: tx})
	if err := be.Archive(newBuilderCtx(), a, roleArchTestSchema(), firingHook); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	p := outboxPayloadFor(t, tx, "aluno", "ARCHIVED")
	if p["matricula"] != "M1" {
		t.Errorf("role ARCHIVED payload must carry the role fields, got %v", p)
	}
	if p["pessoa_id"] != deterministicBaseID("D1") {
		t.Errorf("role ARCHIVED payload must carry the shared-base FK, got %v", p)
	}
	if v, ok := p["deleted_at"]; !ok || v == nil {
		t.Errorf("role ARCHIVED payload must carry a populated soft-delete column, got %v", p)
	}
}

func TestArchiveAggregate_OutboxPayloadRootCarriesDeletedAt(t *testing.T) {
	root := &aggWriteRoot{Name: "r"}
	root.SetID(domain.NewID(uuid.NewString()))
	root.AggregateConstructor([]domain.AggregateValueObject{aggWriteChild{ID: domain.NewIDFromUUID(uuid.New()), Label: "c"}})
	a, err := domain.GetArchivable(root, nil, "GetArchivable")
	if err != nil {
		t.Fatalf("GetArchivable: %v", err)
	}
	tx := &recTx{}
	be := newFlatBE(&recBeginner{tx: tx})
	if err := be.Archive(newBuilderCtx(), a, aggWriteSchema(), firingHook); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	p := outboxPayloadFor(t, tx, "agg_w", "ARCHIVED")
	if p["name"] != "r" {
		t.Errorf("aggregate ARCHIVED payload must carry the root fields flat at the top, got %v", p)
	}
	if v, present := p["deleted_at"]; !present || v == nil {
		t.Errorf("aggregate ARCHIVED payload must carry a populated soft-delete column, got %v", p)
	}
	ch, present := p["_children"].(map[string]any)
	if !present {
		t.Fatalf("aggregate ARCHIVED payload must carry _children, got %v", p)
	}
	items, _ := ch["aggWriteChild"].([]any)
	if len(items) != 1 {
		t.Fatalf("aggregate ARCHIVED _children must list the loaded child, got %v", ch)
	}
	item, _ := items[0].(map[string]any)
	if item["_op"] != "noop" {
		t.Errorf("a soft verb lists children as _op noop (the cascade is verb-implied), got %v", item)
	}
}

func TestDeleteAggregate_OutboxPayloadIsPKOnly(t *testing.T) {
	root := &aggWriteRoot{Name: "r"}
	id := uuid.NewString()
	root.SetID(domain.NewID(id))
	root.AggregateConstructor([]domain.AggregateValueObject{aggWriteChild{ID: domain.NewIDFromUUID(uuid.New()), Label: "c"}})
	d, err := domain.GetDeletable(root, nil, "GetDeletable")
	if err != nil {
		t.Fatalf("GetDeletable: %v", err)
	}
	tx := &recTx{}
	be := newFlatBE(&recBeginner{tx: tx})
	if err := be.Delete(newBuilderCtx(), d, aggWriteSchema(), firingHook); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	p := outboxPayloadFor(t, tx, "agg_w", "DELETED")
	if len(p) != 2 || p["id"] != id {
		t.Errorf("aggregate DELETED payload must be the root PK + the _ids block, got %v", p)
	}
	if ids, ok := p["_ids"].(map[string]any); !ok || ids["id"] != id {
		t.Errorf("aggregate DELETED payload must carry _ids, got %v", p)
	}
}

// ─── helper edge cases ───────────────────────────────────────────────────────

func TestSharedBaseFKField_SharedPKModel_Skipped(t *testing.T) {
	// role.id IS the base link: the id already travels as the outbox
	// aggregate_id, so no FK field is added.
	src := &roleTestEntity{Name: "Ana", Document: "D1", Matricula: "M1"}
	if _, _, ok := sharedBaseFKField(roleTestSchemaSharedPK(), src); ok {
		t.Error("a shared-PK role must not contribute a separate FK payload field")
	}
	p := deleteKeysPayload(roleTestSchemaSharedPK(), src, "some-id")
	if len(p) != 1 {
		t.Errorf("shared-PK DELETED payload must be exactly the PK, got %v", p)
	}
}

func TestSharedBaseFKField_EmptyNaturalKey_Skipped(t *testing.T) {
	// Payload assembly must never veto a write the verb itself allows: an
	// unreadable natural key just omits the FK field.
	src := &roleTestEntity{Name: "Ana", Document: "", Matricula: "M1"}
	if _, _, ok := sharedBaseFKField(roleTestSchema(), src); ok {
		t.Error("an empty natural key must omit the FK payload field, not resolve one")
	}
}

func TestSharedBaseFKField_NoSharedBase_Skipped(t *testing.T) {
	if _, _, ok := sharedBaseFKField(builderTestSchema, &builderTestEntity{}); ok {
		t.Error("a schema without a shared base must not contribute an FK payload field")
	}
}
