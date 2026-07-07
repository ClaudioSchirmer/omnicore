package handlers

import (
	"errors"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/persistence"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	fwresults "github.com/ClaudioSchirmer/omnicore/application/results"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/google/uuid"
)

// noModeEntity returns an empty Modes() set, so every domain.Get* validation
// pass fails (the requested mode is never allowed). Used to drive the
// "validation error" branch of every Auto handler without a live DB.
type noModeEntity struct {
	domain.BaseEntity
	Name string
}

func (e *noModeEntity) Modes() []domain.EntityMode                       { return nil }
func (e *noModeEntity) RequiresService() bool                            { return false }
func (e *noModeEntity) BuildRules(string, domain.Service, *domain.Rules) {}

// badRepo is a ScopedRepository[*noModeEntity]. FindByID/New return a
// no-mode entity so the handler reaches domain.Get* and fails validation
// before any write. The writer methods exist only to satisfy domain.Writer.
type badRepo struct {
	found *noModeEntity
}

func newBadRepo() *badRepo {
	e := &noModeEntity{Name: "bad"}
	e.SetID(domain.NewID(uuid.NewString()))
	return &badRepo{found: e}
}

func (r *badRepo) FindByID(domain.ID) (*noModeEntity, error) { return r.found, nil }
func (r *badRepo) New() *noModeEntity                        { return &noModeEntity{} }
func (r *badRepo) Scope(_ *configuration.AppContext, _ ...persistence.WriteOption[*noModeEntity]) domain.Writer {
	return &badWriter{}
}

type badWriter struct{}

func (badWriter) Insert(domain.Insertable) (domain.ID, error) { return domain.ID{}, nil }
func (badWriter) Update(domain.Updatable) error               { return nil }
func (badWriter) Delete(domain.Deletable) error               { return nil }
func (badWriter) Archive(domain.Archivable) error             { return nil }
func (badWriter) Unarchive(domain.Unarchivable) error         { return nil }

// --- commands over noModeEntity ---

type badInsertCmd struct {
	pipeline.CommandBase
	Name string
}

func (c badInsertCmd) ToEntity(_ *configuration.AppContext) (*noModeEntity, error) {
	return &noModeEntity{Name: c.Name}, nil
}
func (c badInsertCmd) FromEntity(_ *configuration.AppContext, _ *noModeEntity) (fwresults.None, error) {
	return fwresults.None{}, nil
}

type badIDCmd struct {
	pipeline.CommandByIDBase
}

func (c *badIDCmd) ApplyTo(_ *configuration.AppContext, _ *noModeEntity) error { return nil }
func (c *badIDCmd) FromEntity(_ *configuration.AppContext, _ *noModeEntity) (fwresults.None, error) {
	return fwresults.None{}, nil
}

type badPatchCmd struct {
	pipeline.CommandByIDBase
}

func (c *badPatchCmd) ApplyPartiallyTo(_ *configuration.AppContext, _ *noModeEntity) error {
	return nil
}
func (c *badPatchCmd) FromEntity(_ *configuration.AppContext, _ *noModeEntity) (fwresults.None, error) {
	return fwresults.None{}, nil
}

// --- validation-error branch coverage (domain.Get* returns error) ---

func TestInsertCommandHandler_ValidationError(t *testing.T) {
	h := &InsertCommandHandler[*noModeEntity, *badInsertCmd, fwresults.None]{Repo: newBadRepo()}
	if _, err := h.Handle(testCtx(), &badInsertCmd{Name: "x"}); err == nil {
		t.Fatal("expected validation error from GetInsertable")
	}
}

func TestUpdateCommandHandler_ValidationError(t *testing.T) {
	repo := newBadRepo()
	h := &UpdateCommandHandler[*noModeEntity, *badIDCmd, fwresults.None]{Repo: repo}
	cmd := &badIDCmd{}
	cmd.SetPathID(repo.found.GetID().String())
	if _, err := h.Handle(testCtx(), cmd); err == nil {
		t.Fatal("expected validation error from GetUpdatable")
	}
}

func TestPartialUpdateCommandHandler_ValidationError(t *testing.T) {
	repo := newBadRepo()
	h := &PartialUpdateCommandHandler[*noModeEntity, *badPatchCmd, fwresults.None]{Repo: repo}
	cmd := &badPatchCmd{}
	cmd.SetPathID(repo.found.GetID().String())
	if _, err := h.Handle(testCtx(), cmd); err == nil {
		t.Fatal("expected validation error from GetPartialUpdatable")
	}
}

func TestArchiveCommandHandler_ValidationError(t *testing.T) {
	repo := newBadRepo()
	h := &ArchiveCommandHandler[*noModeEntity, *badIDCmd, fwresults.None]{Repo: repo}
	cmd := &badIDCmd{}
	cmd.SetPathID(repo.found.GetID().String())
	if _, err := h.Handle(testCtx(), cmd); err == nil {
		t.Fatal("expected validation error from GetArchivable")
	}
}

func TestDeleteCommandHandler_ValidationError(t *testing.T) {
	repo := newBadRepo()
	h := &DeleteCommandHandler[*noModeEntity, *badIDCmd, fwresults.None]{Repo: repo}
	cmd := &badIDCmd{}
	cmd.SetPathID(repo.found.GetID().String())
	if _, err := h.Handle(testCtx(), cmd); err == nil {
		t.Fatal("expected validation error from GetDeletable")
	}
}

func TestUnarchiveCommandHandler_ValidationError_NewFallback(t *testing.T) {
	// badRepo does NOT implement domain.ArchivedFinder, so the handler takes
	// the Repo.New()+SetID fallback, then GetUnarchivable fails (no mode).
	repo := newBadRepo()
	h := &UnarchiveCommandHandler[*noModeEntity, *badIDCmd, fwresults.None]{Repo: repo}
	cmd := &badIDCmd{}
	cmd.SetPathID(repo.found.GetID().String())
	if _, err := h.Handle(testCtx(), cmd); err == nil {
		t.Fatal("expected validation error from GetUnarchivable")
	}
}

// --- FindByID error propagation ---

func TestArchiveCommandHandler_FindByIDError(t *testing.T) {
	repo := newMockRepo()
	repo.findErr = errors.New("boom")
	h := &ArchiveCommandHandler[*testEntity, *testCmdWithID, fwresults.None]{Repo: repo}
	cmd := &testCmdWithID{}
	cmd.SetPathID(uuid.NewString())
	if _, err := h.Handle(testCtx(), cmd); err == nil {
		t.Fatal("expected FindByID error to propagate")
	}
}

func TestDeleteCommandHandler_FindByIDError(t *testing.T) {
	repo := newMockRepo()
	repo.findErr = errors.New("boom")
	h := &DeleteCommandHandler[*testEntity, *testCmdWithID, fwresults.None]{Repo: repo}
	cmd := &testCmdWithID{}
	cmd.SetPathID(uuid.NewString())
	if _, err := h.Handle(testCtx(), cmd); err == nil {
		t.Fatal("expected FindByID error to propagate")
	}
}

func TestUpdateCommandHandler_FindByIDError(t *testing.T) {
	repo := newMockRepo()
	repo.findErr = errors.New("boom")
	h := &UpdateCommandHandler[*testEntity, *testUpdateCmd, fwresults.None]{Repo: repo}
	cmd := &testUpdateCmd{}
	cmd.SetPathID(uuid.NewString())
	if _, err := h.Handle(testCtx(), cmd); err == nil {
		t.Fatal("expected FindByID error to propagate")
	}
}

// --- write error propagation ---

func TestInsertCommandHandler_InsertError(t *testing.T) {
	repo := newMockRepo()
	repo.insertErr = errors.New("dup")
	h := &InsertCommandHandler[*testEntity, *testInsertCmd, *testEntity]{Repo: repo}
	if _, err := h.Handle(testCtx(), &testInsertCmd{Name: "a"}); err == nil {
		t.Fatal("expected Insert error to propagate")
	}
}

func TestArchiveCommandHandler_ArchiveError(t *testing.T) {
	repo := newMockRepo()
	repo.archiveErr = errors.New("fail")
	h := &ArchiveCommandHandler[*testEntity, *testCmdWithID, fwresults.None]{Repo: repo}
	cmd := &testCmdWithID{}
	cmd.SetPathID(repo.foundData.GetID().String())
	if _, err := h.Handle(testCtx(), cmd); err == nil {
		t.Fatal("expected Archive error to propagate")
	}
}

func TestDeleteCommandHandler_DeleteError(t *testing.T) {
	repo := newMockRepo()
	repo.deleteErr = errors.New("fail")
	h := &DeleteCommandHandler[*testEntity, *testCmdWithID, fwresults.None]{Repo: repo}
	cmd := &testCmdWithID{}
	cmd.SetPathID(repo.foundData.GetID().String())
	if _, err := h.Handle(testCtx(), cmd); err == nil {
		t.Fatal("expected Delete error to propagate")
	}
}

func TestUpdateCommandHandler_UpdateError(t *testing.T) {
	repo := newMockRepo()
	repo.updateErr = errors.New("fail")
	h := &UpdateCommandHandler[*testEntity, *testUpdateCmd, fwresults.None]{Repo: repo}
	cmd := &testUpdateCmd{Name: "z"}
	cmd.SetPathID(repo.foundData.GetID().String())
	if _, err := h.Handle(testCtx(), cmd); err == nil {
		t.Fatal("expected Update error to propagate")
	}
}

func TestUnarchiveCommandHandler_UnarchiveError(t *testing.T) {
	repo := newMockRepo()
	repo.unarchiveErr = errors.New("fail")
	h := &UnarchiveCommandHandler[*testEntity, *testCmdWithID, fwresults.None]{Repo: repo}
	cmd := &testCmdWithID{}
	cmd.SetPathID(uuid.NewString())
	if _, err := h.Handle(testCtx(), cmd); err == nil {
		t.Fatal("expected Unarchive error to propagate")
	}
}

// --- RequirePathID panic branch (empty path ID) ---

func TestAutoHandlers_RequirePathIDPanics(t *testing.T) {
	cases := []struct {
		name string
		run  func()
	}{
		{"archive", func() {
			h := &ArchiveCommandHandler[*testEntity, *testCmdWithID, fwresults.None]{Repo: newMockRepo()}
			_, _ = h.Handle(testCtx(), &testCmdWithID{})
		}},
		{"delete", func() {
			h := &DeleteCommandHandler[*testEntity, *testCmdWithID, fwresults.None]{Repo: newMockRepo()}
			_, _ = h.Handle(testCtx(), &testCmdWithID{})
		}},
		{"update", func() {
			h := &UpdateCommandHandler[*testEntity, *testUpdateCmd, fwresults.None]{Repo: newMockRepo()}
			_, _ = h.Handle(testCtx(), &testUpdateCmd{})
		}},
		{"partial", func() {
			h := &PartialUpdateCommandHandler[*testEntity, *testPatchCmd, fwresults.None]{Repo: newMockRepo()}
			_, _ = h.Handle(testCtx(), &testPatchCmd{})
		}},
		{"unarchive", func() {
			h := &UnarchiveCommandHandler[*testEntity, *testCmdWithID, fwresults.None]{Repo: newMockRepo()}
			_, _ = h.Handle(testCtx(), &testCmdWithID{})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("%s: expected panic on empty path ID", tc.name)
				}
			}()
			tc.run()
		})
	}
}
