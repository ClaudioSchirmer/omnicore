package domain

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// CompleteAsArchive is the domain's own decision that an update finishes as an
// archive. Two things make it inviolable: it can only be SPOKEN from inside the
// entity's BuildRules while the verb is Update, and once Get* seals the
// Updatable the value lives only there — the same place `partial` lives.

type completionEntity struct {
	BaseEntity
	Seats  int
	Status string

	archiveWhenEmpty bool
	callFrom         EntityMode // when set, the rule calls from that mode's closure
}

func (e *completionEntity) Modes() []EntityMode {
	if e.Status == "no-archive" {
		return []EntityMode{ModeUpdate} // deliberately omits ModeArchive
	}
	return []EntityMode{ModeInsert, ModeUpdate, ModeDelete, ModeArchive, ModeUnarchive}
}

func (e *completionEntity) BuildRules(_ string, _ Service, r *Rules) {
	if e.archiveWhenEmpty {
		r.IfUpdate(func() {
			if e.Seats == 0 {
				e.CompleteAsArchive()
			}
		})
	}
	switch e.callFrom {
	case ModeArchive:
		r.IfArchive(func() { e.CompleteAsArchive() })
	case ModeInsert:
		r.IfInsert(func() { e.CompleteAsArchive() })
	case ModeDelete:
		r.IfDelete(func() { e.CompleteAsArchive() })
	}
}

func newCompletionEntity() *completionEntity {
	e := &completionEntity{Seats: 3, Status: "active"}
	e.SetID(NewID(uuid.NewString()))
	return e
}

func mustPanic(t *testing.T, want string, fn func()) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected a panic")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, want) {
			t.Errorf("panic message must explain the misuse — wanted %q in:\n%v", want, r)
		}
	}()
	fn()
}

func TestCompleteAsArchive_SealsTheFinalModeOntoTheUpdatable(t *testing.T) {
	e := newCompletionEntity()
	e.archiveWhenEmpty = true
	e.Seats = 0

	upd, err := GetUpdatable(e, nil, nil, "GetUpdatable")
	if err != nil {
		t.Fatalf("GetUpdatable: %v", err)
	}

	if got := upd.EntityMode(); got != ModeArchive {
		t.Errorf("Completion() = %v, want ModeArchive", got)
	}
	// And it is GONE from the entity: after the seal the value lives in exactly
	// one place, so nothing downstream can read or change it there.
	if got := e.takeRequestedMode(); got != ModeUnknown {
		t.Errorf("the entity must not keep the request after the seal, got %v", got)
	}
}

func TestUpdate_SealsModeUpdateWhenNoRuleAsks(t *testing.T) {
	e := newCompletionEntity()
	e.archiveWhenEmpty = true
	e.Seats = 3 // the rule's condition is false

	upd, err := GetUpdatable(e, nil, nil, "GetUpdatable")
	if err != nil {
		t.Fatalf("GetUpdatable: %v", err)
	}
	if got := upd.EntityMode(); got != ModeUpdate {
		t.Errorf("a plain update must seal ModeUpdate, got %v", got)
	}
}

// The window: only from inside BuildRules.
func TestCompleteAsArchive_OutsideTheRulesWindowPanics(t *testing.T) {
	t.Run("beforeAnyVerb", func(t *testing.T) {
		e := newCompletionEntity()
		mustPanic(t, "only an UPDATE migrates to archive", func() { e.CompleteAsArchive() })
	})

	t.Run("fromTheCommandsApplyTo", func(t *testing.T) {
		e := newCompletionEntity()
		apply := func(x *completionEntity) error {
			x.CompleteAsArchive() // ApplyTo runs before the framework marks the mode
			return nil
		}
		mustPanic(t, "runs before the mode is set", func() {
			_, _ = GetUpdatable(e, apply, nil, "GetUpdatable")
		})
	})

	t.Run("afterTheSeal", func(t *testing.T) {
		e := newCompletionEntity()
		if _, err := GetUpdatable(e, nil, nil, "GetUpdatable"); err != nil {
			t.Fatalf("GetUpdatable: %v", err)
		}
		// The mode is still ModeUpdate here — the WINDOW is what closes the door.
		mustPanic(t, "window closed", func() { e.CompleteAsArchive() })
	})
}

// The mode: only an update migrates.
func TestCompleteAsArchive_FromAnotherVerbPanics(t *testing.T) {
	for _, tc := range []struct {
		name string
		from EntityMode
		call func(e *completionEntity) error
	}{
		{"IfArchive", ModeArchive, func(e *completionEntity) error {
			_, err := GetArchivable(e, nil, "GetArchivable")
			return err
		}},
		{"IfInsert", ModeInsert, func(e *completionEntity) error {
			e.ClearID()
			_, err := GetInsertable(e, nil, "GetInsertable")
			return err
		}},
		{"IfDelete", ModeDelete, func(e *completionEntity) error {
			_, err := GetDeletable(e, nil, "GetDeletable")
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newCompletionEntity()
			e.callFrom = tc.from
			mustPanic(t, "only an UPDATE migrates to archive", func() { _ = tc.call(e) })
		})
	}
}

// Modes() is still the gate: the domain cannot archive through the update door
// what it forbids archiving through the archive door.
func TestCompleteAsArchive_EntityThatForbidsArchivePanics(t *testing.T) {
	e := newCompletionEntity()
	e.Status = "no-archive" // Modes() omits ModeArchive
	e.archiveWhenEmpty = true
	e.Seats = 0

	mustPanic(t, "does not declare", func() {
		_, _ = GetUpdatable(e, nil, nil, "GetUpdatable")
	})
}

// A rule that panics must not leave the window open for later code.
func TestRulesWindow_ClosesEvenWhenARulePanics(t *testing.T) {
	e := newCompletionEntity()
	e.callFrom = ModeArchive // its IfArchive closure panics

	func() {
		defer func() { _ = recover() }()
		_, _ = GetArchivable(e, nil, "GetArchivable")
	}()

	mustPanic(t, "window closed", func() { e.CompleteAsArchive() })
}
