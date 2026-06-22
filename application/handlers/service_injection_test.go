package handlers

import (
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	fwresults "github.com/ClaudioSchirmer/omnicore/application/results"
)

// The 6 Auto handlers expose a Service field. For Insert/Update/PartialUpdate/Delete,
// BuildRules runs and the framework propagates h.Service into the entity's BuildRules
// (captured in testEntity.BuildRulesSeenService). For Archive/Unarchive, BuildRules
// does NOT run (state transitions) — we only verify that setting Service does not
// regress the happy path; it is future plumbing (cf. comment in the handler).
//
// Nil-default: handlers without Service set keep working — BuildRules
// receives nil, behavior identical to the pre-change API.

// capturingInsertCmd lets the test hold a reference to the entity the handler
// will insert (cmd.ToEntity normally creates a new entity per call).
type capturingInsertCmd struct {
	pipeline.CommandBase
	entity *testEntity
}

func (c *capturingInsertCmd) ToEntity(_ *configuration.AppContext) (*testEntity, error) {
	return c.entity, nil
}
func (c *capturingInsertCmd) FromEntity(_ *configuration.AppContext, _ *testEntity) (fwresults.None, error) {
	return fwresults.None{}, nil
}

// ─── Insert ──────────────────────────────────────────────────────────────────

func TestInsertCommandHandler_PassesServiceToBuildRules(t *testing.T) {
	repo := newMockRepo()
	svc := &testService{}
	entity := &testEntity{Name: "with-service"}
	h := &InsertCommandHandler[*testEntity, *capturingInsertCmd, fwresults.None]{
		Repo: repo, Service: svc,
	}

	if _, err := h.Handle(testCtx(), &capturingInsertCmd{entity: entity}); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !entity.BuildRulesCalled {
		t.Fatal("expected BuildRules to be invoked during Insert")
	}
	if entity.BuildRulesSeenService != svc {
		t.Errorf("expected h.Service to reach BuildRules; got %v", entity.BuildRulesSeenService)
	}
}

func TestInsertCommandHandler_NilServiceIsBackwardsCompat(t *testing.T) {
	repo := newMockRepo()
	entity := &testEntity{Name: "no-service"}
	h := &InsertCommandHandler[*testEntity, *capturingInsertCmd, fwresults.None]{
		Repo: repo,
	}

	if _, err := h.Handle(testCtx(), &capturingInsertCmd{entity: entity}); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !entity.BuildRulesCalled {
		t.Fatal("expected BuildRules to be invoked during Insert")
	}
	if entity.BuildRulesSeenService != nil {
		t.Errorf("expected nil service in BuildRules when handler has no Service; got %v", entity.BuildRulesSeenService)
	}
}

// ─── Update (PUT) ────────────────────────────────────────────────────────────

func TestUpdateCommandHandler_PassesServiceToBuildRules(t *testing.T) {
	repo := newMockRepo()
	svc := &testService{}
	h := &UpdateCommandHandler[*testEntity, *testUpdateCmd, fwresults.None]{
		Repo: repo, Service: svc,
	}

	cmd := &testUpdateCmd{Name: "renamed"}
	cmd.SetPathID(repo.foundData.GetID().String())

	if _, err := h.Handle(testCtx(), cmd); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !repo.foundData.BuildRulesCalled {
		t.Fatal("expected BuildRules to be invoked during Update")
	}
	if repo.foundData.BuildRulesSeenService != svc {
		t.Errorf("expected h.Service to reach BuildRules; got %v", repo.foundData.BuildRulesSeenService)
	}
}

func TestUpdateCommandHandler_NilServiceIsBackwardsCompat(t *testing.T) {
	repo := newMockRepo()
	h := &UpdateCommandHandler[*testEntity, *testUpdateCmd, fwresults.None]{
		Repo: repo,
	}

	cmd := &testUpdateCmd{Name: "renamed"}
	cmd.SetPathID(repo.foundData.GetID().String())

	if _, err := h.Handle(testCtx(), cmd); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if repo.foundData.BuildRulesSeenService != nil {
		t.Errorf("expected nil service when handler has no Service; got %v", repo.foundData.BuildRulesSeenService)
	}
}

// ─── PartialUpdate (PATCH) ───────────────────────────────────────────────────

func TestPartialUpdateCommandHandler_PassesServiceToBuildRules(t *testing.T) {
	repo := newMockRepo()
	svc := &testService{}
	h := &PartialUpdateCommandHandler[*testEntity, *testPatchCmd, fwresults.None]{
		Repo: repo, Service: svc,
	}

	newName := "patched"
	cmd := &testPatchCmd{NewName: &newName}
	cmd.SetPathID(repo.foundData.GetID().String())

	if _, err := h.Handle(testCtx(), cmd); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !repo.foundData.BuildRulesCalled {
		t.Fatal("expected BuildRules to be invoked during PartialUpdate")
	}
	if repo.foundData.BuildRulesSeenService != svc {
		t.Errorf("expected h.Service to reach BuildRules; got %v", repo.foundData.BuildRulesSeenService)
	}
}

func TestPartialUpdateCommandHandler_NilServiceIsBackwardsCompat(t *testing.T) {
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
	if repo.foundData.BuildRulesSeenService != nil {
		t.Errorf("expected nil service when handler has no Service; got %v", repo.foundData.BuildRulesSeenService)
	}
}

// ─── Delete ──────────────────────────────────────────────────────────────────

func TestDeleteCommandHandler_PassesServiceToBuildRules(t *testing.T) {
	repo := newMockRepo()
	svc := &testService{}
	h := &DeleteCommandHandler[*testEntity, *testCmdWithID, fwresults.None]{
		Repo: repo, Service: svc,
	}

	cmd := &testCmdWithID{}
	cmd.SetPathID(repo.foundData.GetID().String())

	if _, err := h.Handle(testCtx(), cmd); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !repo.foundData.BuildRulesCalled {
		t.Fatal("expected BuildRules to be invoked during Delete (ModeDelete branch via r.IfDelete)")
	}
	if repo.foundData.BuildRulesSeenService != svc {
		t.Errorf("expected h.Service to reach BuildRules; got %v", repo.foundData.BuildRulesSeenService)
	}
}

func TestDeleteCommandHandler_NilServiceIsBackwardsCompat(t *testing.T) {
	repo := newMockRepo()
	h := &DeleteCommandHandler[*testEntity, *testCmdWithID, fwresults.None]{
		Repo: repo,
	}

	cmd := &testCmdWithID{}
	cmd.SetPathID(repo.foundData.GetID().String())

	if _, err := h.Handle(testCtx(), cmd); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if repo.foundData.BuildRulesSeenService != nil {
		t.Errorf("expected nil service when handler has no Service; got %v", repo.foundData.BuildRulesSeenService)
	}
}

// ─── Archive ─────────────────────────────────────────────────────────────────
//
// Archive/Unarchive do not run BuildRules — state transitions, no field
// mutation. We only verify that setting Service preserves the happy path
// (smoke test for plumbing; the concrete value is not consumed today).

func TestArchiveCommandHandler_ServiceFieldPreservesHappyPath(t *testing.T) {
	repo := newMockRepo()
	svc := &testService{}
	h := &ArchiveCommandHandler[*testEntity, *testCmdWithID, fwresults.None]{
		Repo: repo, Service: svc,
	}

	cmd := &testCmdWithID{}
	cmd.SetPathID(repo.foundData.GetID().String())

	if _, err := h.Handle(testCtx(), cmd); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if repo.archiveCalled != 1 {
		t.Errorf("expected Archive called once, got %d", repo.archiveCalled)
	}
}

// ─── Unarchive ───────────────────────────────────────────────────────────────

func TestUnarchiveCommandHandler_ServiceFieldPreservesHappyPath(t *testing.T) {
	repo := newMockRepo()
	svc := &testService{}
	h := &UnarchiveCommandHandler[*testEntity, *testCmdWithID, fwresults.None]{
		Repo: repo, Service: svc,
	}

	cmd := &testCmdWithID{}
	cmd.SetPathID(repo.foundData.GetID().String())

	if _, err := h.Handle(testCtx(), cmd); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if repo.unarchiveCalled != 1 {
		t.Errorf("expected Unarchive called once, got %d", repo.unarchiveCalled)
	}
}
