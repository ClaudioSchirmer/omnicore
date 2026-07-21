package write

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/persistence"
	"github.com/ClaudioSchirmer/omnicore/domain"
)

// White-box coverage for the per-step error branches of the write verbs
// (aggregate + flat + SharedBase + Batch): every framework write is a fixed
// sequence of statements inside one TX, so each case injects a failure at one
// position (via recTx's substring injection or the hooks) and asserts the write
// errors out without committing. The happy paths and the real SQL remain the
// existing unit + integration suites' contract.

var errBoom = errors.New("boom")

var failAfterBegin = WriteHook{AfterBegin: func(persistence.RequestContext, domain.Entity, persistence.TxHandle) error {
	return errBoom
}}

var failBeforeCommit = WriteHook{BeforeCommit: func(persistence.RequestContext, domain.Entity, domain.ID, persistence.TxHandle) error {
	return errBoom
}}

// stepCase is one injection point in a verb's statement sequence.
type stepCase struct {
	name          string
	tx            *recTx
	beginErr      error
	hook          WriteHook
	wantCommitted bool
	wantCarrier   bool // the injected state maps to a NotificationCarrier, not a raw error
}

// verbSteps builds the shared begin/afterBegin/beforeCommit/commit cases every
// verb sequence starts and ends with; per-verb statement injections are appended
// by the caller.
func verbSteps(extra ...stepCase) []stepCase {
	base := []stepCase{
		{name: "begin", beginErr: errBoom, hook: firingHook},
		{name: "afterBegin", tx: &recTx{count: 1}, hook: failAfterBegin},
		{name: "beforeCommit", tx: &recTx{count: 1}, hook: failBeforeCommit},
		{name: "commit", tx: &recTx{count: 1, commitErr: errBoom}, hook: firingHook, wantCommitted: true},
	}
	return append(base, extra...)
}

func execStep(sub string) stepCase {
	return stepCase{name: "exec:" + sub, tx: &recTx{count: 1, execErrSub: sub}, hook: firingHook}
}

func runStepCases(t *testing.T, cases []stepCase, run func(be *BaseEngine, hook WriteHook) error) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			beginner := &recBeginner{tx: c.tx, beginErr: c.beginErr}
			be := newFlatBE(beginner)
			err := run(be, c.hook)
			if err == nil {
				t.Fatal("expected the injected failure to propagate")
			}
			if c.wantCarrier {
				if _, ok := errAsCarrier(err); !ok {
					t.Fatalf("expected a NotificationCarrier, got %T (%v)", err, err)
				}
			}
			if c.tx != nil && c.tx.committed != c.wantCommitted {
				t.Errorf("committed = %v, want %v", c.tx.committed, c.wantCommitted)
			}
		})
	}
}

func aggInsertableWithChild(t *testing.T) domain.Insertable {
	t.Helper()
	root := &aggWriteRoot{Name: "r"}
	domain.AddAggregateChild(root, aggWriteChild{Label: "a"})
	ins, err := domain.GetInsertable(root, nil, "GetInsertable")
	if err != nil {
		t.Fatalf("GetInsertable: %v", err)
	}
	return ins
}

func aggUpdatableAllOps(t *testing.T) domain.Updatable {
	t.Helper()
	id1, id2 := uuid.NewString(), uuid.NewString()
	root := &aggWriteRoot{Name: "r"}
	root.SetID(domain.NewID(uuid.NewString()))
	root.AggregateConstructor([]domain.AggregateValueObject{
		aggWriteChild{ID: domain.NewID(id1), Label: "keep"},
		aggWriteChild{ID: domain.NewID(id2), Label: "drop"},
	})
	upd, err := domain.GetUpdatable(root, func(r *aggWriteRoot) error {
		domain.ChangeAggregateChild(r, aggWriteChild{ID: domain.NewID(id1), Label: "keep"}, aggWriteChild{ID: domain.NewID(id1), Label: "changed"})
		domain.RemoveAggregateChild(r, aggWriteChild{ID: domain.NewID(id2), Label: "drop"})
		domain.AddAggregateChild(r, aggWriteChild{Label: "new"})
		return nil
	}, nil, "GetUpdatable")
	if err != nil {
		t.Fatalf("GetUpdatable: %v", err)
	}
	return upd
}

func TestInsertAggregate_StepFailures(t *testing.T) {
	cases := verbSteps(
		execStep("INSERT INTO agg_w ("),
		execStep("INSERT INTO agg_w_children"),
		execStep("INSERT INTO outbox"),
		execStep("INSERT INTO audit_events"),
	)
	runStepCases(t, cases, func(be *BaseEngine, hook WriteHook) error {
		_, err := be.Insert(newBuilderCtx(), aggInsertableWithChild(t), aggWriteSchema(), hook)
		return err
	})
}

func TestUpdateAggregate_StepFailures(t *testing.T) {
	cases := verbSteps(
		execStep("UPDATE agg_w SET"),
		execStep("UPDATE agg_w_children SET"),
		execStep("INSERT INTO agg_w_children"),
		execStep("INSERT INTO outbox"),
		execStep("INSERT INTO audit_events"),
		// zero matched rows on the root UPDATE → RecordNotFound carrier.
		stepCase{name: "rootNotFound", tx: &recTx{count: 0}, hook: firingHook, wantCarrier: true},
	)
	runStepCases(t, cases, func(be *BaseEngine, hook WriteHook) error {
		_, err := be.Update(newBuilderCtx(), aggUpdatableAllOps(t), aggWriteSchema(), hook)
		return err
	})
}

func TestSoftWriteAggregate_StepFailures(t *testing.T) {
	newArchivable := func(t *testing.T) domain.Archivable {
		t.Helper()
		root := &aggWriteRoot{Name: "r"}
		root.SetID(domain.NewID(uuid.NewString()))
		root.AggregateConstructor([]domain.AggregateValueObject{aggWriteChild{ID: domain.NewIDFromUUID(uuid.New()), Label: "c"}})
		a, err := domain.GetArchivable(root, nil, "GetArchivable")
		if err != nil {
			t.Fatalf("GetArchivable: %v", err)
		}
		return a
	}
	cases := verbSteps(
		execStep("UPDATE agg_w SET"),
		execStep("UPDATE agg_w_children SET"),
		execStep("INSERT INTO outbox"),
		execStep("INSERT INTO audit_events"),
	)
	runStepCases(t, cases, func(be *BaseEngine, hook WriteHook) error {
		return be.Archive(newBuilderCtx(), newArchivable(t), aggWriteSchema(), hook)
	})
}

// An aggregate root schema without SoftDelete cannot archive — the guard fires
// before the TX opens.
func TestSoftWriteAggregate_MissingSoftDeleteIsError(t *testing.T) {
	schema := NewTableSchema[*aggWriteRoot]("agg_w").
		PK("id").Field("Name", "name").
		Child(NewTableSchema[aggWriteChild]("agg_w_children").
			PK("id").FK("agg_w_id").Field("Label", "label").SoftDelete("deleted_at"))
	root := &aggWriteRoot{Name: "r"}
	root.SetID(domain.NewID(uuid.NewString()))
	a, _ := domain.GetArchivable(root, nil, "GetArchivable")
	tx := &recTx{}
	be := newFlatBE(&recBeginner{tx: tx})
	if err := be.Archive(newBuilderCtx(), a, schema, WriteHook{}); err == nil {
		t.Fatal("expected the missing-SoftDelete guard to error")
	}
	if len(tx.execs) != 0 {
		t.Errorf("no statement may run, got %v", tx.execs)
	}
}

// The cascade skips (a) loaded item types with no declared child schema and
// (b) declared children without a soft-delete column — root-only soft write.
func TestSoftWriteAggregate_CascadeSkips(t *testing.T) {
	t.Run("undeclaredChildType", func(t *testing.T) {
		schema := NewTableSchema[*aggWriteRoot]("agg_w").
			PK("id").Field("Name", "name").SoftDelete("deleted_at")
		root := &aggWriteRoot{Name: "r"}
		root.SetID(domain.NewID(uuid.NewString()))
		root.AggregateConstructor([]domain.AggregateValueObject{aggWriteChild{ID: domain.NewIDFromUUID(uuid.New()), Label: "c"}})
		a, _ := domain.GetArchivable(root, nil, "GetArchivable")
		tx := &recTx{}
		be := newFlatBE(&recBeginner{tx: tx})
		if err := be.Archive(newBuilderCtx(), a, schema, firingHook); err != nil {
			t.Fatalf("Archive: %v", err)
		}
		// root soft-write + outbox + audit only — the undeclared child type is skipped.
		if len(tx.execs) != 3 {
			t.Errorf("expected 3 statements, got %d: %v", len(tx.execs), tx.execs)
		}
	})
	t.Run("childWithoutSoftDelete", func(t *testing.T) {
		schema := NewTableSchema[*aggWriteRoot]("agg_w").
			PK("id").Field("Name", "name").SoftDelete("deleted_at").
			Child(NewTableSchema[aggWriteChild]("agg_w_children").
				PK("id").FK("agg_w_id").Field("Label", "label"))
		root := &aggWriteRoot{Name: "r"}
		root.SetID(domain.NewID(uuid.NewString()))
		root.AggregateConstructor([]domain.AggregateValueObject{aggWriteChild{ID: domain.NewIDFromUUID(uuid.New()), Label: "c"}})
		u, _ := domain.GetUnarchivable(root, nil, "GetUnarchivable")
		tx := &recTx{}
		be := newFlatBE(&recBeginner{tx: tx})
		if err := be.Unarchive(newBuilderCtx(), u, schema, firingHook); err != nil {
			t.Fatalf("Unarchive: %v", err)
		}
		if len(tx.execs) != 3 {
			t.Errorf("expected 3 statements (no child cascade), got %d: %v", len(tx.execs), tx.execs)
		}
	})
}

// aggDeleteSchema declares the full cascade width: a root sibling, a child, and
// the child's own sibling — hard delete must clear all four tables in order.
func aggDeleteSchema() *TableSchema {
	return NewTableSchema[*aggWriteRoot]("agg_w").
		PK("id").Field("Name", "name").SoftDelete("deleted_at").
		Sibling(NewSiblingSchema[*aggWriteRoot]("agg_w_sib").Field("Name", "name")).
		Child(NewTableSchema[aggWriteChild]("agg_w_children").
			PK("id").FK("agg_w_id").Field("Label", "label").SoftDelete("deleted_at").
			Sibling(NewSiblingSchema[aggWriteChild]("agg_w_child_sib").Field("Label", "label")))
}

func TestHardDelete_FullCascadeOrderAndStepFailures(t *testing.T) {
	newDeletable := func(t *testing.T) domain.Deletable {
		t.Helper()
		root := &aggWriteRoot{Name: "r"}
		root.SetID(domain.NewID(uuid.NewString()))
		d, err := domain.GetDeletable(root, nil, "GetDeletable")
		if err != nil {
			t.Fatalf("GetDeletable: %v", err)
		}
		return d
	}

	// Happy path: child sibling → child → root sibling → root → outbox → audit.
	tx := &recTx{}
	be := newFlatBE(&recBeginner{tx: tx})
	if err := be.Delete(newBuilderCtx(), newDeletable(t), aggDeleteSchema(), firingHook); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(tx.execs) != 6 {
		t.Fatalf("expected 6 statements, got %d: %v", len(tx.execs), tx.execs)
	}
	wantOrder := []string{"agg_w_child_sib", "agg_w_children", "agg_w_sib", "DELETE FROM agg_w WHERE", "outbox", "audit_events"}
	for i, sub := range wantOrder {
		if !strings.Contains(tx.execs[i], sub) {
			t.Errorf("stmt[%d]: expected %q, got %q", i, sub, tx.execs[i])
		}
	}

	cases := verbSteps(
		execStep("DELETE FROM agg_w_child_sib"),
		execStep("DELETE FROM agg_w_children"),
		execStep("DELETE FROM agg_w_sib"),
		execStep("DELETE FROM agg_w WHERE"),
		execStep("INSERT INTO outbox"),
		execStep("INSERT INTO audit_events"),
	)
	runStepCases(t, cases, func(be *BaseEngine, hook WriteHook) error {
		return be.Delete(newBuilderCtx(), newDeletable(t), aggDeleteSchema(), hook)
	})
}

// The root sibling INSERT and the sibling upsert are steps of the aggregate
// insert/update sequences — inject failures there too.
func TestAggregateSiblingWrites_StepFailures(t *testing.T) {
	t.Run("insertSiblingFails", func(t *testing.T) {
		root := &aggWriteRoot{Name: "r"}
		ins, _ := domain.GetInsertable(root, nil, "GetInsertable")
		tx := &recTx{execErrSub: "agg_w_sib"}
		be := newFlatBE(&recBeginner{tx: tx})
		if _, err := be.Insert(newBuilderCtx(), ins, aggDeleteSchema(), firingHook); !errors.Is(err, errRecExec) {
			t.Fatalf("expected the sibling INSERT failure, got %v", err)
		}
		if tx.committed {
			t.Error("must not commit")
		}
	})
	t.Run("updateSiblingUpsertFails", func(t *testing.T) {
		root := &aggWriteRoot{Name: "r"}
		root.SetID(domain.NewID(uuid.NewString()))
		upd, _ := domain.GetUpdatable(root, func(*aggWriteRoot) error { return nil }, nil, "GetUpdatable")
		tx := &recTx{count: 1, execErrSub: "agg_w_sib"}
		be := newFlatBE(&recBeginner{tx: tx})
		if _, err := be.Update(newBuilderCtx(), upd, aggDeleteSchema(), firingHook); !errors.Is(err, errRecExec) {
			t.Fatalf("expected the sibling upsert failure, got %v", err)
		}
		if tx.committed {
			t.Error("must not commit")
		}
	})
}

// ─── Flat verbs: late-step failures ─────────────────────────────────────────

func TestFlatInsert_LateStepFailures(t *testing.T) {
	cases := []stepCase{
		execStep("INSERT INTO outbox"),
		execStep("INSERT INTO audit_events"),
		{name: "beforeCommit", tx: &recTx{}, hook: failBeforeCommit},
	}
	runStepCases(t, cases, func(be *BaseEngine, hook WriteHook) error {
		_, err := be.Insert(newBuilderCtx(), flatInsertable(t), builderTestSchema, hook)
		return err
	})
}

func TestFlatUpdate_StepFailures(t *testing.T) {
	cases := verbSteps(
		execStep("INSERT INTO outbox"),
		execStep("INSERT INTO audit_events"),
	)
	runStepCases(t, cases, func(be *BaseEngine, hook WriteHook) error {
		_, err := be.Update(newBuilderCtx(), flatUpdatable(t), builderTestSchema, hook)
		return err
	})
}

func TestFlatSoftWrite_StepFailures(t *testing.T) {
	newArchivable := func(t *testing.T) domain.Archivable {
		t.Helper()
		e := &builderTestEntity{Name: "a", Email: "a@x"}
		e.SetID(domain.NewID(uuid.NewString()))
		a, err := domain.GetArchivable(e, nil, "GetArchivable")
		if err != nil {
			t.Fatalf("GetArchivable: %v", err)
		}
		return a
	}
	cases := verbSteps(
		execStep("UPDATE builder_test_entities SET"),
		execStep("INSERT INTO outbox"),
		execStep("INSERT INTO audit_events"),
	)
	runStepCases(t, cases, func(be *BaseEngine, hook WriteHook) error {
		return be.Archive(newBuilderCtx(), newArchivable(t), builderTestSchema, hook)
	})
}

func TestFlatDelete_StepFailures(t *testing.T) {
	newDeletable := func(t *testing.T) domain.Deletable {
		t.Helper()
		e := &builderTestEntity{Name: "a", Email: "a@x"}
		e.SetID(domain.NewID(uuid.NewString()))
		d, err := domain.GetDeletable(e, nil, "GetDeletable")
		if err != nil {
			t.Fatalf("GetDeletable: %v", err)
		}
		return d
	}
	cases := verbSteps(
		execStep("DELETE FROM builder_test_entities"),
		execStep("INSERT INTO outbox"),
		execStep("INSERT INTO audit_events"),
	)
	runStepCases(t, cases, func(be *BaseEngine, hook WriteHook) error {
		return be.Delete(newBuilderCtx(), newDeletable(t), builderTestSchema, hook)
	})
}

// ─── Batch ───────────────────────────────────────────────────────────────────

func batchOps(t *testing.T) ([]domain.ValidEntity, []*TableSchema) {
	t.Helper()
	mk := func() *builderTestEntity {
		e := &builderTestEntity{Name: "a", Email: "a@x"}
		e.SetID(domain.NewID(uuid.NewString()))
		return e
	}
	ins, err := domain.GetInsertable(&builderTestEntity{Name: "a", Email: "a@x"}, nil, "GetInsertable")
	if err != nil {
		t.Fatalf("GetInsertable: %v", err)
	}
	upd, err := domain.GetUpdatable(mk(), func(*builderTestEntity) error { return nil }, nil, "GetUpdatable")
	if err != nil {
		t.Fatalf("GetUpdatable: %v", err)
	}
	arc, err := domain.GetArchivable(mk(), nil, "GetArchivable")
	if err != nil {
		t.Fatalf("GetArchivable: %v", err)
	}
	una, err := domain.GetUnarchivable(mk(), nil, "GetUnarchivable")
	if err != nil {
		t.Fatalf("GetUnarchivable: %v", err)
	}
	del, err := domain.GetDeletable(mk(), nil, "GetDeletable")
	if err != nil {
		t.Fatalf("GetDeletable: %v", err)
	}
	ops := []domain.ValidEntity{ins, upd, arc, una, del}
	schemas := []*TableSchema{builderTestSchema, builderTestSchema, builderTestSchema, builderTestSchema, builderTestSchema}
	return ops, schemas
}

func TestBatch_AllVerbsOneTx(t *testing.T) {
	ops, schemas := batchOps(t)
	tx := &recTx{count: 1}
	be := newFlatBE(&recBeginner{tx: tx})
	results, err := be.Batch(newBuilderCtx(), domain.NewBatch(ops), schemas)
	if err != nil {
		t.Fatalf("Batch: %v", err)
	}
	if len(results) != 5 {
		t.Fatalf("expected 5 results, got %d", len(results))
	}
	if !tx.committed {
		t.Error("expected one commit for the whole batch")
	}
	// Each member lands its data statement + one outbox row (audit is skipped by
	// design for Batch): 5 × 2 = 10.
	if len(tx.execs) != 10 {
		t.Errorf("expected 10 statements, got %d: %v", len(tx.execs), tx.execs)
	}
	for _, sql := range tx.execs {
		if strings.Contains(sql, "audit_events") {
			t.Errorf("Batch must not write audit rows, got %q", sql)
		}
	}
}

func TestBatch_ErrorBranches(t *testing.T) {
	t.Run("schemaCountMismatch", func(t *testing.T) {
		ops, _ := batchOps(t)
		be := newFlatBE(&recBeginner{tx: &recTx{}})
		if _, err := be.Batch(newBuilderCtx(), domain.NewBatch(ops), []*TableSchema{builderTestSchema}); err == nil ||
			!strings.Contains(err.Error(), "one TableSchema per operation") {
			t.Fatalf("expected the schema-count guard, got %v", err)
		}
	})
	t.Run("beginError", func(t *testing.T) {
		ops, schemas := batchOps(t)
		be := newFlatBE(&recBeginner{beginErr: errBoom})
		if _, err := be.Batch(newBuilderCtx(), domain.NewBatch(ops), schemas); !errors.Is(err, errBoom) {
			t.Fatalf("expected the begin error, got %v", err)
		}
	})
	t.Run("memberFailureRollsBack", func(t *testing.T) {
		ops, schemas := batchOps(t)
		for _, sub := range []string{
			"INSERT INTO builder_test_entities", // insert member
			"UPDATE builder_test_entities SET",  // update member
			"DELETE FROM builder_test_entities", // delete member
			"INSERT INTO outbox",                // any member's outbox row
		} {
			tx := &recTx{count: 1, execErrSub: sub}
			be := newFlatBE(&recBeginner{tx: tx})
			if _, err := be.Batch(newBuilderCtx(), domain.NewBatch(ops), schemas); !errors.Is(err, errRecExec) {
				t.Fatalf("%s: expected the member failure, got %v", sub, err)
			}
			if tx.committed {
				t.Errorf("%s: must not commit", sub)
			}
		}
	})
	t.Run("updateMemberNotFound", func(t *testing.T) {
		ops, schemas := batchOps(t)
		tx := &recTx{count: 0} // the update member matches no row
		be := newFlatBE(&recBeginner{tx: tx})
		_, err := be.Batch(newBuilderCtx(), domain.NewBatch(ops), schemas)
		if _, ok := errAsCarrier(err); !ok {
			t.Fatalf("expected a NotFound carrier, got %T (%v)", err, err)
		}
	})
	t.Run("archiveMemberWithoutSoftDelete", func(t *testing.T) {
		noSD := NewTableSchema[*builderTestEntity]("nsd").PK("id").Revision("revision").Field("Name", "name").Field("Email", "email")
		e := &builderTestEntity{Name: "a", Email: "a@x"}
		e.SetID(domain.NewID(uuid.NewString()))
		arc, _ := domain.GetArchivable(e, nil, "GetArchivable")
		be := newFlatBE(&recBeginner{tx: &recTx{}})
		if _, err := be.Batch(newBuilderCtx(), domain.NewBatch([]domain.ValidEntity{arc}), []*TableSchema{noSD}); err == nil {
			t.Fatal("expected the missing-SoftDelete guard")
		}
	})
	t.Run("commitError", func(t *testing.T) {
		ops, schemas := batchOps(t)
		tx := &recTx{count: 1, commitErr: errBoom}
		be := newFlatBE(&recBeginner{tx: tx})
		if _, err := be.Batch(newBuilderCtx(), domain.NewBatch(ops), schemas); !errors.Is(err, errBoom) {
			t.Fatalf("expected the commit error, got %v", err)
		}
	})
}

// ─── BaseRepository.Scope + boundWriter delegation ──────────────────────────

func TestBaseRepositoryScope_DelegatesAllVerbs(t *testing.T) {
	repo := &BaseRepository[*builderTestEntity]{
		Engine:    fakeRelEngine{},
		NewEntity: func() *builderTestEntity { return &builderTestEntity{} },
	}
	repo.WithSchema(builderTestSchema)
	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG)
	w := repo.Scope(ctx)

	mk := func() *builderTestEntity {
		e := &builderTestEntity{Name: "a", Email: "a@x"}
		e.SetID(domain.NewID(uuid.NewString()))
		return e
	}
	ins, _ := domain.GetInsertable(&builderTestEntity{Name: "a", Email: "a@x"}, nil, "GetInsertable")
	if _, err := w.Insert(ins); err != nil {
		t.Errorf("Insert: %v", err)
	}
	upd, _ := domain.GetUpdatable(mk(), func(*builderTestEntity) error { return nil }, nil, "GetUpdatable")
	if err := w.Update(upd); err != nil {
		t.Errorf("Update: %v", err)
	}
	arc, _ := domain.GetArchivable(mk(), nil, "GetArchivable")
	if err := w.Archive(arc); err != nil {
		t.Errorf("Archive: %v", err)
	}
	una, _ := domain.GetUnarchivable(mk(), nil, "GetUnarchivable")
	if err := w.Unarchive(una); err != nil {
		t.Errorf("Unarchive: %v", err)
	}
	del, _ := domain.GetDeletable(mk(), nil, "GetDeletable")
	if err := w.Delete(del); err != nil {
		t.Errorf("Delete: %v", err)
	}
}

// ─── BaseEngine post-commit surface ──────────────────────────────────────────

type fakePublisher struct {
	err error
	got int
}

func (p *fakePublisher) Publish(_ persistence.RequestContext, _ domain.Event) error {
	p.got++
	return p.err
}

func (p *fakePublisher) PublishAll(_ persistence.RequestContext, evs []domain.DomainEvent) error {
	p.got += len(evs)
	return p.err
}

func TestPublishEvents_SetPublisherAndBestEffort(t *testing.T) {
	ctx := newBuilderCtx()
	evs := []domain.DomainEvent{{Type: domain.EventLog, Class: "X", Msg: "m"}}

	// nil publisher → no-op.
	be := newFlatBE(&recBeginner{tx: &recTx{}})
	be.PublishEvents(ctx, evs)

	// wired publisher receives the events.
	pub := &fakePublisher{}
	be.SetPublisher(pub)
	be.PublishEvents(ctx, evs)
	if pub.got != 1 {
		t.Errorf("expected 1 published event, got %d", pub.got)
	}

	// empty event list → no-op even with a publisher.
	be.PublishEvents(ctx, nil)
	if pub.got != 1 {
		t.Errorf("no extra publish expected, got %d", pub.got)
	}

	// a failing publisher is swallowed (best-effort, logged at Warn).
	be.SetPublisher(&fakePublisher{err: errBoom})
	be.PublishEvents(ctx, evs) // must not panic nor propagate
}

// ─── Pure helpers ────────────────────────────────────────────────────────────

func TestScalarString(t *testing.T) {
	s := "v"
	var nilP *string
	cases := []struct {
		in   any
		want string
	}{
		{"plain", "plain"},
		{42, "42"},
		{&s, "v"},
		{nilP, ""},
	}
	for _, c := range cases {
		if got := scalarString(c.in); got != c.want {
			t.Errorf("scalarString(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestIsNilValue(t *testing.T) {
	var nilP *string
	var nilM map[string]any
	var nilS []string
	s := "x"
	cases := []struct {
		in   any
		want bool
	}{
		{nil, true},
		{nilP, true},
		{nilM, true},
		{nilS, true},
		{&s, false},
		{"x", false},
		{0, false},
	}
	for _, c := range cases {
		if got := isNilValue(c.in); got != c.want {
			t.Errorf("isNilValue(%#v) = %v, want %v", c.in, got, c.want)
		}
	}
}

// WriteOutbox marshals the payload first — an unmarshalable payload surfaces
// the json error before any statement runs.
func TestWriteOutbox_MarshalError(t *testing.T) {
	tx := &recTx{}
	if err := WriteOutbox(context.Background(), tx, "t", "INSERTED", "id", map[string]any{"bad": make(chan int)}); err == nil {
		t.Fatal("expected a json.Marshal error")
	}
	if len(tx.execs) != 0 {
		t.Errorf("no statement may run on marshal failure, got %v", tx.execs)
	}
}

// ─── SharedBase write paths: per-step failures ───────────────────────────────

// scriptedQuery routes tx.Query by SQL substring: errSubs inject probe
// failures, foundSubs script a 1-row hit; anything else misses (0 rows).
func scriptedQuery(errSubs, foundSubs []string) func(string, []any) (Rows, error) {
	return func(sql string, _ []any) (Rows, error) {
		for _, sub := range errSubs {
			if strings.Contains(sql, sub) {
				return nil, errBoom
			}
		}
		for _, sub := range foundSubs {
			if strings.Contains(sql, sub) {
				// The 1/0 CASE projections (baseIsArchived, the natural-key
				// guard) scan 1; other probes only need the row to exist.
				return &fakeRows{remaining: 1, scan: func(dest []any) error {
					if len(dest) == 1 {
						if p, ok := dest[0].(*bool); ok {
							*p = true
						}
						if p, ok := dest[0].(*int64); ok {
							*p = 1 // the ANSI CASE 1/0 projection (baseIsArchived)
						}
					}
					return nil
				}}, nil
			}
		}
		return &fakeRows{remaining: 0}, nil
	}
}

func roleInsertable(t *testing.T, action string) domain.Insertable {
	t.Helper()
	ins, err := domain.GetInsertable(&roleTestEntity{Name: "Ana", Document: "D1", Matricula: "M1"}, nil, action)
	if err != nil {
		t.Fatalf("GetInsertable: %v", err)
	}
	return ins
}

func roleUpdatable(t *testing.T) domain.Updatable {
	t.Helper()
	e := &roleTestEntity{Name: "Ana", Document: "D1", Matricula: "M1"}
	e.SetID(domain.NewID(uuid.NewString()))
	upd, err := domain.GetUpdatable(e, func(*roleTestEntity) error { return nil }, nil, "GetUpdatable")
	if err != nil {
		t.Fatalf("GetUpdatable: %v", err)
	}
	return upd
}

func TestInsertWithBase_StepFailures(t *testing.T) {
	run := func(tx *recTx, beginErr error, hook WriteHook) error {
		if tx != nil && tx.queryFn == nil {
			tx.queryFn = scriptedQuery(nil, nil)
		}
		be := newFlatBE(&recBeginner{tx: tx, beginErr: beginErr})
		_, err := be.Insert(newBuilderCtx(), roleInsertable(t, "GetUpsertable"), roleTestSchema(), hook)
		return err
	}

	cases := verbSteps(
		execStep("INSERT INTO pessoa"), // shared-base cold INSERT (identity is new)
		execStep("INSERT INTO aluno"),  // role INSERT
		execStep("INSERT INTO outbox"), // role outbox (first outbox row)
		execStep("INSERT INTO audit_events"),
	)
	runStepCases(t, cases, func(be *BaseEngine, hook WriteHook) error {
		if rb, ok := be.beginner.(*recBeginner); ok && rb.tx != nil && rb.tx.queryFn == nil {
			rb.tx.queryFn = scriptedQuery(nil, nil)
		}
		_, err := be.Insert(newBuilderCtx(), roleInsertable(t, "GetUpsertable"), roleTestSchema(), hook)
		return err
	})

	t.Run("baseExistsProbeError", func(t *testing.T) {
		tx := &recTx{queryFn: scriptedQuery([]string{"FROM pessoa"}, nil)}
		if err := run(tx, nil, firingHook); !errors.Is(err, errBoom) {
			t.Fatalf("expected the probe error, got %v", err)
		}
	})
	t.Run("activeRoleProbeError", func(t *testing.T) {
		tx := &recTx{queryFn: scriptedQuery([]string{"FROM aluno"}, nil)}
		if err := run(tx, nil, firingHook); !errors.Is(err, errBoom) {
			t.Fatalf("expected the probe error, got %v", err)
		}
	})
	t.Run("forgotGuard_blindInsertOnExistingIdentity", func(t *testing.T) {
		tx := &recTx{queryFn: scriptedQuery(nil, []string{"FROM pessoa"})}
		be := newFlatBE(&recBeginner{tx: tx})
		_, err := be.Insert(newBuilderCtx(), roleInsertable(t, "GetInsertable"), roleTestSchema(), firingHook)
		if err == nil || !strings.Contains(err.Error(), "load it first") {
			t.Fatalf("expected the forgot-guard error, got %v", err)
		}
	})
	t.Run("insertEmitsSingleOutboxRow", func(t *testing.T) {
		// v2 single-row contract: the role INSERT emits exactly ONE outbox row
		// (the self-sufficient v2 payload) — the historical empty base-table
		// fan-out row must NOT exist.
		tx := &recTx{queryFn: scriptedQuery(nil, nil)}
		be := newFlatBE(&recBeginner{tx: tx})
		if _, err := be.Insert(newBuilderCtx(), roleInsertable(t, "GetUpsertable"), roleTestSchema(), firingHook); err != nil {
			t.Fatalf("Insert: %v", err)
		}
		outboxRows := 0
		for _, s := range tx.execs {
			if strings.HasPrefix(s, "INSERT INTO outbox") {
				outboxRows++
			}
		}
		if outboxRows != 1 {
			t.Fatalf("a role insert must emit exactly ONE outbox row (v2), got %d: %v", outboxRows, tx.execs)
		}
	})
}

// nthFailTx wraps recTx and fails the Nth statement matching failSub — for
// sequences with two identical statement shapes (role outbox + base fan-out).
type nthFailTx struct {
	*recTx
	failSub string
	failOn  int
	seen    int
}

func (t *nthFailTx) Exec(ctx context.Context, sql string, args ...any) error {
	if strings.Contains(sql, t.failSub) {
		t.seen++
		if t.seen == t.failOn {
			t.execs = append(t.execs, sql)
			return errBoom
		}
	}
	return t.recTx.Exec(ctx, sql, args...)
}

type singleTxBeginner struct{ tx WriteTx }

func (b singleTxBeginner) Begin(context.Context) (WriteTx, error) { return b.tx, nil }

func TestUpdateWithBase_StepFailures(t *testing.T) {
	newTx := func(sub string) *recTx {
		return &recTx{count: 1, execErrSub: sub, queryFn: scriptedQuery(nil, []string{"FROM aluno"})}
	}
	run := func(tx *recTx, beginErr error, hook WriteHook) error {
		be := newFlatBE(&recBeginner{tx: tx, beginErr: beginErr})
		_, err := be.Update(newBuilderCtx(), roleUpdatable(t), roleTestSchema(), hook)
		return err
	}

	t.Run("begin", func(t *testing.T) {
		if err := run(nil, errBoom, firingHook); !errors.Is(err, errBoom) {
			t.Fatalf("expected begin error, got %v", err)
		}
	})
	for _, sub := range []string{"UPDATE aluno SET", "UPDATE pessoa SET", "INSERT INTO outbox", "INSERT INTO audit_events"} {
		t.Run("exec:"+sub, func(t *testing.T) {
			tx := newTx(sub)
			if err := run(tx, nil, firingHook); !errors.Is(err, errRecExec) {
				t.Fatalf("expected exec error, got %v", err)
			}
			if tx.committed {
				t.Error("must not commit")
			}
		})
	}
	t.Run("roleNotFound", func(t *testing.T) {
		tx := &recTx{count: 0, queryFn: scriptedQuery(nil, []string{"FROM aluno"})}
		err := run(tx, nil, firingHook)
		if _, ok := errAsCarrier(err); !ok {
			t.Fatalf("expected NotFound carrier, got %T (%v)", err, err)
		}
	})
	t.Run("beforeCommitAndCommit", func(t *testing.T) {
		tx := &recTx{count: 1, queryFn: scriptedQuery(nil, []string{"FROM aluno"})}
		if err := run(tx, nil, failBeforeCommit); !errors.Is(err, errBoom) {
			t.Fatalf("expected the beforeCommit error, got %v", err)
		}
		tx = &recTx{count: 1, commitErr: errBoom, queryFn: scriptedQuery(nil, []string{"FROM aluno"})}
		if err := run(tx, nil, firingHook); !errors.Is(err, errBoom) {
			t.Fatalf("expected the commit error, got %v", err)
		}
	})
	t.Run("missingNaturalKeyValue", func(t *testing.T) {
		e := &roleTestEntity{Name: "Ana", Document: "", Matricula: "M1"} // empty natural key
		e.SetID(domain.NewID(uuid.NewString()))
		upd, _ := domain.GetUpdatable(e, func(*roleTestEntity) error { return nil }, nil, "GetUpdatable")
		be := newFlatBE(&recBeginner{tx: &recTx{count: 1}})
		if _, err := be.Update(newBuilderCtx(), upd, roleTestSchema(), firingHook); err == nil {
			t.Fatal("expected the natural-key guard to error")
		}
	})
}

// ─── SharedBase lifecycle cascade (soft-deletable base with native child) ───

// cascadeRoleSchema: a role over a base that HAS SoftDelete and one native
// child — the shape whose lifecycle converges on archive/unarchive.
type cascadeBaseChild struct {
	ID   string
	Note string
}

func (c cascadeBaseChild) GetID() domain.ID                                 { return domain.NewID(c.ID) }
func (c cascadeBaseChild) BuildRules(string, domain.Service, *domain.Rules) {}

func cascadeRoleSchema() *TableSchema {
	base := NewSharedBase("pessoa").Revision("revision").
		PK("id").
		Field("Name", "name").
		Field("Document", "document").
		NaturalKey("document").
		SoftDelete("deleted_at").
		Child(NewTableSchema[cascadeBaseChild]("pessoa_filhos").
			PK("id").FK("pessoa_id").Field("Note", "note").SoftDelete("deleted_at"))
	return NewTableSchema[*roleTestEntity]("aluno").
		PK("id").
		Field("Matricula", "matricula").
		SoftDelete("deleted_at").
		SharedBase(base, "pessoa_id")
}

func TestArchiveRole_CascadesBaseAndNativeChildren(t *testing.T) {
	e := &roleTestEntity{Name: "Ana", Document: "D1", Matricula: "M1"}
	e.SetID(domain.NewID(uuid.NewString()))
	a, _ := domain.GetArchivable(e, nil, "GetArchivable")

	// No role stays active → the base + its native child archive with the role.
	tx := &recTx{queryFn: scriptedQuery(nil, nil)}
	be := newFlatBE(&recBeginner{tx: tx})
	if err := be.Archive(newBuilderCtx(), a, cascadeRoleSchema(), firingHook); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if !tx.committed {
		t.Fatal("expected commit")
	}
	var basePessoa, baseChild bool
	for _, sql := range tx.execs {
		if strings.Contains(sql, "UPDATE pessoa SET") {
			basePessoa = true
		}
		if strings.Contains(sql, "UPDATE pessoa_filhos SET") {
			baseChild = true
		}
	}
	if !basePessoa || !baseChild {
		t.Errorf("expected the base + native child cascade, got %v", tx.execs)
	}
}

func TestArchiveRole_CascadeStepFailures(t *testing.T) {
	newArchivable := func(t *testing.T) domain.Archivable {
		t.Helper()
		e := &roleTestEntity{Name: "Ana", Document: "D1", Matricula: "M1"}
		e.SetID(domain.NewID(uuid.NewString()))
		a, err := domain.GetArchivable(e, nil, "GetArchivable")
		if err != nil {
			t.Fatalf("GetArchivable: %v", err)
		}
		return a
	}

	t.Run("lifecycleProbeError", func(t *testing.T) {
		tx := &recTx{queryFn: scriptedQuery([]string{"FROM aluno"}, nil)} // anyActiveRole probe fails
		be := newFlatBE(&recBeginner{tx: tx})
		if err := be.Archive(newBuilderCtx(), newArchivable(t), cascadeRoleSchema(), firingHook); !errors.Is(err, errBoom) {
			t.Fatalf("expected the probe error, got %v", err)
		}
	})
	for _, sub := range []string{"UPDATE pessoa SET", "UPDATE pessoa_filhos SET"} {
		t.Run("exec:"+sub, func(t *testing.T) {
			tx := &recTx{execErrSub: sub, queryFn: scriptedQuery(nil, nil)}
			be := newFlatBE(&recBeginner{tx: tx})
			if err := be.Archive(newBuilderCtx(), newArchivable(t), cascadeRoleSchema(), firingHook); !errors.Is(err, errRecExec) {
				t.Fatalf("expected the cascade exec error, got %v", err)
			}
			if tx.committed {
				t.Error("must not commit")
			}
		})
	}
}

func TestUnarchiveRole_VetoProbeError(t *testing.T) {
	e := &roleTestEntity{Name: "Ana", Document: "D1", Matricula: "M1"}
	e.SetID(domain.NewID(uuid.NewString()))
	u, _ := domain.GetUnarchivable(e, nil, "GetUnarchivable")

	// The one-active-role veto probe fails → the unarchive aborts pre-write.
	tx := &recTx{queryFn: scriptedQuery([]string{"FROM aluno"}, nil)}
	be := newFlatBE(&recBeginner{tx: tx})
	if err := be.Unarchive(newBuilderCtx(), u, roleTestSchema(), firingHook); !errors.Is(err, errBoom) {
		t.Fatalf("expected the veto probe error, got %v", err)
	}
	if tx.committed {
		t.Error("must not commit")
	}
}
