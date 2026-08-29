package write

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/criteria"
)

// White-box coverage of the outbox payload builder: the one shape every
// body verb emits — scalars flat at the top (root ∪ base ∪ siblings ∪ verb
// timestamps), the _ids structural block, and the _children/_base_children
// groups with per-item _op verbs.

func TestBuildWritePayload_FlatInsertShape(t *testing.T) {
	e := &builderTestEntity{Name: "alice"}
	id := uuid.NewString()
	p := buildWritePayload(builderTestSchema, e, nil, "INSERTED", testNow, CascadeStamps{},
		builderTestSchema.WriteFields(e), outboxMeta{ID: id})

	if p["name"] != "alice" {
		t.Errorf("root fields must land flat at the top, got %v", p)
	}
	ids, ok := p["_ids"].(map[string]any)
	if !ok || ids["id"] != id {
		t.Fatalf("_ids must carry the aggregate ID, got %v", p)
	}
	if _, has := ids["base_id"]; has {
		t.Errorf("a schema without a shared base must not carry base ids, got %v", ids)
	}
	if _, has := p["_children"]; has {
		t.Errorf("a flat entity has no _children block, got %v", p)
	}
}

func TestBuildWritePayload_TimestampsByVerb(t *testing.T) {
	schema := NewTableSchema[*builderTestEntity]("t").ID("id").Revision("revision").
		Field("Name", "name").
		DeletedAt("deleted_at").CreatedAt("created_at").UpdatedAt("updated_at")
	e := &builderTestEntity{Name: "a"}
	meta := outboxMeta{ID: uuid.NewString()}

	ins := buildWritePayload(schema, e, nil, "INSERTED", testNow, CascadeStamps{}, schema.WriteFields(e), meta)
	if ins["created_at"] != testNow || ins["updated_at"] != testNow {
		t.Errorf("INSERTED must carry created_at + updated_at = the op stamp, got %v", ins)
	}
	upd := buildWritePayload(schema, e, nil, "UPDATED", testNow, CascadeStamps{}, schema.WriteFields(e), meta)
	if _, has := upd["created_at"]; has {
		t.Errorf("UPDATED must NOT carry created_at (absent key = untouched on $set), got %v", upd)
	}
	if upd["updated_at"] != testNow {
		t.Errorf("UPDATED must carry updated_at = the op stamp, got %v", upd)
	}
	arc := buildWritePayload(schema, e, nil, "ARCHIVED", testNow, CascadeStamps{}, schema.WriteFields(e), meta)
	if arc["deleted_at"] != testNow {
		t.Errorf("ARCHIVED must carry the DeletedAt stamp, got %v", arc)
	}
	una := buildWritePayload(schema, e, nil, "UNARCHIVED", testNow, CascadeStamps{}, schema.WriteFields(e), meta)
	if v, has := una["deleted_at"]; !has || v != nil {
		t.Errorf("UNARCHIVED must carry an explicit null DeletedAt, got %v", una)
	}
}

// Shared-base role with one own child (insert) and one base child (noop, the
// warm's Constructor): base business fields land flat, the separate ParentID is
// injected, the children split into _children vs _base_children with their ops.
func TestBuildWritePayload_SharedBaseRoleWithChildren(t *testing.T) {
	schema := bcRoleSchema(true) // aluno + base pessoa with base-child endereco
	e := &bcRole{Name: "Ana", Document: "D1", Matricula: "M1"}
	addr := bcAddr{Street: "Main St"}
	domain.AddAggregateChild(e, addr)
	root, _ := any(e).(domain.AggregateRootProvider)
	baseID := deterministicBaseID("D1")

	p := buildWritePayload(schema, e, root.GetAggregateRoot(), "INSERTED", testNow, CascadeStamps{},
		schema.WriteFields(e), outboxMeta{ID: uuid.NewString(), BaseID: baseID, BaseRevision: 7})

	if p["name"] != "Ana" || p["document"] != "D1" {
		t.Errorf("base business fields must land flat at the top, got %v", p)
	}
	if got := p["pessoa_id"]; got != domain.NewID(baseID) {
		t.Errorf("the separate ParentID column must carry the base id, got %v", got)
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

// The UNARCHIVED payload reports the SAME set the restore statement wrote: the
// child this root's archive put to sleep comes back with an explicit null, the
// one archived on its own two hours earlier is left untouched ("noop"), so the
// projected document and the relational rows cannot drift apart.
func TestBuildWritePayload_UnarchiveRestoresOnlyTheCascadedChildren(t *testing.T) {
	schema := bcRoleSchema(true)
	e := &bcRole{Name: "Ana", Document: "D1", Matricula: "M1"}
	cascade := time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)

	withRoot := domain.WithID(bcAddr{Street: "went down with the base"}, domain.NewID("a1"))
	domain.SetManagedColumns(&withRoot, 1, nil, nil, &cascade)
	own := cascade.Add(-2 * time.Hour)
	onItsOwn := domain.WithID(bcAddr{Street: "archived on its own"}, domain.NewID("a2"))
	domain.SetManagedColumns(&onItsOwn, 1, nil, nil, &own)

	root, _ := any(e).(domain.AggregateRootProvider)
	root.GetAggregateRoot().AggregateConstructor([]domain.AggregateValueObject{withRoot, onItsOwn})

	p := buildWritePayload(schema, e, root.GetAggregateRoot(), "UNARCHIVED", testNow, CascadeStamps{Base: cascade},
		schema.WriteFields(e), outboxMeta{ID: uuid.NewString(), BaseID: deterministicBaseID("D1")})

	items := p["_base_children"].(map[string]any)["bcAddr"].([]map[string]any)
	if len(items) != 2 {
		t.Fatalf("both loaded children must travel, got %v", items)
	}
	byID := map[string]map[string]any{}
	for _, it := range items {
		byID[it["id"].(domain.ID).Value()] = it
	}
	restored, untouched := byID["a1"], byID["a2"]
	if restored == nil || untouched == nil {
		t.Fatalf("children lost their identity in the payload: %v", items)
	}
	if restored["_op"] != "unarchive" {
		t.Errorf("the child the root archived must be restored, got %v", restored)
	}
	if v, present := restored["deleted_at"]; !present || v != nil {
		t.Errorf("a restore carries the explicit null the cascade wrote, got %v", restored)
	}
	if untouched["_op"] != "noop" {
		t.Errorf("a child archived on its own must stay archived, got %v", untouched)
	}
	if _, present := untouched["deleted_at"]; present {
		t.Errorf("an untouched child must not have its stamp rewritten, got %v", untouched)
	}
}

// One verb, TWO instants. A role's own children come back from the ROLE's stamp
// and a shared base's native children from the BASE's — which are the same value
// only when the role that archived the base is the one being restored. Reporting
// either segment against the other's stamp would describe rows that never moved.
func TestBuildWritePayload_UnarchiveReadsBaseChildrenAgainstTheBaseStamp(t *testing.T) {
	schema := bcRoleSchema(true)
	e := &bcRole{Name: "Ana", Document: "D1", Matricula: "M1"}
	roleStamp := time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC) // this role went down here
	baseStamp := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC) // the base only later, with the LAST role

	withBase := domain.WithID(bcAddr{Street: "went down with the base"}, domain.NewID("a1"))
	domain.SetManagedColumns(&withBase, 1, nil, nil, &baseStamp)
	roleTimed := domain.WithID(bcAddr{Street: "carries the role's instant"}, domain.NewID("a2"))
	domain.SetManagedColumns(&roleTimed, 1, nil, nil, &roleStamp)

	root, _ := any(e).(domain.AggregateRootProvider)
	root.GetAggregateRoot().AggregateConstructor([]domain.AggregateValueObject{withBase, roleTimed})

	p := buildWritePayload(schema, e, root.GetAggregateRoot(), "UNARCHIVED", testNow,
		CascadeStamps{Own: roleStamp, Base: baseStamp},
		schema.WriteFields(e), outboxMeta{ID: uuid.NewString(), BaseID: deterministicBaseID("D1")})

	byID := map[string]map[string]any{}
	for _, it := range p["_base_children"].(map[string]any)["bcAddr"].([]map[string]any) {
		byID[it["id"].(domain.ID).Value()] = it
	}
	if op := byID["a1"]["_op"]; op != "unarchive" {
		t.Errorf("a base child is restored from the BASE's stamp, got %v", byID["a1"])
	}
	if op := byID["a2"]["_op"]; op != "noop" {
		t.Errorf("the role's instant says nothing about a base child, got %v", byID["a2"])
	}
}

// An archive that leaves another role active does NOT take the shared identity
// down, so its native children never moved — and the event must not claim they
// did. The zero Base stamp is exactly that statement: no base transition.
func TestBuildWritePayload_ArchiveWithoutBaseTransitionLeavesBaseChildrenAlone(t *testing.T) {
	schema := bcRoleSchema(true)
	e := &bcRole{Name: "Ana", Document: "D1", Matricula: "M1"}
	now := time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)

	addr := domain.WithID(bcAddr{Street: "still active under a live identity"}, domain.NewID("a1"))
	root, _ := any(e).(domain.AggregateRootProvider)
	root.GetAggregateRoot().AggregateConstructor([]domain.AggregateValueObject{addr})

	p := buildWritePayload(schema, e, root.GetAggregateRoot(), "ARCHIVED", testNow,
		CascadeStamps{Own: now}, // Base zero: another role kept the identity up
		schema.WriteFields(e), outboxMeta{ID: uuid.NewString(), BaseID: deterministicBaseID("D1")})

	items := p["_base_children"].(map[string]any)["bcAddr"].([]map[string]any)
	if len(items) != 1 || items[0]["_op"] != "noop" {
		t.Errorf("no base transition → the base children are untouched, got %v", items)
	}
	if _, present := items[0]["deleted_at"]; present {
		t.Errorf("an untouched base child must not be stamped by the event, got %v", items[0])
	}
}

func TestChildOpName_Mapping(t *testing.T) {
	if got := childOpName(domain.OperationOf(domain.StatusAdded, domain.StatusAdded), false, "UPDATED", true, false); got != "insert" {
		t.Errorf("new item → insert, got %q", got)
	}
	if got := childOpName(domain.OperationOf(domain.StatusConstructor, domain.StatusChanged), false, "UPDATED", true, false); got != "update" {
		t.Errorf("DB item changed → update, got %q", got)
	}
	if got := childOpName(domain.OperationOf(domain.StatusConstructor, domain.StatusConstructor), false, "UPDATED", true, false); got != "noop" {
		t.Errorf("untouched DB item → noop, got %q", got)
	}
	if got := childOpName(domain.OperationOf(domain.StatusConstructor, domain.StatusRemoved), false, "UPDATED", true, false); got != "archive" {
		t.Errorf("removed (archivable) → archive, got %q", got)
	}
	// The column decides, and nothing else does: a removed child that declares no
	// DeletedAt reports the DELETE the persister issued — whether it is a role's
	// own child or a shared base's native one.
	if got := childOpName(domain.OperationOf(domain.StatusConstructor, domain.StatusRemoved), false, "UPDATED", false, false); got != "delete" {
		t.Errorf("removed child without DeletedAt → delete, got %q", got)
	}

	// Soft verbs report the CASCADE the root statement performed, not the item's
	// own status: the row the statement reached takes the transition.
	if got := childOpName(domain.OperationOf(domain.StatusConstructor, domain.StatusChanged), true, "ARCHIVED", true, true); got != "archive" {
		t.Errorf("archive cascades onto the active children, got %q", got)
	}
	if got := childOpName(domain.OperationOf(domain.StatusConstructor, domain.StatusConstructor), true, "UNARCHIVED", true, true); got != "unarchive" {
		t.Errorf("unarchive restores the children it archived, got %q", got)
	}
	// A child table with no DeletedAt takes no cascade at all.
	if got := childOpName(domain.OperationOf(domain.StatusConstructor, domain.StatusConstructor), true, "ARCHIVED", false, false); got != "noop" {
		t.Errorf("a child without DeletedAt is skipped by the cascade, got %q", got)
	}
	// And neither does a row the cascade's predicate did not reach: already
	// archived when the root archived, or carrying a stamp that is not the one
	// being undone. The statement left it alone, so the event must too.
	if got := childOpName(domain.OperationOf(domain.StatusConstructor, domain.StatusConstructor), true, "ARCHIVED", true, false); got != "noop" {
		t.Errorf("an already-archived child is skipped by the archive cascade, got %q", got)
	}
	if got := childOpName(domain.OperationOf(domain.StatusConstructor, domain.StatusConstructor), true, "UNARCHIVED", true, false); got != "noop" {
		t.Errorf("a child archived on its own is skipped by the restore, got %q", got)
	}
}

func TestBuildDeletePayload_KeysGrowOnly(t *testing.T) {
	e := &roleTestEntity{Name: "Ana", Document: "D1", Matricula: "M1"}
	id := uuid.NewString()
	baseID := deterministicBaseID("D1")
	p := buildDeletePayload(roleTestSchema(), e, id, outboxMeta{ID: id, BaseID: baseID, BaseRevision: 3, BasePurged: true})

	// The historical structural keys survive untouched…
	if p["id"] != domain.NewID(id) || p["pessoa_id"] != domain.NewID(baseID) {
		t.Errorf("the structural keys must survive, got %v", p)
	}
	// …and the _ids block only ADDS.
	ids := p["_ids"].(map[string]any)
	if ids["base_purged"] != true || ids["base_revision"] != int64(3) {
		t.Errorf("_ids must carry the purge flag + revision, got %v", ids)
	}
}

// The warm shared-base UPDATE (upsertSharedBase's exact buildUpdate call) must
// bump the revision in the SAME statement — one row lock serializes concurrent
// role writes of the identity.
func TestSharedBaseUpdate_BumpsRevisionInOneStatement(t *testing.T) {
	base := NewSharedBaseSchema("pessoa").Revision("revision").ID("id").
		Field("Name", "name").Field("Document", "document").NaturalID("document")
	baseID := deterministicBaseID("D1")
	sql, args, err := buildUpdate(testPGDialect{}, schemaTarget(base), criteria.Eq("ID", domain.NewID(baseID)),
		domain.Fields{"name": "Ana"}, base.UpdateNowColumns(), testNow, base.RevisionColumn(), 0)
	if err != nil {
		t.Fatalf("buildUpdate: %v", err)
	}
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

// The sibling fields merge flat into the payload UNCONDITIONALLY — an all-nil
// facet emits explicit nulls (the removed-row marker): under event-carried
// state an absent key is indistinguishable from "untouched", so a PUT that
// cleared the sibling row would otherwise leave stale values on the projected
// document forever. The projector drops the keys when the whole group is null.
func TestBuildWritePayload_SiblingsMergeFlat(t *testing.T) {
	sib := NewSiblingSchema[*sibTestEntity]("usuario_login").Field("UserName", "user_name")
	schema := NewTableSchema[*sibTestEntity]("usuario").ID("id").Revision("revision").
		Field("Name", "name").Sibling(sib)
	un := "alice"
	e := &sibTestEntity{Name: "Ana", UserName: &un}
	p := buildWritePayload(schema, e, nil, "INSERTED", testNow, CascadeStamps{}, schema.WriteFields(e), outboxMeta{ID: "u1"})
	if got, _ := p["user_name"].(*string); got == nil || *got != "alice" {
		t.Errorf("sibling fields must merge flat, got %v", p)
	}
	// All-nil sibling → columns PRESENT with null values (removed-row marker).
	e2 := &sibTestEntity{Name: "Bo"}
	p2 := buildWritePayload(schema, e2, nil, "INSERTED", testNow, CascadeStamps{}, schema.WriteFields(e2), outboxMeta{ID: "u2"})
	v, has := p2["user_name"]
	if !has {
		t.Fatalf("an all-nil sibling must still emit its columns (explicit nulls), got %v", p2)
	}
	if s, isPtr := v.(*string); isPtr && s != nil {
		t.Errorf("cleared sibling column must be null, got %v", *s)
	}
}

// Shape #4 on the wire: a child's SIBLING fields merge FLAT into the child's
// payload item, exactly like the composed document renders them.
func TestBuildWritePayload_ChildSiblingFieldsFlat(t *testing.T) {
	child := NewTableSchema[bcAddr]("bc_addrs").ID("id").ParentID("root_id").
		Field("Street", "street").
		Sibling(NewSiblingSchema[bcAddr]("bc_addr_extras").Field("Street", "street_copy"))
	base := NewSharedBaseSchema("pessoa").Revision("revision").ID("id").
		Field("Name", "name").Field("Document", "document").NaturalID("document")
	schema := NewTableSchema[*bcRole]("aluno").ID("id").Revision("revision").
		Field("Matricula", "matricula").SharedBase(base, "pessoa_id").Child(child)
	e := &bcRole{Name: "Ana", Document: "D9", Matricula: "M9"}
	domain.AddAggregateChild(e, bcAddr{Street: "Main"})
	root, _ := any(e).(domain.AggregateRootProvider)

	p := buildWritePayload(schema, e, root.GetAggregateRoot(), "INSERTED", testNow, CascadeStamps{},
		schema.WriteFields(e), outboxMeta{ID: "r1", Revision: 1, BaseID: deterministicBaseID("D9"), BaseRevision: 1})
	items := p["_children"].(map[string]any)["bcAddr"].([]map[string]any)
	if items[0]["street_copy"] != "Main" {
		t.Fatalf("child item must carry the child-sibling field flat, got %v", items[0])
	}
}

// The physical revision column must NEVER leak into the payload's scalars —
// buildInsert appends it to the statement without mutating the caller's map
// (which becomes the outbox payload and WriteResult.Fields).
func TestBuildInsert_DoesNotLeakRevisionIntoFields(t *testing.T) {
	fields := domain.Fields{"name": "alice"}
	_, args := buildInsert(testPGDialect{}, "users", "id", "11111111-1111-1111-1111-111111111111",
		fields, nil, testNow, "revision")
	if _, leaked := fields["revision"]; leaked {
		t.Fatal("buildInsert mutated the caller's fields map — the payload would carry the physical revision column")
	}
	if args[len(args)-1] != int64(1) {
		t.Fatalf("the revision init must bind 1 as the last arg, got %v", args)
	}
}
