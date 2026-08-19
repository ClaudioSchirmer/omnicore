package handlers

import (
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/persistence"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	fwresults "github.com/ClaudioSchirmer/omnicore/application/results"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/google/uuid"
)

// End-to-end over the Auto handlers: with a Command that mutates a PERSISTED
// field in ApplyTo and a domain that mutates another one from inside
// IfArchive/IfUnarchive, domain.Old must still answer the state the repository
// handed over. The manual-route twin at the bottom proves the same guarantee
// for a hand-written handler (critical rule #3: Auto == manual).

type tenantEntity struct {
	domain.BaseEntity
	Name   string
	Status string
}

func (e *tenantEntity) Modes() []domain.EntityMode {
	return []domain.EntityMode{
		domain.ModeInsert, domain.ModeUpdate, domain.ModeDelete,
		domain.ModeArchive, domain.ModeUnarchive,
	}
}
func (e *tenantEntity) RequiresService() bool { return false }
func (e *tenantEntity) BuildRules(_ string, _ domain.Service, r *domain.Rules) {
	r.IfArchive(func() { e.Status = "suspended" })
	r.IfUnarchive(func() { e.Status = "active" })
}

// tenantRepo is the hand-rolled repository shape (no ScopedReader), so the load
// goes through persistence.LoadForWrite's own snapshot net.
type tenantRepo struct {
	stored          *tenantEntity
	archiveCalled   int
	unarchiveCalled int
	deleteCalled    int
	updateCalled    int
}

func newTenantRepo() *tenantRepo {
	e := &tenantEntity{Name: "acme", Status: "trial"}
	e.SetID(domain.NewID(uuid.NewString()))
	return &tenantRepo{stored: e}
}

func (r *tenantRepo) FindByID(domain.ID) (*tenantEntity, error)         { return r.stored, nil }
func (r *tenantRepo) FindArchivedByID(domain.ID) (*tenantEntity, error) { return r.stored, nil }
func (r *tenantRepo) New() *tenantEntity                                { return &tenantEntity{} }
func (r *tenantRepo) Scope(*configuration.AppContext, ...persistence.WriteOption[*tenantEntity]) domain.Writer {
	return &tenantWriter{repo: r}
}

type tenantWriter struct{ repo *tenantRepo }

func (w *tenantWriter) Insert(domain.Insertable) (domain.ID, error) {
	return domain.NewRandomID(), nil
}
func (w *tenantWriter) Update(domain.Updatable) error { w.repo.updateCalled++; return nil }
func (w *tenantWriter) Delete(domain.Deletable) error { w.repo.deleteCalled++; return nil }
func (w *tenantWriter) Archive(domain.Archivable) error {
	w.repo.archiveCalled++
	return nil
}
func (w *tenantWriter) Unarchive(domain.Unarchivable) error {
	w.repo.unarchiveCalled++
	return nil
}

// tenantCmd mutates a PERSISTED field in ApplyTo — the seam the handler calls
// between the load and the verb.
type tenantCmd struct {
	pipeline.CommandByIDBase
	applied bool
}

func (c *tenantCmd) ApplyTo(_ *configuration.AppContext, e *tenantEntity) error {
	c.applied = true
	e.Name = "renamed-by-applyto"
	return nil
}
func (c *tenantCmd) FromEntity(_ *configuration.AppContext, _ *tenantEntity) (fwresults.None, error) {
	return fwresults.None{}, nil
}

func assertPersistedSnapshot(t *testing.T, e *tenantEntity, wantStatus string) {
	t.Helper()
	old := domain.Old(e)
	if old == nil {
		t.Fatal("domain.Old must be non-nil after a framework load")
	}
	if old.Name != "acme" {
		t.Errorf("Old().Name = %q, want the persisted %q — ApplyTo's mutation leaked into the snapshot", old.Name, "acme")
	}
	if old.Status != wantStatus {
		t.Errorf("Old().Status = %q, want the persisted %q — a rule's mutation leaked into the snapshot", old.Status, wantStatus)
	}
}

func TestArchiveHandler_OldIsThePersistedState(t *testing.T) {
	repo := newTenantRepo()
	h := &ArchiveCommandHandler[*tenantEntity, *tenantCmd, fwresults.None]{Repo: repo}
	cmd := &tenantCmd{}
	cmd.SetPathID(repo.stored.GetID().String())

	if _, err := h.Handle(testCtx(), cmd); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if !cmd.applied || repo.archiveCalled != 1 {
		t.Fatalf("handler did not run the verb: applied=%v archiveCalled=%d", cmd.applied, repo.archiveCalled)
	}
	// Both mutations must be LIVE on the entity the write path received...
	if repo.stored.Name != "renamed-by-applyto" || repo.stored.Status != "suspended" {
		t.Fatalf("live entity lost a mutation: Name=%q Status=%q", repo.stored.Name, repo.stored.Status)
	}
	// ...and absent from the snapshot.
	assertPersistedSnapshot(t, repo.stored, "trial")
}

func TestUnarchiveHandler_OldIsThePersistedState(t *testing.T) {
	repo := newTenantRepo()
	repo.stored.Status = "suspended"
	h := &UnarchiveCommandHandler[*tenantEntity, *tenantCmd, fwresults.None]{Repo: repo}
	cmd := &tenantCmd{}
	cmd.SetPathID(repo.stored.GetID().String())

	if _, err := h.Handle(testCtx(), cmd); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if repo.unarchiveCalled != 1 {
		t.Fatalf("expected Unarchive once, got %d", repo.unarchiveCalled)
	}
	if repo.stored.Status != "active" {
		t.Fatalf("IfUnarchive must reach the live entity, got Status=%q", repo.stored.Status)
	}
	assertPersistedSnapshot(t, repo.stored, "suspended")
}

func TestDeleteHandler_OldIsThePersistedState(t *testing.T) {
	repo := newTenantRepo()
	h := &DeleteCommandHandler[*tenantEntity, *tenantCmd, fwresults.None]{Repo: repo}
	cmd := &tenantCmd{}
	cmd.SetPathID(repo.stored.GetID().String())

	if _, err := h.Handle(testCtx(), cmd); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if repo.deleteCalled != 1 {
		t.Fatalf("expected Delete once, got %d", repo.deleteCalled)
	}
	// The delete audit event is built from this snapshot — it must describe the
	// row as the system of record held it, not as ApplyTo left it.
	assertPersistedSnapshot(t, repo.stored, "trial")
}

// The manual route: a hand-written handler loads through the framework's load
// helper, mutates, and calls the Get* itself. Same guarantee, no extra step.
func TestManualHandlerShape_OldIsThePersistedState(t *testing.T) {
	repo := newTenantRepo()
	ctx := testCtx()

	current, err := persistence.LoadForWrite[*tenantEntity](repo, ctx, *repo.stored.GetID())
	if err != nil {
		t.Fatalf("LoadForWrite: %v", err)
	}
	current.Name = "renamed-by-hand"

	archivable, err := domain.GetArchivable(current, nil, "GetArchivable")
	if err != nil {
		t.Fatalf("GetArchivable: %v", err)
	}
	if err := repo.Scope(ctx).Archive(archivable); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	old := domain.Old(current)
	if old == nil {
		t.Fatal("a manual handler must get the snapshot from the load, with no extra call")
	}
	if old.Name != "acme" || old.Status != "trial" {
		t.Errorf("manual route snapshot diverged from the Auto route: Name=%q Status=%q", old.Name, old.Status)
	}
}
