package write

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// White-box coverage of the v2 outbox payload builder: the one shape every
// body verb emits — scalars flat at the top (root ∪ base ∪ siblings ∪ verb
// timestamps), the _ids structural block, and the _children/_base_children
// groups with per-item _op verbs.

func TestBuildWritePayloadV2_FlatInsertShape(t *testing.T) {
	e := &builderTestEntity{Name: "alice"}
	id := uuid.NewString()
	p := buildWritePayloadV2(builderTestSchema, e, nil, "INSERTED", testNow,
		builderTestSchema.WriteFields(e), outboxMeta{ID: id})

	if p["name"] != "alice" {
		t.Errorf("root fields must land flat at the top, got %v", p)
	}
	ids, ok := p["_ids"].(map[string]any)
	if !ok || ids["id"] != id {
		t.Fatalf("_ids must carry the aggregate PK, got %v", p)
	}
	if _, has := ids["base_id"]; has {
		t.Errorf("a schema without a shared base must not carry base ids, got %v", ids)
	}
	if _, has := p["_children"]; has {
		t.Errorf("a flat entity has no _children block, got %v", p)
	}
}

func TestBuildWritePayloadV2_TimestampsByVerb(t *testing.T) {
	schema := NewTableSchema[*builderTestEntity]("t").PK("id").Revision("revision").
		Field("Name", "name").
		SoftDelete("deleted_at").CreatedAt("created_at").UpdatedAt("updated_at")
	e := &builderTestEntity{Name: "a"}
	meta := outboxMeta{ID: uuid.NewString()}

	ins := buildWritePayloadV2(schema, e, nil, "INSERTED", testNow, schema.WriteFields(e), meta)
	if ins["created_at"] != testNow || ins["updated_at"] != testNow {
		t.Errorf("INSERTED must carry created_at + updated_at = the op stamp, got %v", ins)
	}
	upd := buildWritePayloadV2(schema, e, nil, "UPDATED", testNow, schema.WriteFields(e), meta)
	if _, has := upd["created_at"]; has {
		t.Errorf("UPDATED must NOT carry created_at (absent key = untouched on $set), got %v", upd)
	}
	if upd["updated_at"] != testNow {
		t.Errorf("UPDATED must carry updated_at = the op stamp, got %v", upd)
	}
	arc := buildWritePayloadV2(schema, e, nil, "ARCHIVED", testNow, schema.WriteFields(e), meta)
	if arc["deleted_at"] != testNow {
		t.Errorf("ARCHIVED must carry the soft-delete stamp, got %v", arc)
	}
	una := buildWritePayloadV2(schema, e, nil, "UNARCHIVED", testNow, schema.WriteFields(e), meta)
	if v, has := una["deleted_at"]; !has || v != nil {
		t.Errorf("UNARCHIVED must carry an explicit null soft-delete, got %v", una)
	}
}

// Shared-base role with one own child (insert) and one base child (noop, the
// warm's Constructor): base business fields land flat, the separate FK is
// injected, the children split into _children vs _base_children with their ops.
func TestBuildWritePayloadV2_SharedBaseRoleWithChildren(t *testing.T) {
	schema := bcRoleSchema(true) // aluno + base pessoa with base-child endereco
	e := &bcRole{Name: "Ana", Document: "D1", Matricula: "M1"}
	addr := bcAddr{Street: "Main St"}
	domain.AddAggregateChild(e, addr)
	root, _ := any(e).(domain.AggregateRootProvider)
	baseID := deterministicBaseID("D1")

	p := buildWritePayloadV2(schema, e, root.GetAggregateRoot(), "INSERTED", testNow,
		schema.WriteFields(e), outboxMeta{ID: uuid.NewString(), BaseID: baseID, BaseRevision: 7})

	if p["name"] != "Ana" || p["document"] != "D1" {
		t.Errorf("base business fields must land flat at the top, got %v", p)
	}
	if got := p["pessoa_id"]; got != domain.NewID(baseID) {
		t.Errorf("the separate FK column must carry the base id, got %v", got)
	}
	ids := p["_ids"].(map[string]any)
	if ids["base_id"] != baseID || ids["base_revision"] != int64(7) {
		t.Errorf("_ids must carry base_id + base_revision, got %v", ids)
	}
	bch, ok := p["_base_children"].(map[string]any)
	if !ok {
		t.Fatalf("the base child must land under _base_children, got %v", p)
	}
	items := bch["bcAddr"].([]map[string]any)
	if len(items) != 1 || items[0]["_op"] != "insert" || items[0]["street"] != "Main St" {
		t.Errorf("base child must carry _op + fields, got %v", items)
	}
	if _, has := p["_children"]; has {
		t.Errorf("no own children were added — _children must be absent, got %v", p)
	}
}

func TestChildOpName_Mapping(t *testing.T) {
	withSD := NewTableSchema[*builderTestEntity]("c").PK("id").FK("r_id").
		Field("Name", "name").SoftDelete("deleted_at")
	noSD := NewTableSchema[*builderTestEntity]("c2").PK("id").FK("r_id").
		Field("Name", "name")

	if got := childOpName(domain.OperationOf(domain.StatusAdded, domain.StatusAdded), false, false, withSD); got != "insert" {
		t.Errorf("new item → insert, got %q", got)
	}
	if got := childOpName(domain.OperationOf(domain.StatusConstructor, domain.StatusChanged), false, false, withSD); got != "update" {
		t.Errorf("DB item changed → update, got %q", got)
	}
	if got := childOpName(domain.OperationOf(domain.StatusConstructor, domain.StatusConstructor), false, false, withSD); got != "noop" {
		t.Errorf("untouched DB item → noop, got %q", got)
	}
	if got := childOpName(domain.OperationOf(domain.StatusConstructor, domain.StatusRemoved), false, false, withSD); got != "archive" {
		t.Errorf("removed (soft-deletable) → archive, got %q", got)
	}
	if got := childOpName(domain.OperationOf(domain.StatusConstructor, domain.StatusRemoved), false, true, noSD); got != "delete" {
		t.Errorf("removed base-child without soft-delete → delete, got %q", got)
	}
	if got := childOpName(domain.OperationOf(domain.StatusConstructor, domain.StatusChanged), true, false, withSD); got != "noop" {
		t.Errorf("soft verbs list every child as noop, got %q", got)
	}
}

func TestBuildDeletePayloadV2_KeysGrowOnly(t *testing.T) {
	e := &roleTestEntity{Name: "Ana", Document: "D1", Matricula: "M1"}
	id := uuid.NewString()
	baseID := deterministicBaseID("D1")
	p := buildDeletePayloadV2(roleTestSchema(), e, id, outboxMeta{ID: id, BaseID: baseID, BaseRevision: 3, BasePurged: true})

	// The historical structural keys survive untouched…
	if p["id"] != domain.NewID(id) || p["pessoa_id"] != domain.NewID(baseID) {
		t.Errorf("the legacy structural keys must survive, got %v", p)
	}
	// …and the _ids block only ADDS.
	ids := p["_ids"].(map[string]any)
	if ids["base_purged"] != true || ids["base_revision"] != int64(3) {
		t.Errorf("_ids must carry the purge flag + revision, got %v", ids)
	}
}

func TestBuildBaseUpdate_BumpsRevisionInOneStatement(t *testing.T) {
	base := NewSharedBase("pessoa").Revision("revision").PK("id").
		Field("Name", "name").Field("Document", "document").NaturalKey("document")
	// Attach to resolve nothing — buildBaseUpdate reads only the base schema.
	baseID := deterministicBaseID("D1")
	sql, args := buildBaseUpdate(testPGDialect{}, base, baseID, domain.Fields{"name": "Ana"}, testNow)
	want := "UPDATE pessoa SET name = $1, revision = revision + 1 WHERE id = $2"
	if sql != want {
		t.Fatalf("sql = %q, want %q", sql, want)
	}
	if len(args) != 2 || args[0] != "Ana" || args[1] != baseID {
		t.Fatalf("args = %v, want [Ana %s]", args, baseID)
	}
}

// outboxMetaFor edge branches: no shared base → bare id; an empty natural key
// never vetoes payload assembly; a revision-read error propagates.
func TestOutboxMetaFor_Branches(t *testing.T) {
	ctx := context.Background()
	d := testPGDialect{}

	// No shared base.
	m, err := outboxMetaFor(ctx, &recTx{}, d, builderTestSchema, &builderTestEntity{Name: "a"}, "x1")
	if err != nil || m.BaseID != "" || m.ID != "x1" {
		t.Fatalf("no-base meta = %+v, %v", m, err)
	}

	// Empty natural key → bare id, no error (payload assembly never vetoes).
	m, err = outboxMetaFor(ctx, &recTx{}, d, roleTestSchema(), &roleTestEntity{Name: "Ana", Document: ""}, "x2")
	if err != nil || m.BaseID != "" {
		t.Fatalf("empty-nk meta = %+v, %v", m, err)
	}

	// Revision read error propagates.
	tx := &recTx{queryFn: func(string, []any) (Rows, error) { return nil, errBoom }}
	if _, err := outboxMetaFor(ctx, tx, d, roleTestSchema(), &roleTestEntity{Name: "Ana", Document: "D1"}, "x3"); !errors.Is(err, errBoom) {
		t.Fatalf("revision read error must propagate, got %v", err)
	}
}

// The sibling fields of a materialized sibling row merge flat into the payload;
// an all-nil sibling is omitted, mirroring the write.
func TestBuildWritePayloadV2_SiblingsMergeFlat(t *testing.T) {
	sib := NewSiblingSchema[*sibTestEntity]("usuario_login").Field("UserName", "user_name")
	schema := NewTableSchema[*sibTestEntity]("usuario").PK("id").Revision("revision").
		Field("Name", "name").Sibling(sib)
	un := "alice"
	e := &sibTestEntity{Name: "Ana", UserName: &un}
	p := buildWritePayloadV2(schema, e, nil, "INSERTED", testNow, schema.WriteFields(e), outboxMeta{ID: "u1"})
	if got, _ := p["user_name"].(*string); got == nil || *got != "alice" {
		t.Errorf("sibling fields must merge flat, got %v", p)
	}
	// All-nil sibling omitted.
	e2 := &sibTestEntity{Name: "Bo"}
	p2 := buildWritePayloadV2(schema, e2, nil, "INSERTED", testNow, schema.WriteFields(e2), outboxMeta{ID: "u2"})
	if _, has := p2["user_name"]; has {
		t.Errorf("an all-nil sibling must be omitted, got %v", p2)
	}
}
