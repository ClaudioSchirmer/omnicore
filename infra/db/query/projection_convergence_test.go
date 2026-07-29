package query

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// The projection-convergence mechanisms: the base-revision registry handshake
// (a document born after a fan-out that missed it repairs itself), the
// document-tombstone handshake (a zombie upsert after DELETED removes itself)
// and the guarded consult/backfill writes (a late writer can no longer regress
// a fresher document).

// convRoleView builds the aluno role view with revisions on both role and base.
func convRoleView() *ViewDefinition {
	base := core.NewSharedBaseSchema("pessoa").Revision("revision").ID("id").
		Field("Name", "name").NaturalID("name")
	schema := core.NewTableSchema[*builderTestEntity]("aluno").
		ID("id").
		Revision("revision").
		Field("Email", "email").
		SharedBase(base, "pessoa_id")
	return View("aluno").Schema(schema).Version(1)
}

// convComposerRows scripts the relational reads of the aluno/pessoa closure.
func convComposerRows(sql string, _ []any) ([]map[string]any, error) {
	switch {
	case strings.Contains(sql, "FROM pessoa"):
		return mapsFromColsData([]string{"id", "name", "revision"}, [][]any{{"p1", "Ana", int64(5)}}), nil
	case strings.Contains(sql, "FROM aluno"):
		return mapsFromColsData([]string{"id", "email", "pessoa_id", "revision"}, [][]any{{"a1", "a@x", "p1", int64(2)}}), nil
	}
	return nil, nil
}

// A role event stamps the base revision into the registry (the push side of
// the handshake) — advance-only, keyed by base table + base id.
func TestProcess_RoleEvent_StampsBaseRevision(t *testing.T) {
	coll := &fakeColl{}
	store := newFakeMongo(coll)
	view := convRoleView()
	s := NewSyncEngine(composerEngine(convComposerRows), store, identityResolver, nil, "", []*ViewDefinition{view}, 1)

	event := kafkaEvent{
		AggregateType: "aluno", EventType: "UPDATED", AggregateID: "a1",
		Payload: []byte(`{"email":"a@x","name":"Ana","_ids":{"id":"a1","revision":2,"base_id":"p1","base_revision":3}}`),
	}
	if err := s.process(context.Background(), event); err != nil {
		t.Fatalf("process: %v", err)
	}
	if store.state == nil || len(store.state.updates) != 1 {
		t.Fatalf("the role event must stamp exactly one base-revision record, got %v", store.state)
	}
	u := store.state.updates[0]
	if u["_id"] != "base:pessoa:p1" {
		t.Errorf("base-revision record keyed %v, want base:pessoa:p1", u["_id"])
	}
	stages, _ := u["$pipeline"].([]Document)
	if len(stages) != 1 {
		t.Fatalf("base-revision stamp must be one advance-only stage, got %v", stages)
	}
	set, _ := stages[0]["$set"].(Document)
	if _, ok := set["base_revision"]; !ok {
		t.Errorf("base-revision stamp must write the base revision watermark, got %v", set)
	}
}

// PULL side of the handshake: an INSERTED role event whose payload carries an
// OLDER base revision than the registry's base revision proves a fan-out already
// passed without finding this document — the projection repairs by consult
// (one extra guarded upsert beyond the own-document projection). With the
// registry NOT ahead, no repair runs.
func TestProcess_RoleInsert_PullCheckHealsStaleBase(t *testing.T) {
	run := func(t *testing.T, registryRevision int64) (ownWrites int) {
		coll := &fakeColl{} // no pre-existing documents: the fan-out cannot see this one
		store := newFakeMongo(coll)
		if registryRevision > 0 {
			store.state = &fakeColl{docs: []any{map[string]any{"_id": "base:pessoa:p1", "base_revision": registryRevision}}}
		}
		view := convRoleView()
		s := NewSyncEngine(composerEngine(convComposerRows), store, identityResolver, nil, "", []*ViewDefinition{view}, 1)
		event := kafkaEvent{
			AggregateType: "aluno", EventType: "INSERTED", AggregateID: "a1",
			Payload: []byte(`{"email":"a@x","name":"Ana","_ids":{"id":"a1","revision":1,"base_id":"p1","base_revision":3}}`),
		}
		if err := s.process(context.Background(), event); err != nil {
			t.Fatalf("process: %v", err)
		}
		for _, u := range coll.updates {
			if up, ok := u["$upsert"].(bool); ok && up {
				ownWrites++
			}
		}
		return ownWrites
	}

	// Registry ahead (base_revision 5 > payload base_revision 3): projection + repair.
	if got := run(t, 5); got != 2 {
		t.Errorf("with the registry ahead the insert must project AND repair, got %d upserting writes", got)
	}
	// Registry current (base_revision 3 == payload's 3): projection only.
	if got := run(t, 3); got != 1 {
		t.Errorf("with the registry current no repair must run, got %d upserting writes", got)
	}
	// No registry record: projection only.
	if got := run(t, 0); got != 1 {
		t.Errorf("with no registry record no repair must run, got %d upserting writes", got)
	}
}

// The rebirth discriminator: a DELETED carrying the dead row's created_at
// scopes the tombstone AND the guarded delete to that incarnation, and the
// creator-side check hands the tombstone's created_at to its self-remove — so
// a deterministic id re-created under the same natural key is never killed by
// the old life's tombstone.
func TestProcess_Deleted_CreatedAtScopesTombstone(t *testing.T) {
	coll := &fakeColl{}
	store := newFakeMongo(coll)
	view := convRoleView()
	s := NewSyncEngine(composerEngine(func(string, []any) ([]map[string]any, error) { return nil, nil }),
		store, identityResolver, nil, "", []*ViewDefinition{view}, 1)
	event := kafkaEvent{
		AggregateType: "aluno", EventType: "DELETED", AggregateID: "a1",
		Payload: []byte(`{"id":"a1","pessoa_id":"p1","_ids":{"id":"a1","revision":7,"base_id":"p1","base_revision":9,"created_at":"2026-07-20T10:00:00.000123Z"}}`),
	}
	if err := s.process(context.Background(), event); err != nil {
		t.Fatalf("process: %v", err)
	}
	wantMs := int64(1784541600000) // 2026-07-20T10:00:00Z in unix millis
	if len(coll.guardedDeletes) != 1 || coll.guardedDeletes[0]["created_at"] != wantMs {
		t.Fatalf("the guarded delete must carry the dead incarnation's created_at (%d), got %v", wantMs, coll.guardedDeletes)
	}
	var stamped bool
	for _, u := range store.state.updates {
		if u["_id"] != "doc:aluno:a1" {
			continue
		}
		stages, _ := u["$pipeline"].([]Document)
		set, _ := stages[0]["$set"].(Document)
		if _, ok := set["created_at"]; ok {
			stamped = true
		}
	}
	if !stamped {
		t.Error("the tombstone must record the dead incarnation's created_at")
	}

	// Creator side: a later write finds the tombstone {revision 7, created_at}
	// and its self-remove is scoped to the OLD incarnation.
	coll2 := &fakeColl{}
	store2 := newFakeMongo(coll2)
	store2.state = &fakeColl{docs: []any{map[string]any{
		"_id": "doc:aluno:a1", "revision": int64(7), "created_at": wantMs,
	}}}
	s2 := NewSyncEngine(composerEngine(convComposerRows), store2, identityResolver, nil, "", []*ViewDefinition{view}, 1)
	reborn := kafkaEvent{
		AggregateType: "aluno", EventType: "INSERTED", AggregateID: "a1",
		Payload: []byte(`{"email":"a@x","name":"Ana","_ids":{"id":"a1","revision":1,"base_id":"p1","base_revision":10}}`),
	}
	if err := s2.process(context.Background(), reborn); err != nil {
		t.Fatalf("process reborn: %v", err)
	}
	if len(coll2.guardedDeletes) == 0 || coll2.guardedDeletes[0]["created_at"] != wantMs {
		t.Fatalf("the creator check must scope its self-remove to the tombstone's created_at, got %v", coll2.guardedDeletes)
	}
}

// The DELETED path records the tombstone BEFORE the guarded delete, and the
// delete carries the row's last revision.
func TestProcess_Deleted_TombstonesAndGuardsDelete(t *testing.T) {
	coll := &fakeColl{}
	store := newFakeMongo(coll)
	view := convRoleView()
	s := NewSyncEngine(composerEngine(func(string, []any) ([]map[string]any, error) { return nil, nil }),
		store, identityResolver, nil, "", []*ViewDefinition{view}, 1)

	event := kafkaEvent{
		AggregateType: "aluno", EventType: "DELETED", AggregateID: "a1",
		Payload: []byte(`{"id":"a1","pessoa_id":"p1","_ids":{"id":"a1","revision":7,"base_id":"p1","base_revision":9}}`),
	}
	if err := s.process(context.Background(), event); err != nil {
		t.Fatalf("process: %v", err)
	}
	// Tombstone recorded in the registry.
	var tombstoned bool
	if store.state != nil {
		for _, u := range store.state.updates {
			if u["_id"] == "doc:aluno:a1" {
				tombstoned = true
			}
		}
	}
	if !tombstoned {
		t.Error("DELETED must record the document tombstone in the registry")
	}
	// The document delete is guarded by the last revision.
	if len(coll.guardedDeletes) != 1 {
		t.Fatalf("DELETED must delete guarded, got %v (plain deletes: %v)", coll.guardedDeletes, coll.deletes)
	}
	if coll.guardedDeletes[0]["revision"] != int64(7) {
		t.Errorf("the guarded delete must carry the row's last revision 7, got %v", coll.guardedDeletes[0])
	}
}

// The creator side of the tombstone handshake: an UPDATED projection racing a
// DELETED that already landed (tombstone rev 6 > event rev 5) removes its own
// write — guarded by the tombstone's revision.
func TestProcess_ZombieUpsertAfterDelete_SelfRemoves(t *testing.T) {
	coll := &fakeColl{}
	store := newFakeMongo(coll)
	store.state = &fakeColl{docs: []any{map[string]any{"_id": "doc:aluno:a1", "revision": int64(6)}}}
	view := convRoleView()
	s := NewSyncEngine(composerEngine(convComposerRows), store, identityResolver, nil, "", []*ViewDefinition{view}, 1)

	event := kafkaEvent{
		AggregateType: "aluno", EventType: "UPDATED", AggregateID: "a1",
		Payload: []byte(`{"email":"a@x","name":"Ana","_ids":{"id":"a1","revision":5,"base_id":"p1","base_revision":3}}`),
	}
	if err := s.process(context.Background(), event); err != nil {
		t.Fatalf("process: %v", err)
	}
	if len(coll.guardedDeletes) == 0 {
		t.Fatal("a zombie upsert behind a newer tombstone must remove its own write")
	}
	if coll.guardedDeletes[0]["revision"] != int64(6) {
		t.Errorf("the self-removal must be guarded by the tombstone revision 6, got %v", coll.guardedDeletes[0])
	}
	// A FRESHER event (rev 8 > tombstone 6) must NOT self-remove. (Unreachable
	// for a hard delete — the row is gone — but the guard's contract is
	// direction, not reachability.)
	coll2 := &fakeColl{}
	store2 := newFakeMongo(coll2)
	store2.state = &fakeColl{docs: []any{map[string]any{"_id": "doc:aluno:a1", "revision": int64(6)}}}
	s2 := NewSyncEngine(composerEngine(convComposerRows), store2, identityResolver, nil, "", []*ViewDefinition{view}, 1)
	event.Payload = []byte(`{"email":"a@x","name":"Ana","_ids":{"id":"a1","revision":8,"base_id":"p1","base_revision":3}}`)
	if err := s2.process(context.Background(), event); err != nil {
		t.Fatalf("process: %v", err)
	}
	if len(coll2.guardedDeletes) != 0 {
		t.Errorf("an event fresher than the tombstone must not self-remove, got %v", coll2.guardedDeletes)
	}
}

// consultGuardedStages: the guarded write form — own scope behind _revision,
// shared-base scope behind _base_revision, embeds only on document creation,
// watermarks advance-only. A document without watermarks falls back to the
// legacy unguarded $set per scope.
func TestConsultGuardedStages_ScopesAndGuards(t *testing.T) {
	view := convRoleView()
	doc := Document{
		"id": "a1", "email": "a@x", "pessoa_id": "p1",
		"name":               "Ana", // shared-base business column
		docRevisionField:     int64(2),
		docBaseRevisionField: int64(5),
	}
	stages := consultGuardedStages(view, doc)
	if len(stages) != 2 {
		t.Fatalf("expected own + base stages, got %d: %v", len(stages), stages)
	}
	// Own scope: guarded by _revision 2, carries email + id, advances the watermark.
	ownSet, _ := stages[0]["$set"].(Document)
	if _, ok := ownSet["email"]; !ok {
		t.Errorf("own stage must carry the role's own fields, got %v", ownSet)
	}
	if _, ok := ownSet["name"]; ok {
		t.Errorf("base fields must not ride the own scope, got %v", ownSet)
	}
	wm, _ := ownSet[docRevisionField].(Document)
	if _, ok := wm["$cond"]; !ok {
		t.Errorf("the own watermark must advance monotonically ($cond), got %v", ownSet[docRevisionField])
	}
	guard, _ := ownSet["email"].(Document)
	if _, ok := guard["$cond"]; !ok {
		t.Errorf("own fields must be guard-wrapped, got %v", ownSet["email"])
	}
	// Base scope: guarded by _base_revision 5, carries name.
	baseSet, _ := stages[1]["$set"].(Document)
	if _, ok := baseSet["name"]; !ok {
		t.Errorf("base stage must carry the shared fields, got %v", baseSet)
	}
	if _, ok := baseSet[docBaseRevisionField]; !ok {
		t.Errorf("base stage must advance _base_revision, got %v", baseSet)
	}

	// Watermark-less document (defensive legacy): plain $set, no guards.
	legacy := consultGuardedStages(view, Document{"id": "a1", "email": "a@x"})
	if len(legacy) != 1 {
		t.Fatalf("expected one legacy stage, got %v", legacy)
	}
	set, _ := legacy[0]["$set"].(Document)
	if _, ok := set["email"].(Document); ok {
		if _, cond := set["email"].(Document)["$cond"]; cond {
			t.Errorf("a watermark-less scope must write unguarded, got %v", set["email"])
		}
	}
}

// A SharedBaseView document is ONE revision-guarded scope: every non-embed field
// (base scalars, segments) rides the _revision guard — the base revision the
// composer stamped from the base row.
func TestConsultGuardedStages_SharedBaseViewSingleScope(t *testing.T) {
	base := core.NewSharedBaseSchema("sbv_persons").Revision("revision").ID("id").
		Field("Name", "name").NaturalID("name")
	role := core.NewTableSchema[*builderTestEntity]("sbv_users").
		ID("id").Revision("revision").Field("Email", "email").SharedBase(base, "id")
	view := SharedBaseView("persons").Schema(base).Role(role).Version(1)
	doc := Document{
		"id": "p1", "name": "Ana",
		"sbvUser":        Document{"id": "p1", "email": "a@x"},
		docRevisionField: int64(9),
	}
	stages := consultGuardedStages(view, doc)
	if len(stages) != 1 {
		t.Fatalf("expected one revision-guarded stage, got %d: %v", len(stages), stages)
	}
	set, _ := stages[0]["$set"].(Document)
	for _, k := range []string{"name", "sbvUser"} {
		g, _ := set[k].(Document)
		if _, ok := g["$cond"]; !ok {
			t.Errorf("field %q must ride the base-revision guard, got %v", k, set[k])
		}
	}
}

// The consult scope stage FILLS MISSING FIELDS AT THE EQUAL REVISION — the
// rolling-deploy closure: a pod on the previous binary projects an event
// without the columns its schema does not know, leaving the document at the
// current revision missing exactly those fields; a later consult of the SAME
// revision (the pull repair, a recompose) must add them without being able to
// overwrite anything present.
func TestConsultScopeStage_EqualRevisionFillsMissing(t *testing.T) {
	emptyShape := scopeShape{arraySegs: map[string]string{}, objectSegs: map[string]bool{}}
	st := scopeStage(docRevisionField, Document{"nickname": "rs-nick-value"}, 2, emptyShape)
	set, _ := st["$set"].(Document)
	cond, _ := set["nickname"].(Document)
	branches, _ := cond["$cond"].([]any)
	if len(branches) != 3 {
		t.Fatalf("field must be a 3-branch $cond, got %v", cond)
	}
	// Outer: newer → apply fresh.
	if _, ok := branches[0].(Document)["$lt"]; !ok {
		t.Fatalf("outer condition must be the strictly-newer guard, got %v", branches[0])
	}
	// Inner: equal → fill-if-missing; otherwise keep stored.
	inner, _ := branches[2].(Document)["$cond"].([]any)
	if len(inner) != 3 {
		t.Fatalf("the not-newer branch must be the equal-revision $cond, got %v", branches[2])
	}
	if _, ok := inner[0].(Document)["$eq"]; !ok {
		t.Errorf("the inner condition must be watermark equality, got %v", inner[0])
	}
	fill, _ := inner[1].(Document)["$cond"].([]any)
	if len(fill) != 3 || fill[2] != "$nickname" {
		t.Errorf("at the equal revision a present field must keep the stored value, got %v", inner[1])
	}
	if inner[2] != "$nickname" {
		t.Errorf("below the watermark the stored value must stand untouched, got %v", inner[2])
	}
	// And the watermark itself still only ADVANCES on strictly newer.
	wm, _ := set[docRevisionField].(Document)
	wmBranches, _ := wm["$cond"].([]any)
	if lt, ok := wmBranches[0].(Document)["$lt"]; !ok || lt == nil {
		t.Errorf("watermark advance must stay strictly-newer-gated, got %v", wm)
	}
}

// Structured fill: a child-collection segment merges PER ELEMENT (keyed by the
// child ID, stored keys winning) and a SharedBaseView role segment merges as a
// sub-document — so a column added anywhere (root, sibling, base, child
// element, role-segment scalar) reaches a document written by a
// previous-binary pod at the same revision.
func TestConsultScopeStage_StructuredFillShapes(t *testing.T) {
	shape := scopeShape{
		arraySegs:  map[string]string{"addresses": "id"},
		objectSegs: map[string]bool{"sbvUser": true},
	}
	st := scopeStage(docRevisionField, Document{
		"addresses": []Document{{"id": "a1", "city": "POA"}},
		"sbvUser":   Document{"id": "u1", "nickname": "nn"},
	}, 3, shape)
	set, _ := st["$set"].(Document)

	// Array segment: the equal branch must $map the STORED array and
	// $mergeObjects with the ID-matched composed element (stored last = wins).
	arrCond, _ := set["addresses"].(Document)["$cond"].([]any)
	arrEqual, _ := arrCond[2].(Document)["$cond"].([]any)
	arrFill, _ := arrEqual[1].(Document)["$cond"].([]any)
	mapExpr, ok := arrFill[1].(Document)["$map"].(Document)
	if !ok {
		t.Fatalf("array segment equal-revision fill must $map the stored array, got %v", arrFill[1])
	}
	if mapExpr["input"] != "$addresses" {
		t.Errorf("the $map input must be the STORED array, got %v", mapExpr["input"])
	}
	merge, _ := mapExpr["in"].(Document)["$mergeObjects"].([]any)
	if len(merge) != 2 || merge[1] != "$$stored" {
		t.Errorf("element merge must keep stored keys winning (stored LAST), got %v", mapExpr["in"])
	}

	// Object segment: the equal branch must $mergeObjects composed-then-stored.
	objCond, _ := set["sbvUser"].(Document)["$cond"].([]any)
	objEqual, _ := objCond[2].(Document)["$cond"].([]any)
	objFill, _ := objEqual[1].(Document)["$cond"].([]any)
	objMerge, _ := objFill[1].(Document)["$mergeObjects"].([]any)
	if len(objMerge) != 2 || objMerge[1] != "$sbvUser" {
		t.Errorf("segment merge must keep the stored sub-document winning (stored LAST), got %v", objFill[1])
	}
}

// The backfill writes guarded pipelines through BulkApplyProjection — never a
// plain $set batch (which could regress fresher dual-applied shadow writes).
func TestBackfill_WritesGuardedPipelines(t *testing.T) {
	view := convRoleView()
	coll := &fakeColl{}
	store := newFakeMongo(coll)
	eng := newScriptEngine([]string{"a1"}, func(id string) map[string]any {
		return map[string]any{"id": id, "email": "a@x", "pessoa_id": "p1", "revision": int64(2)}
	})
	s := scriptSyncEngine(eng, store, []*ViewDefinition{view})
	if err := s.backfillInto(context.Background(), view, pc("shadow"), "", 1, 10, nil); err != nil {
		t.Fatalf("backfillInto: %v", err)
	}
	if len(coll.updates) == 0 {
		t.Fatal("backfill wrote nothing")
	}
	for _, u := range coll.updates {
		if _, ok := u["$pipeline"]; !ok {
			t.Errorf("backfill writes must be pipeline updates, got %v", u)
		}
	}
}

// The base purge (a base-table DELETED with base_purged) removes the
// identity's base-revision record from the registry.
func TestProcess_BasePurge_DropsRegistryRecord(t *testing.T) {
	coll := &fakeColl{}
	store := newFakeMongo(coll)
	view := convRoleView()
	s := NewSyncEngine(composerEngine(func(string, []any) ([]map[string]any, error) { return nil, nil }),
		store, identityResolver, nil, "", []*ViewDefinition{view}, 1)

	event := kafkaEvent{
		AggregateType: "pessoa", EventType: "DELETED", AggregateID: "p1",
		Payload: []byte(`{"id":"p1","_ids":{"id":"p1","base_purged":true}}`),
	}
	if err := s.process(context.Background(), event); err != nil {
		t.Fatalf("process: %v", err)
	}
	if store.state == nil || len(store.state.deletes) != 1 || store.state.deletes[0] != "base:pessoa:p1" {
		t.Fatalf("the purge must drop the identity's registry record, got %v", store.state)
	}
	// A non-purge base event (the backlog UPDATED) must NOT drop it.
	coll2 := &fakeColl{}
	store2 := newFakeMongo(coll2)
	s2 := NewSyncEngine(composerEngine(func(string, []any) ([]map[string]any, error) { return nil, nil }),
		store2, identityResolver, nil, "", []*ViewDefinition{view}, 1)
	if err := s2.process(context.Background(), kafkaEvent{
		AggregateType: "pessoa", EventType: "UPDATED", AggregateID: "p1",
		Payload: []byte(`{"_ids":{"id":"p1"}}`),
	}); err != nil {
		t.Fatalf("process: %v", err)
	}
	if store2.state != nil && len(store2.state.deletes) != 0 {
		t.Errorf("a non-purge base event must keep the registry record, got %v", store2.state.deletes)
	}
}

// UNARCHIVED re-materializes a document exactly like INSERTED (under
// DeleteOnArchive the archive removed it) — the pull check must cover it too.
func TestProcess_RoleUnarchive_PullCheckHealsStaleBase(t *testing.T) {
	coll := &fakeColl{}
	store := newFakeMongo(coll)
	store.state = &fakeColl{docs: []any{map[string]any{"_id": "base:pessoa:p1", "base_revision": int64(9)}}}
	view := convRoleView()
	s := NewSyncEngine(composerEngine(convComposerRows), store, identityResolver, nil, "", []*ViewDefinition{view}, 1)

	event := kafkaEvent{
		AggregateType: "aluno", EventType: "UNARCHIVED", AggregateID: "a1",
		Payload: []byte(`{"email":"a@x","name":"Ana","_ids":{"id":"a1","revision":4,"base_id":"p1","base_revision":6}}`),
	}
	if err := s.process(context.Background(), event); err != nil {
		t.Fatalf("process: %v", err)
	}
	var upserts int
	for _, u := range coll.updates {
		if up, ok := u["$upsert"].(bool); ok && up {
			upserts++
		}
	}
	if upserts != 2 {
		t.Errorf("UNARCHIVED behind the registry must project AND repair, got %d upserting writes", upserts)
	}
}

// ARCHIVED under DeleteOnArchive removes the document — with the SAME
// tombstone discipline as DELETED: the event's revision is recorded and the
// delete is guarded by it.
func TestProcess_DeleteOnArchive_TombstonesAndGuardsDelete(t *testing.T) {
	coll := &fakeColl{}
	store := newFakeMongo(coll)
	base := core.NewSharedBaseSchema("pessoa").Revision("revision").ID("id").
		Field("Name", "name").NaturalID("name")
	schema := core.NewTableSchema[*builderTestEntity]("aluno").
		ID("id").Revision("revision").Field("Email", "email").
		SoftDelete("deleted_at").SharedBase(base, "pessoa_id")
	view := View("aluno_hot").Schema(schema).Version(1).DeleteOnArchive()
	s := NewSyncEngine(composerEngine(func(string, []any) ([]map[string]any, error) { return nil, nil }),
		store, identityResolver, nil, "", []*ViewDefinition{view}, 1)

	event := kafkaEvent{
		AggregateType: "aluno", EventType: "ARCHIVED", AggregateID: "a1",
		Payload: []byte(`{"email":"a@x","_ids":{"id":"a1","revision":4,"base_id":"p1","base_revision":6}}`),
	}
	if err := s.process(context.Background(), event); err != nil {
		t.Fatalf("process: %v", err)
	}
	if len(coll.guardedDeletes) != 1 || coll.guardedDeletes[0]["revision"] != int64(4) {
		t.Fatalf("DeleteOnArchive must delete guarded by the event revision, got %v", coll.guardedDeletes)
	}
	var tombstoned bool
	if store.state != nil {
		for _, u := range store.state.updates {
			if u["_id"] == "doc:aluno_hot:a1" {
				tombstoned = true
			}
		}
	}
	if !tombstoned {
		t.Error("DeleteOnArchive must record the document tombstone")
	}
}

// A consult view WITH embeds: the guarded pipeline must keep the ripple's
// ownership — the embed segment rides an existence-probed create-only stage,
// placed FIRST (before the own stage writes the ID the probe reads), and never
// enters the revision-guarded scopes.
func TestConsultGuardedStages_EmbedsCreateOnlyAndFirst(t *testing.T) {
	external := JoinUpstream(core.NewExternalSchema("buyers").ID("id"), "Buyers", "buyers")
	schema := core.NewTableSchema[*builderTestEntity]("orders").
		ID("id").Revision("revision").Field("Email", "email")
	view := View("orders").Version(1).Schema(schema).EmbedMany(external).On("order_id")

	doc := Document{
		"id": "o1", "email": "a@x",
		"buyers":         []Document{{"_id": "b1"}},
		docRevisionField: int64(3),
	}
	stages := consultGuardedStages(view, doc)
	if len(stages) != 2 {
		t.Fatalf("expected embed stage + own stage, got %d: %v", len(stages), stages)
	}
	// Embed stage FIRST: create-only via the ID existence probe.
	embedSet, _ := stages[0]["$set"].(Document)
	cond, _ := embedSet["buyers"].(Document)["$cond"].([]any)
	if len(cond) != 3 || cond[1] != "$buyers" {
		t.Fatalf("the embed segment must keep the stored value on an existing document, got %v", embedSet["buyers"])
	}
	if _, ok := cond[0].(Document)["$ne"]; !ok {
		t.Errorf("the embed stage must probe document existence, got %v", cond[0])
	}
	// Own stage second, embeds excluded from it.
	ownSet, _ := stages[1]["$set"].(Document)
	if _, leaked := ownSet["buyers"]; leaked {
		t.Errorf("the embed segment must not enter the revision-guarded scope, got %v", ownSet)
	}
	if _, ok := ownSet["email"]; !ok {
		t.Errorf("the own scope must carry the root fields, got %v", ownSet)
	}
}

// A redelivered DELETED re-stamps the tombstone with the advance-only form:
// the revision watermark rides a $cond (never regresses) and the TTL stamp
// "at" is set only when the watermark advances — a replay keeps the original
// expiry window.
func TestStampTombstone_RedeliveryKeepsAdvanceOnlyForm(t *testing.T) {
	coll := &fakeColl{}
	store := newFakeMongo(coll)
	view := convRoleView()
	s := NewSyncEngine(composerEngine(func(string, []any) ([]map[string]any, error) { return nil, nil }),
		store, identityResolver, nil, "", []*ViewDefinition{view}, 1)
	event := kafkaEvent{
		AggregateType: "aluno", EventType: "DELETED", AggregateID: "a1",
		Payload: []byte(`{"id":"a1","pessoa_id":"p1","_ids":{"id":"a1","revision":7,"base_id":"p1","base_revision":9}}`),
	}
	for i := 0; i < 2; i++ { // original + redelivery
		if err := s.process(context.Background(), event); err != nil {
			t.Fatalf("process #%d: %v", i, err)
		}
	}
	var stamps int
	for _, u := range store.state.updates {
		if u["_id"] != "doc:aluno:a1" {
			continue
		}
		stamps++
		stages, _ := u["$pipeline"].([]Document)
		set, _ := stages[0]["$set"].(Document)
		wm, _ := set["revision"].(Document)
		if _, ok := wm["$cond"]; !ok {
			t.Errorf("the tombstone revision must be advance-only ($cond), got %v", set["revision"])
		}
		at, _ := set["at"].(Document)
		if _, ok := at["$cond"]; !ok {
			t.Errorf("the TTL stamp must be written only when the watermark advances, got %v", set["at"])
		}
	}
	if stamps != 2 {
		t.Errorf("both deliveries must go through the same advance-only stamp, got %d", stamps)
	}
}

// The tombstone discipline reaches the SHADOW slot during a rebuild: the
// guarded delete dual-applies with the same revision.
func TestApplyDelete_Guarded_DualAppliesToShadow(t *testing.T) {
	mongo, colls := bothSlotsMongo("v", "v__0")
	resolver, eng := shadowResolver(t, "v__0")
	s := &SyncEngine{eng: eng, mongo: mongo, resolver: resolver}
	if err := s.applyDelete(context.Background(), "v", "id1", 5, 0); err != nil {
		t.Fatalf("applyDelete: %v", err)
	}
	if len(colls["v"].guardedDeletes) != 1 || len(colls["v__0"].guardedDeletes) != 1 {
		t.Errorf("guarded delete must reach active AND shadow, got active=%v shadow=%v",
			colls["v"].guardedDeletes, colls["v__0"].guardedDeletes)
	}
	for _, c := range []*fakeColl{colls["v"], colls["v__0"]} {
		if len(c.guardedDeletes) == 1 && c.guardedDeletes[0]["revision"] != int64(5) {
			t.Errorf("both slots must be guarded by the event revision 5, got %v", c.guardedDeletes[0])
		}
	}
}

// watermarkOf tolerates every numeric form a decode path can hand it; an
// unknown form degrades to 0 (the legacy unguarded scope), never a panic.
func TestWatermarkOf_NumericForms(t *testing.T) {
	cases := []struct {
		in   any
		want int64
	}{
		{int64(7), 7}, {int32(6), 6}, {int(5), 5}, {float64(4), 4},
		{json.Number("3"), 3}, {"x", 0}, {nil, 0},
	}
	for _, c := range cases {
		if got := watermarkOf(c.in); got != c.want {
			t.Errorf("watermarkOf(%v) = %d, want %d", c.in, got, c.want)
		}
	}
}

// A registry read failure on the tombstone check fails the event — the
// at-least-once redelivery retries; silence would let a zombie write survive
// unchecked.
func TestProcess_TombstoneCheckReadError_FailsEvent(t *testing.T) {
	coll := &fakeColl{}
	store := newFakeMongo(coll)
	store.state = &fakeColl{findErr: errFake}
	view := convRoleView()
	s := NewSyncEngine(composerEngine(convComposerRows), store, identityResolver, nil, "", []*ViewDefinition{view}, 1)
	event := kafkaEvent{
		AggregateType: "aluno", EventType: "UPDATED", AggregateID: "a1",
		Payload: []byte(`{"email":"a@x","name":"Ana","_ids":{"id":"a1","revision":2,"base_id":"p1","base_revision":3}}`),
	}
	if err := s.process(context.Background(), event); err == nil {
		t.Fatal("a tombstone-check read failure must fail the event for redelivery")
	}
}

// A registry write failure on the base-revision stamp fails the event AND
// blocks the fan-out probe — the handshake's premise (stamp precedes probe)
// must never be silently skipped.
//
// It does NOT block the event's own document. That is the deliberate change:
// the two are independent obligations over different documents, and the writer's
// own projection is the one thing only this event can supply. Blocking it bought
// nothing once the event is genuinely retried — the retry re-runs the stamp, the
// probe, and (idempotently) the projection.
func TestProcess_BaseRevisionStampError_BlocksProbeNotOwnWrite(t *testing.T) {
	coll := &fakeColl{docs: []any{map[string]any{"_id": "a1"}}}
	store := newFakeMongo(coll)
	store.state = &fakeColl{updateErr: errFake}
	view := convRoleView()
	s := NewSyncEngine(composerEngine(convComposerRows), store, identityResolver, nil, "", []*ViewDefinition{view}, 1)
	event := kafkaEvent{
		AggregateType: "aluno", EventType: "UPDATED", AggregateID: "a1",
		Payload: []byte(`{"email":"a@x","name":"Ana","_ids":{"id":"a1","revision":2,"base_id":"p1","base_revision":3}}`),
	}
	if err := s.process(context.Background(), event); err == nil {
		t.Fatal("a base-revision stamp failure must fail the event")
	}
	// THE PREMISE: the probe reads target documents on the assumption that the
	// stamp already landed. A failed stamp dissolves that, so the probe must not
	// run — a late-born document it missed would have no stamp to repair from.
	if coll.idFinds != 0 {
		t.Errorf("the fan-out probe must not run after a failed stamp, FindIDsByField calls=%d", coll.idFinds)
	}
	// THE ISOLATION: the writer's own document is still projected.
	if len(coll.updates) != 1 {
		t.Errorf("the event's own document must still be projected, updates=%v", coll.updates)
	}
}

// The pull check's registry read failure fails the event too (redelivery
// re-runs the whole handshake).
func TestProcess_PullCheckReadError_FailsEvent(t *testing.T) {
	coll := &fakeColl{}
	store := newFakeMongo(coll)
	// The stamp write must succeed but the read must fail: script per-call.
	store.state = &fakeColl{findErr: errFake}
	view := convRoleView()
	s := NewSyncEngine(composerEngine(convComposerRows), store, identityResolver, nil, "", []*ViewDefinition{view}, 1)
	event := kafkaEvent{
		AggregateType: "aluno", EventType: "INSERTED", AggregateID: "a1",
		Payload: []byte(`{"email":"a@x","name":"Ana","_ids":{"id":"a1","revision":1,"base_id":"p1","base_revision":3}}`),
	}
	if err := s.process(context.Background(), event); err == nil {
		t.Fatal("a pull-check read failure must fail the event")
	}
}

// stampTombstone / stampBaseRevision skip a value the write side never
// produces (rev <= 0) — the defensive no-op, direct.
func TestRegistryStamps_SkipNonPositiveRevision(t *testing.T) {
	coll := &fakeColl{}
	store := newFakeMongo(coll)
	s := &SyncEngine{mongo: store, resolver: identityResolver}
	if err := s.stampTombstone(context.Background(), "v", "id", 0, 0); err != nil {
		t.Fatalf("stampTombstone(0): %v", err)
	}
	if err := s.stampBaseRevision(context.Background(), "t", "id", 0); err != nil {
		t.Fatalf("stampBaseRevision(0): %v", err)
	}
	if store.state != nil && len(store.state.updates) != 0 {
		t.Errorf("non-positive revisions must not touch the registry, got %v", store.state.updates)
	}
}

// consultGuardedStages with CHILD COLLECTIONS on both scopes: own children
// ride the own scope's element-merge shape, base children the base scope's —
// the arraySegs wiring a childless fixture never exercises.
func TestConsultGuardedStages_ChildSegmentsInBothScopes(t *testing.T) {
	base := core.NewSharedBaseSchema("pessoa").Revision("revision").ID("id").
		Field("Name", "name").NaturalID("name").
		Child(core.NewTableSchema[*builderTestEntity]("enderecos").ID("id").ParentID("pessoa_id").Field("Email", "city"))
	schema := core.NewTableSchema[*builderTestEntity]("aluno").
		ID("id").Revision("revision").Field("Email", "email").
		SharedBase(base, "pessoa_id")
	view := View("aluno").Schema(schema).Version(1)

	doc := Document{
		"id": "a1", "email": "a@x", "name": "Ana",
		"builderTestEntities": []Document{{"id": "e1", "city": "POA"}},
		docRevisionField:      int64(2),
		docBaseRevisionField:  int64(5),
	}
	stages := consultGuardedStages(view, doc)
	if len(stages) != 2 {
		t.Fatalf("want own + base stages, got %d: %v", len(stages), stages)
	}
	baseSet, _ := stages[1]["$set"].(Document)
	segCond, _ := baseSet["builderTestEntities"].(Document)["$cond"].([]any)
	if len(segCond) != 3 {
		t.Fatalf("base-children segment must be guard-wrapped, got %v", baseSet["builderTestEntities"])
	}
	equalBranch, _ := segCond[2].(Document)["$cond"].([]any)
	fillBranch, _ := equalBranch[1].(Document)["$cond"].([]any)
	if _, ok := fillBranch[1].(Document)["$map"]; !ok {
		t.Errorf("base-children equal-revision fill must be the per-element merge, got %v", equalBranch[1])
	}
}

// createdAtMillis: RFC 3339 parses to unix millis; empty and garbage degrade
// to 0 (revision-only tombstone fallback).
func TestPayloadIDs_CreatedAtMillis(t *testing.T) {
	if got := (payloadIDs{CreatedAt: "2026-07-20T10:00:00.000123Z"}).createdAtMillis(); got != 1784541600000 {
		t.Errorf("rfc3339 → %d, want 1784541600000", got)
	}
	if got := (payloadIDs{}).createdAtMillis(); got != 0 {
		t.Errorf("empty → %d, want 0", got)
	}
	if got := (payloadIDs{CreatedAt: "garbage"}).createdAtMillis(); got != 0 {
		t.Errorf("garbage → %d, want 0", got)
	}
}

// The tombstone self-remove dual-applies to the SHADOW slot during a rebuild —
// a zombie must not survive in the collection about to be flipped.
func TestCheckTombstone_SelfRemoveDualAppliesToShadow(t *testing.T) {
	mongo, colls := bothSlotsMongo("v", "v__0")
	mongo.state = &fakeColl{docs: []any{map[string]any{
		"_id": "doc:v:id1", "revision": int64(6), "created_at": int64(1784541600000),
	}}}
	resolver, eng := shadowResolver(t, "v__0")
	s := &SyncEngine{eng: eng, mongo: mongo, resolver: resolver}
	if err := s.checkTombstone(context.Background(), "v", "id1", 5); err != nil {
		t.Fatalf("checkTombstone: %v", err)
	}
	for _, slot := range []string{"v", "v__0"} {
		if len(colls[slot].guardedDeletes) != 1 {
			t.Errorf("slot %s: the self-remove must land, got %v", slot, colls[slot].guardedDeletes)
		} else if colls[slot].guardedDeletes[0]["created_at"] != int64(1784541600000) {
			t.Errorf("slot %s: the self-remove must carry the tombstone's created_at, got %v", slot, colls[slot].guardedDeletes[0])
		}
	}
}

// A registry write failure on the tombstone stamp fails the DELETED event —
// deleting the document without a durable tombstone would leave the zombie
// window open with no record to close it.
func TestApplyDelete_TombstoneStampErrorFailsEvent(t *testing.T) {
	coll := &fakeColl{}
	store := newFakeMongo(coll)
	store.state = &fakeColl{updateErr: errFake}
	s := &SyncEngine{mongo: store, resolver: identityResolver}
	if err := s.applyDelete(context.Background(), "v", "id1", 7, 0); err == nil {
		t.Fatal("a tombstone stamp failure must fail the delete for redelivery")
	}
	if len(coll.deletes) != 0 {
		t.Errorf("no document delete may run before the tombstone is durable, got %v", coll.deletes)
	}
}

// The PAYLOAD-DIRECT guard applies its FULL carried state at the equal
// revision — the rolling-deploy closure: a previous-binary pod's consult
// repair can advance the document TO the event's revision through its own
// (older) column list, leaving a column the old schema does not know exactly
// as an earlier event wrote it — null or stale-valued, not necessarily
// missing. A missing-only completion kept that stale value forever (the
// rebuild_scale nickname RED); one revision identifies exactly one committed
// state, so re-asserting every carried column at the equal revision is
// idempotent for redeliveries and can only replace stale-under-watermark
// values with that revision's truth. Columns the event does not carry are
// still never touched, and the watermark still advances on strictly-newer
// only.
func TestGuardedSetStage_EqualRevisionAppliesPayload(t *testing.T) {
	st := guardedSetStage(docRevisionField, Document{"nickname": lit("rs-nick-value")}, 2)
	set, _ := st["$set"].(Document)
	cond, _ := set["nickname"].(Document)["$cond"].([]any)
	if len(cond) != 3 || cond[2] != "$nickname" {
		t.Fatalf("field must keep the stored value outside the apply condition, got %v", set["nickname"])
	}
	apply, _ := cond[0].(Document)["$or"].([]any)
	if len(apply) != 2 {
		t.Fatalf("apply must be newer OR equal, got %v", cond[0])
	}
	if _, ok := apply[0].(Document)["$lt"]; !ok {
		t.Errorf("the first arm must be the strictly-newer $lt, got %v", apply[0])
	}
	if _, ok := apply[1].(Document)["$eq"]; !ok {
		t.Errorf("the second arm must be plain revision equality (no column-absence AND), got %v", apply[1])
	}
	// The watermark still advances ONLY on strictly newer.
	wm, _ := set[docRevisionField].(Document)["$cond"].([]any)
	if _, ok := wm[0].(Document)["$lt"]; !ok {
		t.Errorf("the watermark must stay strictly-newer-gated, got %v", set[docRevisionField])
	}
}
