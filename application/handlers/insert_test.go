package handlers

import (
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	fwresults "github.com/ClaudioSchirmer/omnicore/application/results"
)

type testInsertCmd struct {
	pipeline.CommandBase
	Name string
}

func (c testInsertCmd) ToEntity(_ *configuration.AppContext) (*testEntity, error) {
	return &testEntity{Name: c.Name}, nil
}

// FromEntity returns the entity verbatim so the test below can observe
// SetID was called after orch.Insert. Cmd-side projection is the canonical
// post-ctx pattern — symmetric with ToEntity on the input side.
func (c testInsertCmd) FromEntity(_ *configuration.AppContext, e *testEntity) (*testEntity, error) {
	return e, nil
}

// TestInsertCommandHandler_HappyPath verifies that cmd.FromEntity is called
// after orch.Insert + SetID — the test FromEntity returns the entity itself
// so the assertion can inspect its post-persistence state.
func TestInsertCommandHandler_HappyPath(t *testing.T) {
	repo := newMockRepo()
	h := &InsertCommandHandler[*testEntity, *testInsertCmd, *testEntity]{
		Repo: repo,
	}

	got, err := h.Handle(testCtx(), &testInsertCmd{Name: "alice"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil entity from cmd.FromEntity")
	}
	if got.GetID() == nil || got.GetID().IsEmpty() {
		t.Error("expected entity to have ID populated by SetID after Insert")
	}
	if repo.insertCalled != 1 {
		t.Errorf("expected Insert called once, got %d", repo.insertCalled)
	}
}

// testInsertCmdNone covers the "no projection" pattern — Cmd.FromEntity
// returns results.None{} so the success envelope renders without a "data"
// field. Bodyless Archive/Unarchive/Delete follow the same shape.
type testInsertCmdNone struct {
	pipeline.CommandBase
	Name string
}

func (c testInsertCmdNone) ToEntity(_ *configuration.AppContext) (*testEntity, error) {
	return &testEntity{Name: c.Name}, nil
}
func (c testInsertCmdNone) FromEntity(_ *configuration.AppContext, _ *testEntity) (fwresults.None, error) {
	return fwresults.None{}, nil
}

// TestInsertCommandHandler_NoneDefault proves the "no projection" pattern
// (TResult = results.None) plugs cleanly into the same Handler — only the
// generic Cmd type changes; no helper function required.
func TestInsertCommandHandler_NoneDefault(t *testing.T) {
	repo := newMockRepo()
	h := &InsertCommandHandler[*testEntity, *testInsertCmdNone, fwresults.None]{
		Repo: repo,
	}

	got, err := h.Handle(testCtx(), &testInsertCmdNone{Name: "alice"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != (fwresults.None{}) {
		t.Errorf("expected zero-value None, got %+v", got)
	}
	if repo.insertCalled != 1 {
		t.Errorf("expected Insert called once, got %d", repo.insertCalled)
	}
}
