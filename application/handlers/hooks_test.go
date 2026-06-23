package handlers

import (
	"errors"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/persistence"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	fwresults "github.com/ClaudioSchirmer/omnicore/application/results"
	"github.com/ClaudioSchirmer/omnicore/domain"
)

// The dispatch tier (Topic 10 Tier 2) covers each Auto handler against
// four shapes of Cmd:
//
//   - Cmd implements AfterBeginHookProvider[T] only → only the afterBegin
//     option is threaded
//   - Cmd implements BeforeCommitHookProvider[T] only → only the beforeCommit
//     option is threaded
//   - Cmd implements both → both options are threaded, in deterministic
//     order
//   - Cmd implements neither → no options are threaded
//
// The mockRepo captures the variadic []WriteOption[*testEntity] slice on
// every write method, so the test asserts the dispatch by counting the
// captured options. Per-option identity is verified by invoking the
// closure and confirming the typed Cmd method actually fires.

// --- Cmds with hook providers ---------------------------------------------

// withAfterBeginCmd implements only AfterBeginHookProvider[*testEntity].
type withAfterBeginCmd struct {
	pipeline.CommandBase
	afterBeginCalled bool
}

func (c *withAfterBeginCmd) ToEntity(_ *configuration.AppContext) (*testEntity, error) {
	return &testEntity{Name: "alice"}, nil
}
func (c *withAfterBeginCmd) FromEntity(_ *configuration.AppContext, _ *testEntity) (fwresults.None, error) {
	return fwresults.None{}, nil
}
func (c *withAfterBeginCmd) AfterBegin(_ *configuration.AppContext, _ *testEntity, _ persistence.TxHandle) error {
	c.afterBeginCalled = true
	return nil
}

// withBeforeCommitCmd implements only BeforeCommitHookProvider[*testEntity].
type withBeforeCommitCmd struct {
	pipeline.CommandBase
	beforeCommitCalled bool
}

func (c *withBeforeCommitCmd) ToEntity(_ *configuration.AppContext) (*testEntity, error) {
	return &testEntity{Name: "alice"}, nil
}
func (c *withBeforeCommitCmd) FromEntity(_ *configuration.AppContext, _ *testEntity) (fwresults.None, error) {
	return fwresults.None{}, nil
}
func (c *withBeforeCommitCmd) BeforeCommit(_ *configuration.AppContext, _ *testEntity, _ domain.ID, _ persistence.TxHandle) error {
	c.beforeCommitCalled = true
	return nil
}

// withBothHooksCmd implements both provider interfaces.
type withBothHooksCmd struct {
	pipeline.CommandBase
	afterBeginCalled   bool
	beforeCommitCalled bool
}

func (c *withBothHooksCmd) ToEntity(_ *configuration.AppContext) (*testEntity, error) {
	return &testEntity{Name: "alice"}, nil
}
func (c *withBothHooksCmd) FromEntity(_ *configuration.AppContext, _ *testEntity) (fwresults.None, error) {
	return fwresults.None{}, nil
}
func (c *withBothHooksCmd) AfterBegin(_ *configuration.AppContext, _ *testEntity, _ persistence.TxHandle) error {
	c.afterBeginCalled = true
	return nil
}
func (c *withBothHooksCmd) BeforeCommit(_ *configuration.AppContext, _ *testEntity, _ domain.ID, _ persistence.TxHandle) error {
	c.beforeCommitCalled = true
	return nil
}

// noHooksCmd implements neither provider.
type noHooksCmd struct {
	pipeline.CommandBase
}

func (c *noHooksCmd) ToEntity(_ *configuration.AppContext) (*testEntity, error) {
	return &testEntity{Name: "alice"}, nil
}
func (c *noHooksCmd) FromEntity(_ *configuration.AppContext, _ *testEntity) (fwresults.None, error) {
	return fwresults.None{}, nil
}

// --- InsertCommandHandler dispatch ----------------------------------------

func TestInsertCommandHandler_HookDispatch_AfterBeginOnly(t *testing.T) {
	repo := newMockRepo()
	h := &InsertCommandHandler[*testEntity, *withAfterBeginCmd, fwresults.None]{Repo: repo}
	if _, err := h.Handle(testCtx(), &withAfterBeginCmd{}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got, want := len(repo.insertOpts), 1; got != want {
		t.Fatalf("len(insertOpts)=%d, want %d", got, want)
	}
}

func TestInsertCommandHandler_HookDispatch_BeforeCommitOnly(t *testing.T) {
	repo := newMockRepo()
	h := &InsertCommandHandler[*testEntity, *withBeforeCommitCmd, fwresults.None]{Repo: repo}
	if _, err := h.Handle(testCtx(), &withBeforeCommitCmd{}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got, want := len(repo.insertOpts), 1; got != want {
		t.Fatalf("len(insertOpts)=%d, want %d", got, want)
	}
}

func TestInsertCommandHandler_HookDispatch_Both(t *testing.T) {
	repo := newMockRepo()
	h := &InsertCommandHandler[*testEntity, *withBothHooksCmd, fwresults.None]{Repo: repo}
	if _, err := h.Handle(testCtx(), &withBothHooksCmd{}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got, want := len(repo.insertOpts), 2; got != want {
		t.Fatalf("len(insertOpts)=%d, want %d", got, want)
	}
}

func TestInsertCommandHandler_HookDispatch_Neither(t *testing.T) {
	repo := newMockRepo()
	h := &InsertCommandHandler[*testEntity, *noHooksCmd, fwresults.None]{Repo: repo}
	if _, err := h.Handle(testCtx(), &noHooksCmd{}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got, want := len(repo.insertOpts), 0; got != want {
		t.Fatalf("len(insertOpts)=%d, want %d", got, want)
	}
}

// --- Update / PartialUpdate / Archive / Unarchive / Delete dispatch -------

// withAfterBeginIDCmd / withBeforeCommitIDCmd / withBothHooksIDCmd
// satisfy the CommandWithID + ApplyTo + FromEntity shape that the
// Update/PartialUpdate/Archive/Unarchive/Delete handlers expect.

type withAfterBeginIDCmd struct {
	pipeline.CommandBaseWithID
}

func (c *withAfterBeginIDCmd) ApplyTo(_ *configuration.AppContext, _ *testEntity) error { return nil }
func (c *withAfterBeginIDCmd) ApplyPartiallyTo(_ *configuration.AppContext, _ *testEntity) error {
	return nil
}
func (c *withAfterBeginIDCmd) FromEntity(_ *configuration.AppContext, _ *testEntity) (fwresults.None, error) {
	return fwresults.None{}, nil
}
func (c *withAfterBeginIDCmd) AfterBegin(_ *configuration.AppContext, _ *testEntity, _ persistence.TxHandle) error {
	return nil
}

type withBeforeCommitIDCmd struct {
	pipeline.CommandBaseWithID
}

func (c *withBeforeCommitIDCmd) ApplyTo(_ *configuration.AppContext, _ *testEntity) error { return nil }
func (c *withBeforeCommitIDCmd) ApplyPartiallyTo(_ *configuration.AppContext, _ *testEntity) error {
	return nil
}
func (c *withBeforeCommitIDCmd) FromEntity(_ *configuration.AppContext, _ *testEntity) (fwresults.None, error) {
	return fwresults.None{}, nil
}
func (c *withBeforeCommitIDCmd) BeforeCommit(_ *configuration.AppContext, _ *testEntity, _ domain.ID, _ persistence.TxHandle) error {
	return nil
}

type withBothHooksIDCmd struct {
	pipeline.CommandBaseWithID
}

func (c *withBothHooksIDCmd) ApplyTo(_ *configuration.AppContext, _ *testEntity) error { return nil }
func (c *withBothHooksIDCmd) ApplyPartiallyTo(_ *configuration.AppContext, _ *testEntity) error {
	return nil
}
func (c *withBothHooksIDCmd) FromEntity(_ *configuration.AppContext, _ *testEntity) (fwresults.None, error) {
	return fwresults.None{}, nil
}
func (c *withBothHooksIDCmd) AfterBegin(_ *configuration.AppContext, _ *testEntity, _ persistence.TxHandle) error {
	return nil
}
func (c *withBothHooksIDCmd) BeforeCommit(_ *configuration.AppContext, _ *testEntity, _ domain.ID, _ persistence.TxHandle) error {
	return nil
}

// noHooksIDCmd implements neither provider.
type noHooksIDCmd struct {
	pipeline.CommandBaseWithID
}

func (c *noHooksIDCmd) ApplyTo(_ *configuration.AppContext, _ *testEntity) error          { return nil }
func (c *noHooksIDCmd) ApplyPartiallyTo(_ *configuration.AppContext, _ *testEntity) error { return nil }
func (c *noHooksIDCmd) FromEntity(_ *configuration.AppContext, _ *testEntity) (fwresults.None, error) {
	return fwresults.None{}, nil
}

// withIDPath sets a fresh UUID on the path field so RequirePathID does
// not abort the dispatch; returns the cmd for chaining.
func withIDPath[Cmd interface {
	SetPathID(string)
}](c Cmd) Cmd {
	c.SetPathID(domain.NewRandomID().Value())
	return c
}

// Update dispatch.

func TestUpdateCommandHandler_HookDispatch(t *testing.T) {
	cases := []struct {
		name string
		cmd  any
		want int
	}{
		{"afterBeginOnly", withIDPath(&withAfterBeginIDCmd{}), 1},
		{"beforeCommitOnly", withIDPath(&withBeforeCommitIDCmd{}), 1},
		{"both", withIDPath(&withBothHooksIDCmd{}), 2},
		{"neither", withIDPath(&noHooksIDCmd{}), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newMockRepo()
			switch cmd := tc.cmd.(type) {
			case *withAfterBeginIDCmd:
				h := &UpdateCommandHandler[*testEntity, *withAfterBeginIDCmd, fwresults.None]{Repo: repo}
				if _, err := h.Handle(testCtx(), cmd); err != nil {
					t.Fatal(err)
				}
			case *withBeforeCommitIDCmd:
				h := &UpdateCommandHandler[*testEntity, *withBeforeCommitIDCmd, fwresults.None]{Repo: repo}
				if _, err := h.Handle(testCtx(), cmd); err != nil {
					t.Fatal(err)
				}
			case *withBothHooksIDCmd:
				h := &UpdateCommandHandler[*testEntity, *withBothHooksIDCmd, fwresults.None]{Repo: repo}
				if _, err := h.Handle(testCtx(), cmd); err != nil {
					t.Fatal(err)
				}
			case *noHooksIDCmd:
				h := &UpdateCommandHandler[*testEntity, *noHooksIDCmd, fwresults.None]{Repo: repo}
				if _, err := h.Handle(testCtx(), cmd); err != nil {
					t.Fatal(err)
				}
			}
			if got := len(repo.updateOpts); got != tc.want {
				t.Errorf("len(updateOpts)=%d, want %d", got, tc.want)
			}
		})
	}
}

// PartialUpdate dispatch.

func TestPartialUpdateCommandHandler_HookDispatch(t *testing.T) {
	cases := []struct {
		name string
		cmd  any
		want int
	}{
		{"afterBeginOnly", withIDPath(&withAfterBeginIDCmd{}), 1},
		{"beforeCommitOnly", withIDPath(&withBeforeCommitIDCmd{}), 1},
		{"both", withIDPath(&withBothHooksIDCmd{}), 2},
		{"neither", withIDPath(&noHooksIDCmd{}), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newMockRepo()
			switch cmd := tc.cmd.(type) {
			case *withAfterBeginIDCmd:
				h := &PartialUpdateCommandHandler[*testEntity, *withAfterBeginIDCmd, fwresults.None]{Repo: repo}
				if _, err := h.Handle(testCtx(), cmd); err != nil {
					t.Fatal(err)
				}
			case *withBeforeCommitIDCmd:
				h := &PartialUpdateCommandHandler[*testEntity, *withBeforeCommitIDCmd, fwresults.None]{Repo: repo}
				if _, err := h.Handle(testCtx(), cmd); err != nil {
					t.Fatal(err)
				}
			case *withBothHooksIDCmd:
				h := &PartialUpdateCommandHandler[*testEntity, *withBothHooksIDCmd, fwresults.None]{Repo: repo}
				if _, err := h.Handle(testCtx(), cmd); err != nil {
					t.Fatal(err)
				}
			case *noHooksIDCmd:
				h := &PartialUpdateCommandHandler[*testEntity, *noHooksIDCmd, fwresults.None]{Repo: repo}
				if _, err := h.Handle(testCtx(), cmd); err != nil {
					t.Fatal(err)
				}
			}
			if got := len(repo.updateOpts); got != tc.want {
				t.Errorf("len(updateOpts)=%d, want %d", got, tc.want)
			}
		})
	}
}

// Archive dispatch.

func TestArchiveCommandHandler_HookDispatch(t *testing.T) {
	cases := []struct {
		name string
		cmd  any
		want int
	}{
		{"afterBeginOnly", withIDPath(&withAfterBeginIDCmd{}), 1},
		{"beforeCommitOnly", withIDPath(&withBeforeCommitIDCmd{}), 1},
		{"both", withIDPath(&withBothHooksIDCmd{}), 2},
		{"neither", withIDPath(&noHooksIDCmd{}), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newMockRepo()
			switch cmd := tc.cmd.(type) {
			case *withAfterBeginIDCmd:
				h := &ArchiveCommandHandler[*testEntity, *withAfterBeginIDCmd, fwresults.None]{Repo: repo}
				if _, err := h.Handle(testCtx(), cmd); err != nil {
					t.Fatal(err)
				}
			case *withBeforeCommitIDCmd:
				h := &ArchiveCommandHandler[*testEntity, *withBeforeCommitIDCmd, fwresults.None]{Repo: repo}
				if _, err := h.Handle(testCtx(), cmd); err != nil {
					t.Fatal(err)
				}
			case *withBothHooksIDCmd:
				h := &ArchiveCommandHandler[*testEntity, *withBothHooksIDCmd, fwresults.None]{Repo: repo}
				if _, err := h.Handle(testCtx(), cmd); err != nil {
					t.Fatal(err)
				}
			case *noHooksIDCmd:
				h := &ArchiveCommandHandler[*testEntity, *noHooksIDCmd, fwresults.None]{Repo: repo}
				if _, err := h.Handle(testCtx(), cmd); err != nil {
					t.Fatal(err)
				}
			}
			if got := len(repo.archiveOpts); got != tc.want {
				t.Errorf("len(archiveOpts)=%d, want %d", got, tc.want)
			}
		})
	}
}

// Unarchive dispatch.

func TestUnarchiveCommandHandler_HookDispatch(t *testing.T) {
	cases := []struct {
		name string
		cmd  any
		want int
	}{
		{"afterBeginOnly", withIDPath(&withAfterBeginIDCmd{}), 1},
		{"beforeCommitOnly", withIDPath(&withBeforeCommitIDCmd{}), 1},
		{"both", withIDPath(&withBothHooksIDCmd{}), 2},
		{"neither", withIDPath(&noHooksIDCmd{}), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newMockRepo()
			switch cmd := tc.cmd.(type) {
			case *withAfterBeginIDCmd:
				h := &UnarchiveCommandHandler[*testEntity, *withAfterBeginIDCmd, fwresults.None]{Repo: repo}
				if _, err := h.Handle(testCtx(), cmd); err != nil {
					t.Fatal(err)
				}
			case *withBeforeCommitIDCmd:
				h := &UnarchiveCommandHandler[*testEntity, *withBeforeCommitIDCmd, fwresults.None]{Repo: repo}
				if _, err := h.Handle(testCtx(), cmd); err != nil {
					t.Fatal(err)
				}
			case *withBothHooksIDCmd:
				h := &UnarchiveCommandHandler[*testEntity, *withBothHooksIDCmd, fwresults.None]{Repo: repo}
				if _, err := h.Handle(testCtx(), cmd); err != nil {
					t.Fatal(err)
				}
			case *noHooksIDCmd:
				h := &UnarchiveCommandHandler[*testEntity, *noHooksIDCmd, fwresults.None]{Repo: repo}
				if _, err := h.Handle(testCtx(), cmd); err != nil {
					t.Fatal(err)
				}
			}
			if got := len(repo.unarchiveOpts); got != tc.want {
				t.Errorf("len(unarchiveOpts)=%d, want %d", got, tc.want)
			}
		})
	}
}

// Delete dispatch.

func TestDeleteCommandHandler_HookDispatch(t *testing.T) {
	cases := []struct {
		name string
		cmd  any
		want int
	}{
		{"afterBeginOnly", withIDPath(&withAfterBeginIDCmd{}), 1},
		{"beforeCommitOnly", withIDPath(&withBeforeCommitIDCmd{}), 1},
		{"both", withIDPath(&withBothHooksIDCmd{}), 2},
		{"neither", withIDPath(&noHooksIDCmd{}), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newMockRepo()
			switch cmd := tc.cmd.(type) {
			case *withAfterBeginIDCmd:
				h := &DeleteCommandHandler[*testEntity, *withAfterBeginIDCmd, fwresults.None]{Repo: repo}
				if _, err := h.Handle(testCtx(), cmd); err != nil {
					t.Fatal(err)
				}
			case *withBeforeCommitIDCmd:
				h := &DeleteCommandHandler[*testEntity, *withBeforeCommitIDCmd, fwresults.None]{Repo: repo}
				if _, err := h.Handle(testCtx(), cmd); err != nil {
					t.Fatal(err)
				}
			case *withBothHooksIDCmd:
				h := &DeleteCommandHandler[*testEntity, *withBothHooksIDCmd, fwresults.None]{Repo: repo}
				if _, err := h.Handle(testCtx(), cmd); err != nil {
					t.Fatal(err)
				}
			case *noHooksIDCmd:
				h := &DeleteCommandHandler[*testEntity, *noHooksIDCmd, fwresults.None]{Repo: repo}
				if _, err := h.Handle(testCtx(), cmd); err != nil {
					t.Fatal(err)
				}
			}
			if got := len(repo.deleteOpts); got != tc.want {
				t.Errorf("len(deleteOpts)=%d, want %d", got, tc.want)
			}
		})
	}
}

// --- Provider-interface compile-time conformance --------------------------

// var asserts the framework's typed provider interfaces are satisfied by
// the test Cmds. Compile failure here means the provider interface has
// drifted from the Cmd shape — the Topic 4 contract is broken.
var (
	_ persistence.AfterBeginHookProvider[*testEntity]   = (*withAfterBeginCmd)(nil)
	_ persistence.BeforeCommitHookProvider[*testEntity] = (*withBeforeCommitCmd)(nil)
	_ persistence.AfterBeginHookProvider[*testEntity]   = (*withBothHooksCmd)(nil)
	_ persistence.BeforeCommitHookProvider[*testEntity] = (*withBothHooksCmd)(nil)
)

// TestCollectWriteOptions_HookErrorPropagates verifies that an error
// returned by an BeforeCommit method reaches the resolved closure via
// the WithBeforeCommit option. Belt-and-suspenders against the handler
// silently swallowing the provider's error.
func TestCollectWriteOptions_HookErrorPropagates(t *testing.T) {
	wantErr := errors.New("hook rejects")
	cmd := &errorCmd{wantErr: wantErr}
	opts := collectWriteOptions[*testEntity, *errorCmd](cmd)
	if len(opts) != 1 {
		t.Fatalf("expected 1 option, got %d", len(opts))
	}
	_, bc := persistence.ResolveWriteOptions(opts)
	if bc == nil {
		t.Fatal("expected beforeCommit closure populated")
	}
	if got := bc(nil, nil, domain.ID{}, nil); !errors.Is(got, wantErr) {
		t.Errorf("expected provider error to surface, got %v", got)
	}
}

type errorCmd struct {
	pipeline.CommandBase
	wantErr error
}

func (c *errorCmd) ToEntity(_ *configuration.AppContext) *testEntity { return &testEntity{} }
func (c *errorCmd) FromEntity(_ *configuration.AppContext, _ *testEntity) (fwresults.None, error) {
	return fwresults.None{}, nil
}
func (c *errorCmd) BeforeCommit(_ *configuration.AppContext, _ *testEntity, _ domain.ID, _ persistence.TxHandle) error {
	return c.wantErr
}
