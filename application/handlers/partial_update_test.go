package handlers

import (
	"errors"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	fwresults "github.com/ClaudioSchirmer/omnicore/application/results"
)

type testPatchCmd struct {
	pipeline.CommandBaseWithID
	NewName *string
}

func (c *testPatchCmd) ApplyPartiallyTo(_ *configuration.AppContext, e *testEntity) {
	if c.NewName != nil {
		e.Name = *c.NewName
	}
}
func (c *testPatchCmd) FromEntity(_ *configuration.AppContext, _ *testEntity) fwresults.None {
	return fwresults.None{}
}

func TestPartialUpdateCommandHandler_HappyPath(t *testing.T) {
	repo := newMockRepo()
	h := &PartialUpdateCommandHandler[*testEntity, *testPatchCmd, fwresults.None]{
		Repo: repo,
	}

	newName := "patched"
	cmd := &testPatchCmd{NewName: &newName}
	cmd.SetPathID(repo.foundData.GetID().String())

	if _, err := h.Handle(testCtx(), cmd); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if repo.findByIDCalled != 1 {
		t.Errorf("expected FindByID called once, got %d", repo.findByIDCalled)
	}
	if repo.updateCalled != 1 {
		t.Errorf("expected Update called once, got %d", repo.updateCalled)
	}
	if repo.foundData.Name != "patched" {
		t.Errorf("expected entity.Name patched, got %q", repo.foundData.Name)
	}
}

func TestPartialUpdateCommandHandler_NilFieldsAreNoOp(t *testing.T) {
	repo := newMockRepo()
	originalName := repo.foundData.Name
	h := &PartialUpdateCommandHandler[*testEntity, *testPatchCmd, fwresults.None]{
		Repo: repo,
	}

	cmd := &testPatchCmd{} // NewName nil → noop
	cmd.SetPathID(repo.foundData.GetID().String())

	if _, err := h.Handle(testCtx(), cmd); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if repo.foundData.Name != originalName {
		t.Errorf("expected name unchanged, got %q (was %q)", repo.foundData.Name, originalName)
	}
}

func TestPartialUpdateCommandHandler_FindByIDError_Propagates(t *testing.T) {
	repo := newMockRepo()
	repo.findErr = errors.New("boom")
	h := &PartialUpdateCommandHandler[*testEntity, *testPatchCmd, fwresults.None]{
		Repo: repo,
	}

	cmd := &testPatchCmd{}
	cmd.SetPathID(repo.foundData.GetID().String())

	if _, err := h.Handle(testCtx(), cmd); err == nil {
		t.Fatal("expected error, got nil")
	}
	if repo.updateCalled != 0 {
		t.Errorf("expected Update NOT called when FindByID fails, got %d", repo.updateCalled)
	}
}

func TestPartialUpdateCommandHandler_UpdateError_Propagates(t *testing.T) {
	repo := newMockRepo()
	repo.updateErr = errors.New("boom")
	h := &PartialUpdateCommandHandler[*testEntity, *testPatchCmd, fwresults.None]{
		Repo: repo,
	}

	cmd := &testPatchCmd{}
	cmd.SetPathID(repo.foundData.GetID().String())

	if _, err := h.Handle(testCtx(), cmd); err == nil {
		t.Fatal("expected error from Update to propagate")
	}
}
