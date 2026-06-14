package handlers

import (
	"errors"
	"testing"

	fwresults "github.com/ClaudioSchirmer/omnicore/application/results"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/google/uuid"
)

func TestUnarchiveCommandHandler_HappyPath(t *testing.T) {
	repo := newMockRepo()
	h := &UnarchiveCommandHandler[*testEntity, *testCmdWithID, fwresults.None]{
		Repo: repo,
	}

	cmd := &testCmdWithID{}
	cmd.SetPathID(uuid.NewString())

	if _, err := h.Handle(testCtx(), cmd); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	// Unarchive deliberately does NOT call FindByID (archived rows are filtered
	// by Repository.FindByID's WHERE deleted_at IS NULL convention).
	if repo.findByIDCalled != 0 {
		t.Errorf("expected FindByID NOT called, got %d", repo.findByIDCalled)
	}
	if repo.unarchiveCalled != 1 {
		t.Errorf("expected Unarchive called once, got %d", repo.unarchiveCalled)
	}
}

// mockRepoWithArchivedFinder embeds mockRepo and adds ArchivedFinder so we can
// exercise the handler path that uses FindArchivedByID for aggregate unarchive.
type mockRepoWithArchivedFinder struct {
	*mockRepo
	findArchivedCalled int
	findArchivedErr    error
	foundArchived      *testEntity
}

func (r *mockRepoWithArchivedFinder) FindArchivedByID(id domain.ID) (*testEntity, error) {
	r.findArchivedCalled++
	if r.findArchivedErr != nil {
		return nil, r.findArchivedErr
	}
	return r.foundArchived, nil
}

// LoadIncludingArchived (and therefore FindArchivedByID) returns NotFound when
// the record is active. The handler must propagate that error rather than
// fall through to a no-op Unarchive SQL.
func TestUnarchiveCommandHandler_PropagatesFindArchivedNotFound(t *testing.T) {
	notFound := domain.NotFoundError("User", "id", "active-record-uuid")
	repo := &mockRepoWithArchivedFinder{
		mockRepo:        newMockRepo(),
		findArchivedErr: notFound,
	}

	h := &UnarchiveCommandHandler[*testEntity, *testCmdWithID, fwresults.None]{
		Repo: repo,
	}

	cmd := &testCmdWithID{}
	cmd.SetPathID("active-record-uuid")

	_, err := h.Handle(testCtx(), cmd)
	if err == nil {
		t.Fatalf("expected handler to propagate NotFound from FindArchivedByID")
	}
	if !errors.Is(err, notFound) {
		t.Errorf("expected wrapped notFound error, got: %v", err)
	}
	if repo.findArchivedCalled != 1 {
		t.Errorf("expected FindArchivedByID called once, got %d", repo.findArchivedCalled)
	}
	if repo.unarchiveCalled != 0 {
		t.Errorf("expected Unarchive NOT called when finder returns error, got %d", repo.unarchiveCalled)
	}
}

// When the Repository implements ArchivedFinder and returns a valid archived
// entity, the handler proceeds to Unarchive normally.
func TestUnarchiveCommandHandler_FindArchivedHappyPath(t *testing.T) {
	id := uuid.NewString()
	archived := &testEntity{Name: "was archived"}
	archived.SetID(domain.NewID(id))
	repo := &mockRepoWithArchivedFinder{
		mockRepo:      newMockRepo(),
		foundArchived: archived,
	}

	h := &UnarchiveCommandHandler[*testEntity, *testCmdWithID, fwresults.None]{
		Repo: repo,
	}

	cmd := &testCmdWithID{}
	cmd.SetPathID(id)

	if _, err := h.Handle(testCtx(), cmd); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if repo.findArchivedCalled != 1 {
		t.Errorf("expected FindArchivedByID called once, got %d", repo.findArchivedCalled)
	}
	if repo.unarchiveCalled != 1 {
		t.Errorf("expected Unarchive called once, got %d", repo.unarchiveCalled)
	}
}
