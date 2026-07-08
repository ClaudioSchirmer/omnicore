package write

import (
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"

	appaudit "github.com/ClaudioSchirmer/omnicore/application/audit"
	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/persistence"
	"github.com/ClaudioSchirmer/omnicore/domain"
	infraaudit "github.com/ClaudioSchirmer/omnicore/infra/audit"
)

// Second pass over the write-path branch matrix: child-mutation guards, the
// Batch member dispatch edges, the audit-row gate, the WithSchema boot guards,
// and the SharedBase reactivation probes.

// ─── Child mutation guards ───────────────────────────────────────────────────

// A Removed child must carry an id to archive; a Removed child whose schema has
// no soft-delete column cannot be archived (role children must be archivable).
func TestRemovedChild_Guards(t *testing.T) {
	t.Run("missingID", func(t *testing.T) {
		root := &aggWriteRoot{Name: "r"}
		root.SetID(domain.NewID(uuid.NewString()))
		root.AggregateConstructor([]domain.AggregateValueObject{aggWriteChild{ID: "", Label: "x"}})
		upd, _ := domain.GetUpdatable(root, func(r *aggWriteRoot) error {
			domain.RemoveAggregateChild(r, aggWriteChild{ID: "", Label: "x"})
			return nil
		}, nil, "GetUpdatable")
		be := newFlatBE(&recBeginner{tx: &recTx{count: 1}})
		if _, err := be.Update(newBuilderCtx(), upd, aggWriteSchema(), WriteHook{}); err == nil ||
			!strings.Contains(err.Error(), "without id") {
			t.Fatalf("expected the missing-id guard, got %v", err)
		}
	})
	t.Run("roleChildWithoutSoftDelete", func(t *testing.T) {
		id := uuid.NewString()
		schema := NewTableSchema[*aggWriteRoot]("agg_w").
			PK("id").Field("Name", "name").SoftDelete("deleted_at").
			Child(NewTableSchema[aggWriteChild]("agg_w_children").
				PK("id").FK("agg_w_id").Field("Label", "label")) // no SoftDelete
		root := &aggWriteRoot{Name: "r"}
		root.SetID(domain.NewID(uuid.NewString()))
		root.AggregateConstructor([]domain.AggregateValueObject{aggWriteChild{ID: id, Label: "x"}})
		upd, _ := domain.GetUpdatable(root, func(r *aggWriteRoot) error {
			domain.RemoveAggregateChild(r, aggWriteChild{ID: id, Label: "x"})
			return nil
		}, nil, "GetUpdatable")
		be := newFlatBE(&recBeginner{tx: &recTx{count: 1}})
		if _, err := be.Update(newBuilderCtx(), upd, schema, WriteHook{}); err == nil {
			t.Fatal("expected the missing-SoftDelete guard on the removed child")
		}
	})
}

// baseChildRole is an aggregate role whose child belongs to the SHARED BASE —
// a Removed base-child without soft-delete hard-deletes (its lifecycle follows
// the base), instead of erroring like a role child would.
type baseChildRole struct {
	domain.AggregateRoot
	Name      string
	Document  string
	Matricula string
}

func (e *baseChildRole) Modes() []domain.EntityMode {
	return []domain.EntityMode{domain.ModeInsert, domain.ModeUpdate, domain.ModeDelete, domain.ModeArchive, domain.ModeUnarchive}
}
func (e *baseChildRole) BuildRules(string, domain.Service, *domain.Rules) {}
func (e *baseChildRole) GetAggregateRoot() *domain.AggregateRoot          { return &e.AggregateRoot }
func (e *baseChildRole) AggregateChildren() []domain.AggregateValueObject {
	return []domain.AggregateValueObject{cascadeBaseChild{}}
}

func baseChildRoleSchema() *TableSchema {
	base := NewSharedBase("pessoa").
		PK("id").
		Field("Name", "name").
		Field("Document", "document").
		NaturalKey("document").
		Child(NewTableSchema[cascadeBaseChild]("pessoa_filhos").
			PK("id").FK("pessoa_id").Field("Note", "note")) // no SoftDelete → hard-delete on remove
	return NewTableSchema[*baseChildRole]("aluno").
		PK("id").
		Field("Matricula", "matricula").
		SoftDelete("deleted_at").
		SharedBase(base, "pessoa_id")
}

func TestRemovedBaseChild_WithoutSoftDelete(t *testing.T) {
	newUpd := func(t *testing.T, childID string) domain.Updatable {
		t.Helper()
		root := &baseChildRole{Name: "Ana", Document: "D1", Matricula: "M1"}
		root.SetID(domain.NewID(uuid.NewString()))
		root.AggregateConstructor([]domain.AggregateValueObject{cascadeBaseChild{ID: childID, Note: "n"}})
		upd, err := domain.GetUpdatable(root, func(r *baseChildRole) error {
			domain.RemoveAggregateChild(r, cascadeBaseChild{ID: childID, Note: "n"})
			return nil
		}, nil, "GetUpdatable")
		if err != nil {
			t.Fatalf("GetUpdatable: %v", err)
		}
		return upd
	}

	t.Run("hardDeletesTheRow", func(t *testing.T) {
		tx := &recTx{count: 1, queryFn: scriptedQuery(nil, []string{"FROM aluno"})}
		be := newFlatBE(&recBeginner{tx: tx})
		if _, err := be.Update(newBuilderCtx(), newUpd(t, uuid.NewString()), baseChildRoleSchema(), firingHook); err != nil {
			t.Fatalf("Update: %v", err)
		}
		var deleted bool
		for _, sql := range tx.execs {
			if strings.HasPrefix(sql, "DELETE FROM pessoa_filhos") {
				deleted = true
			}
		}
		if !deleted {
			t.Errorf("expected the base child hard-delete, got %v", tx.execs)
		}
	})
	t.Run("missingIDIsError", func(t *testing.T) {
		tx := &recTx{count: 1, queryFn: scriptedQuery(nil, []string{"FROM aluno"})}
		be := newFlatBE(&recBeginner{tx: tx})
		_, err := be.Update(newBuilderCtx(), newUpd(t, ""), baseChildRoleSchema(), firingHook)
		if err == nil || !strings.Contains(err.Error(), "without id") {
			t.Fatalf("expected the missing-id guard, got %v", err)
		}
	})
}

// ─── WriteAuditRow gate ──────────────────────────────────────────────────────

func TestWriteAuditRow_GateBranches(t *testing.T) {
	tx := &recTx{}
	// nil event → no-op.
	be := &BaseEngine{}
	if err := be.WriteAuditRow(newBuilderCtx(), tx, nil); err != nil {
		t.Fatalf("nil event: %v", err)
	}
	// audit configured WITHOUT the database destination → no row lands even
	// with a built event.
	be = &BaseEngine{}
	be.SetBeginner(&recBeginner{tx: tx})
	be.ConfigureAudit(
		&infraaudit.Config{Destinations: []infraaudit.Destination{infraaudit.DestinationSlog}},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		nil,
	)
	if err := be.WriteAuditRow(newBuilderCtx(), tx, &appaudit.AuditEvent{EntityType: "X"}); err != nil {
		t.Fatalf("slog-only audit: %v", err)
	}
	if len(tx.execs) != 0 {
		t.Errorf("no statement expected, got %v", tx.execs)
	}
}

// ─── Batch member dispatch edges ─────────────────────────────────────────────

func TestBatch_SoftMembersAndUnsupported(t *testing.T) {
	mk := func() *builderTestEntity {
		e := &builderTestEntity{Name: "a", Email: "a@x"}
		e.SetID(domain.NewID(uuid.NewString()))
		return e
	}

	t.Run("archiveExecError", func(t *testing.T) {
		arc, _ := domain.GetArchivable(mk(), nil, "GetArchivable")
		tx := &recTx{execErrSub: "SET deleted_at = NOW()"}
		be := newFlatBE(&recBeginner{tx: tx})
		if _, err := be.Batch(newBuilderCtx(), domain.NewBatch([]domain.ValidEntity{arc}), []*TableSchema{builderTestSchema}); !errors.Is(err, errRecExec) {
			t.Fatalf("expected the archive exec error, got %v", err)
		}
	})
	t.Run("unarchiveExecError", func(t *testing.T) {
		una, _ := domain.GetUnarchivable(mk(), nil, "GetUnarchivable")
		tx := &recTx{execErrSub: "SET deleted_at = NULL"}
		be := newFlatBE(&recBeginner{tx: tx})
		if _, err := be.Batch(newBuilderCtx(), domain.NewBatch([]domain.ValidEntity{una}), []*TableSchema{builderTestSchema}); !errors.Is(err, errRecExec) {
			t.Fatalf("expected the unarchive exec error, got %v", err)
		}
	})
	t.Run("unarchiveWithoutSoftDelete", func(t *testing.T) {
		noSD := NewTableSchema[*builderTestEntity]("nsd").PK("id").Field("Name", "name").Field("Email", "email")
		una, _ := domain.GetUnarchivable(mk(), nil, "GetUnarchivable")
		be := newFlatBE(&recBeginner{tx: &recTx{}})
		if _, err := be.Batch(newBuilderCtx(), domain.NewBatch([]domain.ValidEntity{una}), []*TableSchema{noSD}); err == nil {
			t.Fatal("expected the missing-SoftDelete guard")
		}
	})
	t.Run("unsupportedMember", func(t *testing.T) {
		// A nested Batch is a ValidEntity that matches no Batch member kind.
		be := newFlatBE(&recBeginner{tx: &recTx{}})
		_, err := be.Batch(newBuilderCtx(), domain.NewBatch([]domain.ValidEntity{domain.NewBatch(nil)}), []*TableSchema{builderTestSchema})
		if err == nil {
			t.Fatal("expected the unsupported-member error")
		}
	})
}

// The flat Unarchive on a schema without SoftDelete errors before any statement.
func TestFlatUnarchive_MissingSoftDeleteIsError(t *testing.T) {
	noSD := NewTableSchema[*builderTestEntity]("nsd").PK("id").Field("Name", "name").Field("Email", "email")
	e := &builderTestEntity{Name: "a", Email: "a@x"}
	e.SetID(domain.NewID(uuid.NewString()))
	u, _ := domain.GetUnarchivable(e, nil, "GetUnarchivable")
	tx := &recTx{}
	be := newFlatBE(&recBeginner{tx: tx})
	if err := be.Unarchive(newBuilderCtx(), u, noSD, WriteHook{}); err == nil {
		t.Fatal("expected the missing-SoftDelete guard")
	}
	if len(tx.execs) != 0 {
		t.Errorf("no statement may run, got %v", tx.execs)
	}
}

// ─── WithSchema boot guards + role registration ─────────────────────────────

type registeringEngine struct {
	fakeRelEngine
	registered []*TableSchema
}

func (e *registeringEngine) RegisterSharedBaseRole(s *TableSchema) {
	e.registered = append(e.registered, s)
}

func TestWithSchema_GuardsAndRoleRegistration(t *testing.T) {
	t.Run("siblingAsRootPanics", func(t *testing.T) {
		defer func() {
			if r := recover(); r == nil || !strings.Contains(r.(string), "cannot be a repository root") {
				t.Fatalf("expected the sibling-as-root panic, got %v", r)
			}
		}()
		repo := &BaseRepository[*builderTestEntity]{
			Engine:    fakeRelEngine{},
			NewEntity: func() *builderTestEntity { return &builderTestEntity{} },
		}
		repo.WithSchema(NewSiblingSchema[*builderTestEntity]("sib").Field("Name", "name"))
	})
	t.Run("sharedBaseRoleRegistersOnEngine", func(t *testing.T) {
		eng := &registeringEngine{}
		repo := &BaseRepository[*roleTestEntity]{
			Engine:    eng,
			NewEntity: func() *roleTestEntity { return &roleTestEntity{} },
		}
		repo.WithSchema(roleTestSchema())
		if len(eng.registered) != 1 {
			t.Fatalf("expected 1 registered role schema, got %d", len(eng.registered))
		}
	})
}

// ─── boundWriter error propagation (mapErr raw path) ─────────────────────────

type erroringEngine struct{ fakeRelEngine }

func (erroringEngine) Insert(persistence.RequestContext, domain.Insertable, *TableSchema, WriteHook) (domain.WriteResult, error) {
	return domain.WriteResult{}, errBoom
}

// A non-constraint engine error passes through mapErr raw.
func TestBoundWriter_MapErrRawPassThrough(t *testing.T) {
	repo := &BaseRepository[*builderTestEntity]{
		Engine:    erroringEngine{},
		NewEntity: func() *builderTestEntity { return &builderTestEntity{} },
		Schema:    builderTestSchema,
	}
	w := repo.Scope(configuration.NewAppContextWithRandomID(configuration.LangENG))
	ins, _ := domain.GetInsertable(&builderTestEntity{Name: "a", Email: "a@x"}, nil, "GetInsertable")
	if _, err := w.Insert(ins); !errors.Is(err, errBoom) {
		t.Fatalf("expected the raw engine error, got %v", err)
	}
}

// ─── SharedBase reactivation probe failures ──────────────────────────────────

func TestSharedBaseReactivationProbeError(t *testing.T) {
	t.Run("insert", func(t *testing.T) {
		ins, _ := domain.GetInsertable(&roleTestEntity{Name: "Ana", Document: "D1", Matricula: "M1"}, nil, "GetUpsertable")
		tx := &recTx{queryFn: scriptedQuery([]string{"IS NOT NULL FROM pessoa"}, nil)}
		be := newFlatBE(&recBeginner{tx: tx})
		if _, err := be.Insert(newBuilderCtx(), ins, cascadeRoleSchema(), firingHook); !errors.Is(err, errBoom) {
			t.Fatalf("expected the reactivation probe error, got %v", err)
		}
		if tx.committed {
			t.Error("must not commit")
		}
	})
	t.Run("update", func(t *testing.T) {
		e := &roleTestEntity{Name: "Ana", Document: "D1", Matricula: "M1"}
		e.SetID(domain.NewID(uuid.NewString()))
		upd, _ := domain.GetUpdatable(e, func(*roleTestEntity) error { return nil }, nil, "GetUpdatable")
		tx := &recTx{count: 1, queryFn: scriptedQuery([]string{"IS NOT NULL FROM pessoa"}, []string{"FROM aluno"})}
		be := newFlatBE(&recBeginner{tx: tx})
		if _, err := be.Update(newBuilderCtx(), upd, cascadeRoleSchema(), firingHook); !errors.Is(err, errBoom) {
			t.Fatalf("expected the reactivation probe error, got %v", err)
		}
		if tx.committed {
			t.Error("must not commit")
		}
	})
	t.Run("updateBaseFanOutOutboxError", func(t *testing.T) {
		e := &roleTestEntity{Name: "Ana", Document: "D1", Matricula: "M1"}
		e.SetID(domain.NewID(uuid.NewString()))
		upd, _ := domain.GetUpdatable(e, func(*roleTestEntity) error { return nil }, nil, "GetUpdatable")
		inner := &recTx{count: 1, queryFn: scriptedQuery(nil, []string{"FROM aluno"})}
		tx := &nthFailTx{recTx: inner, failSub: "INSERT INTO outbox", failOn: 2}
		be := newFlatBE(singleTxBeginner{tx})
		if _, err := be.Update(newBuilderCtx(), upd, roleTestSchema(), firingHook); !errors.Is(err, errBoom) {
			t.Fatalf("expected the fan-out outbox failure, got %v", err)
		}
		if inner.committed {
			t.Error("must not commit")
		}
	})
}
