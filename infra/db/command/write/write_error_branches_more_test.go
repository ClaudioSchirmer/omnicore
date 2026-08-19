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
// no DeletedAt column cannot be archived (role children must be archivable).
func TestRemovedChild_Guards(t *testing.T) {
	t.Run("missingID", func(t *testing.T) {
		root := &aggWriteRoot{Name: "r"}
		root.SetID(domain.NewID(uuid.NewString()))
		root.AggregateConstructor([]domain.AggregateValueObject{aggWriteChild{Label: "x"}})
		upd, _ := domain.GetUpdatable(root, func(r *aggWriteRoot) error {
			domain.RemoveAggregateChild(r, aggWriteChild{Label: "x"})
			return nil
		}, nil, "GetUpdatable")
		be := newFlatBE(&recBeginner{tx: &recTx{count: 1}})
		if _, err := be.Update(newBuilderCtx(), upd, aggWriteSchema(), WriteHook{}); err == nil ||
			!strings.Contains(err.Error(), "without id") {
			t.Fatalf("expected the missing-id guard, got %v", err)
		}
	})
	t.Run("roleChildWithoutDeletedAt", func(t *testing.T) {
		id := uuid.NewString()
		schema := NewTableSchema[*aggWriteRoot]("agg_w").
			ID("id").Field("Name", "name").DeletedAt("deleted_at").
			Child(NewTableSchema[aggWriteChild]("agg_w_children").
				ID("id").ParentID("agg_w_id").Field("Label", "label")) // no DeletedAt
		root := &aggWriteRoot{Name: "r"}
		root.SetID(domain.NewID(uuid.NewString()))
		root.AggregateConstructor([]domain.AggregateValueObject{domain.WithID(aggWriteChild{Label: "x"}, domain.NewID(id))})
		upd, _ := domain.GetUpdatable(root, func(r *aggWriteRoot) error {
			domain.RemoveAggregateChild(r, domain.WithID(aggWriteChild{Label: "x"}, domain.NewID(id)))
			return nil
		}, nil, "GetUpdatable")
		be := newFlatBE(&recBeginner{tx: &recTx{count: 1}})
		if _, err := be.Update(newBuilderCtx(), upd, schema, WriteHook{}); err == nil {
			t.Fatal("expected the missing-DeletedAt guard on the removed child")
		}
	})
}

// baseChildRole is an aggregate role whose child belongs to the SHARED BASE —
// a Removed base-child without DeletedAt hard-deletes (its lifecycle follows
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
	base := NewSharedBaseSchema("pessoa").Revision("revision").
		ID("id").
		Field("Name", "name").
		Field("Document", "document").
		NaturalID("document").
		Child(NewTableSchema[cascadeBaseChild]("pessoa_filhos").
			ID("id").ParentID("pessoa_id").Field("Note", "note")) // no DeletedAt → hard-delete on remove
	return NewTableSchema[*baseChildRole]("aluno").
		ID("id").
		Field("Matricula", "matricula").
		DeletedAt("deleted_at").
		SharedBase(base, "pessoa_id")
}

func TestRemovedBaseChild_WithoutDeletedAt(t *testing.T) {
	newUpd := func(t *testing.T, childID string) domain.Updatable {
		t.Helper()
		root := &baseChildRole{Name: "Ana", Document: "D1", Matricula: "M1"}
		root.SetID(domain.NewID(uuid.NewString()))
		root.AggregateConstructor([]domain.AggregateValueObject{domain.WithID(cascadeBaseChild{Note: "n"}, domain.NewID(childID))})
		upd, err := domain.GetUpdatable(root, func(r *baseChildRole) error {
			domain.RemoveAggregateChild(r, domain.WithID(cascadeBaseChild{Note: "n"}, domain.NewID(childID)))
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

// The flat Unarchive on a schema without DeletedAt errors before any statement.
func TestFlatUnarchive_MissingDeletedAtIsError(t *testing.T) {
	noSD := NewTableSchema[*builderTestEntity]("nsd").ID("id").Revision("revision").Field("Name", "name").Field("Email", "email")
	e := &builderTestEntity{Name: "a", Email: "a@x"}
	e.SetID(domain.NewID(uuid.NewString()))
	u, _ := domain.GetUnarchivable(e, nil, "GetUnarchivable")
	tx := &recTx{}
	be := newFlatBE(&recBeginner{tx: tx})
	if err := be.Unarchive(newBuilderCtx(), u, noSD, WriteHook{}); err == nil {
		t.Fatal("expected the missing-DeletedAt guard")
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
	repo := (&BaseRepository[*builderTestEntity]{
		Engine:    erroringEngine{},
		NewEntity: func() *builderTestEntity { return &builderTestEntity{} },
	}).WithSchema(builderTestSchema)
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
		tx := &recTx{queryFn: scriptedQuery([]string{"ELSE 0 END FROM pessoa"}, nil)}
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
		tx := &recTx{count: 1, queryFn: scriptedQuery([]string{"ELSE 0 END FROM pessoa"}, []string{"FROM aluno"})}
		be := newFlatBE(&recBeginner{tx: tx})
		if _, err := be.Update(newBuilderCtx(), upd, cascadeRoleSchema(), firingHook); !errors.Is(err, errBoom) {
			t.Fatalf("expected the reactivation probe error, got %v", err)
		}
		if tx.committed {
			t.Error("must not commit")
		}
	})
	t.Run("updateEmitsSingleOutboxRow", func(t *testing.T) {
		// single-row contract: the role UPDATE emits exactly ONE outbox row
		// (the self-sufficient payload) — the historical empty base-table
		// fan-out row must NOT exist.
		e := &roleTestEntity{Name: "Ana", Document: "D1", Matricula: "M1"}
		e.SetID(domain.NewID(uuid.NewString()))
		upd, _ := domain.GetUpdatable(e, func(*roleTestEntity) error { return nil }, nil, "GetUpdatable")
		tx := &recTx{count: 1, queryFn: scriptedQuery(nil, []string{"FROM aluno"})}
		be := newFlatBE(&recBeginner{tx: tx})
		if _, err := be.Update(newBuilderCtx(), upd, roleTestSchema(), firingHook); err != nil {
			t.Fatalf("Update: %v", err)
		}
		outboxRows := 0
		for _, s := range tx.execs {
			if strings.HasPrefix(s, "INSERT INTO outbox") {
				outboxRows++
			}
		}
		if outboxRows != 1 {
			t.Fatalf("a role update must emit exactly ONE outbox row, got %d: %v", outboxRows, tx.execs)
		}
	})
}
