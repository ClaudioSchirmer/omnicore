package handlers

import (
	"errors"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/application/queries"
	fwresults "github.com/ClaudioSchirmer/omnicore/application/results"
)

// This file drives the Command/Query seats' OWN failure modes through every
// Auto handler: ToEntity/ApplyTo/FromEntity on the write side, ToCriteria on
// the read side, plus the SharedBase upsert's load/merge branches. The repo
// mocks stay green so each test isolates exactly one seat returning an error
// and asserts (a) verbatim propagation and (b) that no write fires past the
// failing seat.

// failCmdWithID is testCmdWithID with injectable ApplyTo/FromEntity errors.
// One type serves Archive/Unarchive/Delete (CommandByID) AND Update
// (CommandWithBodyID) — CommandByIDBase satisfies both id-carrying contracts.
type failCmdWithID struct {
	pipeline.CommandByIDBase
	applyErr error
	fromErr  error
}

func (c *failCmdWithID) ApplyTo(_ *configuration.AppContext, _ *testEntity) error {
	return c.applyErr
}
func (c *failCmdWithID) FromEntity(_ *configuration.AppContext, _ *testEntity) (fwresults.None, error) {
	return fwresults.None{}, c.fromErr
}

// failInsertCmd is testInsertCmd with injectable ToEntity/FromEntity errors.
type failInsertCmd struct {
	pipeline.CommandBase
	toErr   error
	fromErr error
}

func (c *failInsertCmd) ToEntity(_ *configuration.AppContext) (*testEntity, error) {
	if c.toErr != nil {
		return nil, c.toErr
	}
	return &testEntity{Name: "fresh"}, nil
}
func (c *failInsertCmd) FromEntity(_ *configuration.AppContext, _ *testEntity) (fwresults.None, error) {
	return fwresults.None{}, c.fromErr
}

// failPatchCmd is testPatchCmd with an injectable FromEntity error.
type failPatchCmd struct {
	pipeline.CommandByIDBase
	fromErr error
}

func (c *failPatchCmd) ApplyPartiallyTo(_ *configuration.AppContext, _ *testEntity) error {
	return nil
}
func (c *failPatchCmd) FromEntity(_ *configuration.AppContext, _ *testEntity) (fwresults.None, error) {
	return fwresults.None{}, c.fromErr
}

// --- Archive ---

func TestArchiveCommandHandler_ApplyToError_Propagates(t *testing.T) {
	repo := newMockRepo()
	want := errors.New("apply boom")
	h := &ArchiveCommandHandler[*testEntity, *failCmdWithID, fwresults.None]{Repo: repo}

	cmd := &failCmdWithID{applyErr: want}
	cmd.SetPathID(repo.foundData.GetID().String())

	if _, err := h.Handle(testCtx(), cmd); !errors.Is(err, want) {
		t.Fatalf("expected ApplyTo error to propagate, got %v", err)
	}
	if repo.archiveCalled != 0 {
		t.Errorf("expected Archive NOT called when ApplyTo fails, got %d", repo.archiveCalled)
	}
}

func TestArchiveCommandHandler_FromEntityError_Propagates(t *testing.T) {
	repo := newMockRepo()
	want := errors.New("projection boom")
	h := &ArchiveCommandHandler[*testEntity, *failCmdWithID, fwresults.None]{Repo: repo}

	cmd := &failCmdWithID{fromErr: want}
	cmd.SetPathID(repo.foundData.GetID().String())

	if _, err := h.Handle(testCtx(), cmd); !errors.Is(err, want) {
		t.Fatalf("expected FromEntity error to propagate, got %v", err)
	}
	if repo.archiveCalled != 1 {
		t.Errorf("expected Archive called before FromEntity fails, got %d", repo.archiveCalled)
	}
}

// --- Delete ---

func TestDeleteCommandHandler_ApplyToError_Propagates(t *testing.T) {
	repo := newMockRepo()
	want := errors.New("apply boom")
	h := &DeleteCommandHandler[*testEntity, *failCmdWithID, fwresults.None]{Repo: repo}

	cmd := &failCmdWithID{applyErr: want}
	cmd.SetPathID(repo.foundData.GetID().String())

	if _, err := h.Handle(testCtx(), cmd); !errors.Is(err, want) {
		t.Fatalf("expected ApplyTo error to propagate, got %v", err)
	}
	if repo.deleteCalled != 0 {
		t.Errorf("expected Delete NOT called when ApplyTo fails, got %d", repo.deleteCalled)
	}
}

func TestDeleteCommandHandler_FromEntityError_Propagates(t *testing.T) {
	repo := newMockRepo()
	want := errors.New("projection boom")
	h := &DeleteCommandHandler[*testEntity, *failCmdWithID, fwresults.None]{Repo: repo}

	cmd := &failCmdWithID{fromErr: want}
	cmd.SetPathID(repo.foundData.GetID().String())

	if _, err := h.Handle(testCtx(), cmd); !errors.Is(err, want) {
		t.Fatalf("expected FromEntity error to propagate, got %v", err)
	}
	if repo.deleteCalled != 1 {
		t.Errorf("expected Delete called before FromEntity fails, got %d", repo.deleteCalled)
	}
}

// --- Unarchive ---

func TestUnarchiveCommandHandler_ApplyToError_Propagates(t *testing.T) {
	repo := newMockRepo()
	want := errors.New("apply boom")
	h := &UnarchiveCommandHandler[*testEntity, *failCmdWithID, fwresults.None]{Repo: repo}

	cmd := &failCmdWithID{applyErr: want}
	cmd.SetPathID(repo.foundData.GetID().String())

	if _, err := h.Handle(testCtx(), cmd); !errors.Is(err, want) {
		t.Fatalf("expected ApplyTo error to propagate, got %v", err)
	}
	if repo.unarchiveCalled != 0 {
		t.Errorf("expected Unarchive NOT called when ApplyTo fails, got %d", repo.unarchiveCalled)
	}
}

func TestUnarchiveCommandHandler_FromEntityError_Propagates(t *testing.T) {
	repo := newMockRepo()
	want := errors.New("projection boom")
	h := &UnarchiveCommandHandler[*testEntity, *failCmdWithID, fwresults.None]{Repo: repo}

	cmd := &failCmdWithID{fromErr: want}
	cmd.SetPathID(repo.foundData.GetID().String())

	if _, err := h.Handle(testCtx(), cmd); !errors.Is(err, want) {
		t.Fatalf("expected FromEntity error to propagate, got %v", err)
	}
	if repo.unarchiveCalled != 1 {
		t.Errorf("expected Unarchive called before FromEntity fails, got %d", repo.unarchiveCalled)
	}
}

// --- Update / PartialUpdate ---

func TestUpdateCommandHandler_FromEntityError_Propagates(t *testing.T) {
	repo := newMockRepo()
	want := errors.New("projection boom")
	h := &UpdateCommandHandler[*testEntity, *failCmdWithID, fwresults.None]{Repo: repo}

	cmd := &failCmdWithID{fromErr: want}
	cmd.SetPathID(repo.foundData.GetID().String())

	if _, err := h.Handle(testCtx(), cmd); !errors.Is(err, want) {
		t.Fatalf("expected FromEntity error to propagate, got %v", err)
	}
	if repo.updateCalled != 1 {
		t.Errorf("expected Update called before FromEntity fails, got %d", repo.updateCalled)
	}
}

func TestPartialUpdateCommandHandler_FromEntityError_Propagates(t *testing.T) {
	repo := newMockRepo()
	want := errors.New("projection boom")
	h := &PartialUpdateCommandHandler[*testEntity, *failPatchCmd, fwresults.None]{Repo: repo}

	cmd := &failPatchCmd{fromErr: want}
	cmd.SetPathID(repo.foundData.GetID().String())

	if _, err := h.Handle(testCtx(), cmd); !errors.Is(err, want) {
		t.Fatalf("expected FromEntity error to propagate, got %v", err)
	}
	if repo.updateCalled != 1 {
		t.Errorf("expected Update called before FromEntity fails, got %d", repo.updateCalled)
	}
}

// --- Insert ---

func TestInsertCommandHandler_ToEntityError_Propagates(t *testing.T) {
	repo := newMockRepo()
	want := errors.New("hydration boom")
	h := &InsertCommandHandler[*testEntity, *failInsertCmd, fwresults.None]{Repo: repo}

	if _, err := h.Handle(testCtx(), &failInsertCmd{toErr: want}); !errors.Is(err, want) {
		t.Fatalf("expected ToEntity error to propagate, got %v", err)
	}
	if repo.insertCalled != 0 {
		t.Errorf("expected Insert NOT called when ToEntity fails, got %d", repo.insertCalled)
	}
}

func TestInsertCommandHandler_FromEntityError_Propagates(t *testing.T) {
	repo := newMockRepo()
	want := errors.New("projection boom")
	h := &InsertCommandHandler[*testEntity, *failInsertCmd, fwresults.None]{Repo: repo}

	if _, err := h.Handle(testCtx(), &failInsertCmd{fromErr: want}); !errors.Is(err, want) {
		t.Fatalf("expected FromEntity error to propagate, got %v", err)
	}
	if repo.insertCalled != 1 {
		t.Errorf("expected Insert called before FromEntity fails, got %d", repo.insertCalled)
	}
}

// --- SharedBaseInsert ---

// failSBCmd is a SharedBaseInsertCommand that fails on a chosen ApplyTo call
// (1 = the natural-key read onto fresh, 2 = the warm merge onto the loaded
// identity) and/or at FromEntity.
type failSBCmd struct {
	pipeline.CommandBase
	applyCalls int
	failOnCall int // 0 = ApplyTo never fails
	applyErr   error
	fromErr    error
}

func (c *failSBCmd) ApplyTo(_ *configuration.AppContext, _ *testEntity) error {
	c.applyCalls++
	if c.failOnCall != 0 && c.applyCalls == c.failOnCall {
		return c.applyErr
	}
	return nil
}
func (c *failSBCmd) FromEntity(_ *configuration.AppContext, _ *testEntity) (fwresults.None, error) {
	return fwresults.None{}, c.fromErr
}

// failSharedBaseRepo is mockSharedBaseRepo with an injectable load error.
type failSharedBaseRepo struct {
	*mockRepo
	existed bool
	loaded  *testEntity
	loadErr error
}

func (r *failSharedBaseRepo) LoadForSharedBaseInsert(_ *configuration.AppContext, fresh *testEntity) (*testEntity, bool, error) {
	if r.loadErr != nil {
		return nil, false, r.loadErr
	}
	if r.existed {
		return r.loaded, true, nil
	}
	return fresh, false, nil
}

func TestSharedBaseInsertHandler_FreshApplyToError_Propagates(t *testing.T) {
	repo := &failSharedBaseRepo{mockRepo: newMockRepo()}
	want := errors.New("nk apply boom")
	cmd := &failSBCmd{failOnCall: 1, applyErr: want}
	h := &SharedBaseInsertCommandHandler[*testEntity, *failSBCmd, fwresults.None]{Repo: repo}

	if _, err := h.Handle(testCtx(), cmd); !errors.Is(err, want) {
		t.Fatalf("expected fresh ApplyTo error to propagate, got %v", err)
	}
	if repo.insertCalled != 0 {
		t.Errorf("expected Insert NOT called when the nk read fails, got %d", repo.insertCalled)
	}
}

func TestSharedBaseInsertHandler_LoadError_Propagates(t *testing.T) {
	want := errors.New("load boom")
	repo := &failSharedBaseRepo{mockRepo: newMockRepo(), loadErr: want}
	h := &SharedBaseInsertCommandHandler[*testEntity, *failSBCmd, fwresults.None]{Repo: repo}

	if _, err := h.Handle(testCtx(), &failSBCmd{}); !errors.Is(err, want) {
		t.Fatalf("expected LoadForSharedBaseInsert error to propagate, got %v", err)
	}
	if repo.insertCalled != 0 {
		t.Errorf("expected Insert NOT called when the load fails, got %d", repo.insertCalled)
	}
}

func TestSharedBaseInsertHandler_WarmApplyToError_Propagates(t *testing.T) {
	repo := &failSharedBaseRepo{mockRepo: newMockRepo(), existed: true, loaded: &testEntity{Name: "loaded"}}
	want := errors.New("merge boom")
	cmd := &failSBCmd{failOnCall: 2, applyErr: want}
	h := &SharedBaseInsertCommandHandler[*testEntity, *failSBCmd, fwresults.None]{Repo: repo}

	if _, err := h.Handle(testCtx(), cmd); !errors.Is(err, want) {
		t.Fatalf("expected warm-merge ApplyTo error to propagate, got %v", err)
	}
	if cmd.applyCalls != 2 {
		t.Errorf("expected the failure on the second ApplyTo (warm merge), got %d calls", cmd.applyCalls)
	}
	if repo.insertCalled != 0 {
		t.Errorf("expected Insert NOT called when the merge fails, got %d", repo.insertCalled)
	}
}

func TestSharedBaseInsertHandler_InsertError_Propagates(t *testing.T) {
	want := errors.New("insert boom")
	inner := newMockRepo()
	inner.insertErr = want
	repo := &failSharedBaseRepo{mockRepo: inner}
	h := &SharedBaseInsertCommandHandler[*testEntity, *failSBCmd, fwresults.None]{Repo: repo}

	if _, err := h.Handle(testCtx(), &failSBCmd{}); !errors.Is(err, want) {
		t.Fatalf("expected Insert error to propagate, got %v", err)
	}
	if repo.insertCalled != 1 {
		t.Errorf("expected exactly one Insert attempt, got %d", repo.insertCalled)
	}
}

func TestSharedBaseInsertHandler_FromEntityError_Propagates(t *testing.T) {
	repo := &failSharedBaseRepo{mockRepo: newMockRepo()}
	want := errors.New("projection boom")
	h := &SharedBaseInsertCommandHandler[*testEntity, *failSBCmd, fwresults.None]{Repo: repo}

	if _, err := h.Handle(testCtx(), &failSBCmd{fromErr: want}); !errors.Is(err, want) {
		t.Fatalf("expected FromEntity error to propagate, got %v", err)
	}
	if repo.insertCalled != 1 {
		t.Errorf("expected Insert called before FromEntity fails, got %d", repo.insertCalled)
	}
}

// badSharedBaseRepo marries badRepo (no-mode entities) with the SharedBase
// loader so the handler reaches domain.GetInsertable and fails validation.
type badSharedBaseRepo struct {
	*badRepo
}

func (r *badSharedBaseRepo) LoadForSharedBaseInsert(_ *configuration.AppContext, fresh *noModeEntity) (*noModeEntity, bool, error) {
	return fresh, false, nil
}

type badSBCmd struct {
	pipeline.CommandBase
}

func (c *badSBCmd) ApplyTo(_ *configuration.AppContext, _ *noModeEntity) error { return nil }
func (c *badSBCmd) FromEntity(_ *configuration.AppContext, _ *noModeEntity) (fwresults.None, error) {
	return fwresults.None{}, nil
}

func TestSharedBaseInsertHandler_ValidationError(t *testing.T) {
	repo := &badSharedBaseRepo{badRepo: newBadRepo()}
	h := &SharedBaseInsertCommandHandler[*noModeEntity, *badSBCmd, fwresults.None]{Repo: repo}
	if _, err := h.Handle(testCtx(), &badSBCmd{}); err == nil {
		t.Fatal("expected validation error from GetInsertable on the shared-base upsert")
	}
}

// --- read side: ToCriteria failures ---

// failFindParamsQuery fails ToCriteria — identity-overlay code in the Query
// seat is the only thing that can error before the reader runs.
type failFindParamsQuery struct {
	pipeline.QueryBase
	err error
}

func (q *failFindParamsQuery) ToCriteria(*configuration.AppContext) (queries.ReadCriteria, error) {
	return queries.ReadCriteria{}, q.err
}

type failFindIDQuery struct {
	queries.QueryByIDBase
	err error
}

func (q *failFindIDQuery) ToCriteria(*configuration.AppContext) (queries.ReadCriteria, error) {
	return queries.ReadCriteria{}, q.err
}
func (failFindIDQuery) ContextName() string { return "" }

func TestFindByParamsQueryHandler_ToCriteriaError_Propagates(t *testing.T) {
	want := errors.New("overlay boom")
	reader := &spyReader{}
	h := &FindByParamsQueryHandler[*failFindParamsQuery]{Reader: reader, View: "users"}

	_, err := h.Handle(testCtx(), &failFindParamsQuery{err: want})
	if !errors.Is(err, want) {
		t.Fatalf("expected ToCriteria error to propagate, got %v", err)
	}
	if reader.readPageCalled != 0 {
		t.Errorf("expected ReadPage NOT called when ToCriteria fails, got %d", reader.readPageCalled)
	}
}

func TestFindByIDQueryHandler_ToCriteriaError_Propagates(t *testing.T) {
	want := errors.New("overlay boom")
	reader := &spyReader{}
	h := &FindByIDQueryHandler[*failFindIDQuery]{Reader: reader, View: "users"}

	q := &failFindIDQuery{err: want}
	q.SetPathID("abc")

	_, err := h.Handle(testCtx(), q)
	if !errors.Is(err, want) {
		t.Fatalf("expected ToCriteria error to propagate, got %v", err)
	}
	if reader.readByIDCalled != 0 {
		t.Errorf("expected ReadByID NOT called when ToCriteria fails, got %d", reader.readByIDCalled)
	}
}
