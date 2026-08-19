package handlers

import (
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/persistence"
	fwresults "github.com/ClaudioSchirmer/omnicore/application/results"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/google/uuid"
)

// Unarchive REQUIRES a repository that can load an archived aggregate. The
// handler used to fall back to an empty Repo.New() + SetID sample, which works
// only while the verb touches nothing but deleted_at: that sample carries the
// entity's zero value in every business field and no revision, so it can be
// neither written back nor guarded. A repository providing neither capability
// is a wiring error and must fail loudly instead of writing a ghost.

// blindRepo satisfies ScopedRepository but exposes NO archived-scope read.
type blindRepo struct {
	unarchiveCalled int
}

func (r *blindRepo) FindByID(domain.ID) (*testEntity, error) { return &testEntity{}, nil }
func (r *blindRepo) New() *testEntity                        { return &testEntity{} }
func (r *blindRepo) Scope(*configuration.AppContext, ...persistence.WriteOption[*testEntity]) domain.Writer {
	return &blindWriter{repo: r}
}

type blindWriter struct{ repo *blindRepo }

func (w *blindWriter) Insert(domain.Insertable) (domain.ID, error) { return domain.NewRandomID(), nil }
func (w *blindWriter) Update(domain.Updatable) error               { return nil }
func (w *blindWriter) Delete(domain.Deletable) error               { return nil }
func (w *blindWriter) Archive(domain.Archivable) error             { return nil }
func (w *blindWriter) Unarchive(domain.Unarchivable) error {
	w.repo.unarchiveCalled++
	return nil
}

func TestUnarchiveCommandHandler_WithoutArchivedFinder_FailsLoudly(t *testing.T) {
	repo := &blindRepo{}
	h := &UnarchiveCommandHandler[*testEntity, *testCmdWithID, fwresults.None]{Repo: repo}
	cmd := &testCmdWithID{}
	cmd.SetPathID(uuid.NewString())

	_, err := h.Handle(testCtx(), cmd)

	if err == nil {
		t.Fatal("a repository that cannot load an archived aggregate must be refused, not worked around")
	}
	if !strings.Contains(err.Error(), "ArchivedFinder") {
		t.Errorf("the error must name the capability to implement, got %q", err)
	}
	if repo.unarchiveCalled != 0 {
		t.Errorf("nothing may be written when the load is impossible, got %d writes", repo.unarchiveCalled)
	}
}

// The capability being present is what keeps the happy path working — proven
// against the shared mock, which provides it exactly like a real repository.
func TestUnarchiveCommandHandler_WithArchivedFinder_LoadsArchivedScope(t *testing.T) {
	repo := newMockRepo()
	h := &UnarchiveCommandHandler[*testEntity, *testCmdWithID, fwresults.None]{Repo: repo}
	cmd := &testCmdWithID{}
	cmd.SetPathID(repo.foundData.GetID().String())

	if _, err := h.Handle(testCtx(), cmd); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if repo.findArchivedByIDCalled != 1 {
		t.Errorf("expected the archived-scope load once, got %d", repo.findArchivedByIDCalled)
	}
	if repo.findByIDCalled != 0 {
		t.Errorf("the active-scope load must not run on unarchive, got %d", repo.findByIDCalled)
	}
}
