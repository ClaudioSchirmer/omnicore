package handlers

import (
	"testing"

	fwresults "github.com/ClaudioSchirmer/omnicore/application/results"
)

func TestDeleteCommandHandler_HappyPath(t *testing.T) {
	repo := newMockRepo()
	h := &DeleteCommandHandler[*testEntity, *testCmdWithID, fwresults.None]{
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
	if repo.deleteCalled != 1 {
		t.Errorf("expected Delete called once, got %d", repo.deleteCalled)
	}
}
