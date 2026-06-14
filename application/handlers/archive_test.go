package handlers

import (
	"testing"

	fwresults "github.com/ClaudioSchirmer/omnicore/application/results"
)

func TestArchiveCommandHandler_HappyPath(t *testing.T) {
	repo := newMockRepo()
	h := &ArchiveCommandHandler[*testEntity, *testCmdWithID, fwresults.None]{
		Repo: repo,
	}

	cmd := &testCmdWithID{}
	cmd.SetPathID(repo.foundData.GetID().String())

	if _, err := h.Handle(testCtx(), cmd); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if repo.findByIDCalled != 1 {
		t.Errorf("expected FindByID called once, got %d", repo.findByIDCalled)
	}
	if repo.archiveCalled != 1 {
		t.Errorf("expected Archive called once, got %d", repo.archiveCalled)
	}
}
