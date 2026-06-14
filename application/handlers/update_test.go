package handlers

import (
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	fwresults "github.com/ClaudioSchirmer/omnicore/application/results"
)

type testUpdateCmd struct {
	pipeline.CommandBaseWithID
	Name string
}

func (c *testUpdateCmd) ApplyTo(_ *configuration.AppContext, e *testEntity) { e.Name = c.Name }
func (c *testUpdateCmd) FromEntity(_ *configuration.AppContext, _ *testEntity) fwresults.None {
	return fwresults.None{}
}

func TestUpdateCommandHandler_HappyPath(t *testing.T) {
	repo := newMockRepo()
	h := &UpdateCommandHandler[*testEntity, *testUpdateCmd, fwresults.None]{
		Repo: repo,
	}

	cmd := &testUpdateCmd{Name: "bob"}
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
	if repo.foundData.Name != "bob" {
		t.Errorf("expected entity.Name to be applied to 'bob', got %q", repo.foundData.Name)
	}
}

// testUpdateCmdString covers the "return a value from FromEntity" pattern —
// FromEntity has the post-update entity AND access to cmd (via receiver),
// so the projection can observe whatever it needs.
type testUpdateCmdString struct {
	pipeline.CommandBaseWithID
	Name string

	seen string // captured at FromEntity time so the test asserts ctx symmetry
}

func (c *testUpdateCmdString) ApplyTo(_ *configuration.AppContext, e *testEntity) { e.Name = c.Name }
func (c *testUpdateCmdString) FromEntity(_ *configuration.AppContext, e *testEntity) string {
	c.seen = e.Name
	return e.Name
}

// TestUpdateCommandHandler_FromEntityReceivesPostApplyEntity proves cmd.FromEntity
// receives the entity AFTER ApplyTo ran through GetUpdatable, so the projected
// value reflects the post-mutation state.
func TestUpdateCommandHandler_FromEntityReceivesPostApplyEntity(t *testing.T) {
	repo := newMockRepo()
	h := &UpdateCommandHandler[*testEntity, *testUpdateCmdString, string]{
		Repo: repo,
	}

	cmd := &testUpdateCmdString{Name: "renamed"}
	cmd.SetPathID(repo.foundData.GetID().String())

	got, err := h.Handle(testCtx(), cmd)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != "renamed" {
		t.Errorf("expected handler return = renamed, got %q", got)
	}
	if cmd.seen != "renamed" {
		t.Errorf("expected FromEntity to see post-ApplyTo state, got %q", cmd.seen)
	}
}
