package domain

import (
	"testing"

	"github.com/google/uuid"
)

// transitionEntity exercises the closure form of GetUpdatable. Its BuildRules
// reads domain.Old(e) and rejects an Email mutation when the entity is in
// "activated" state — the canonical "field locked after activation" invariant.
type transitionEntity struct {
	BaseEntity
	Email     string
	Activated bool
}

func (t *transitionEntity) Modes() []EntityMode {
	return []EntityMode{ModeInsert, ModeUpdate}
}
func (t *transitionEntity) BuildRules(_ string, _ Service, r *Rules) {
	r.IfUpdate(func() {
		if old := Old(t); old != nil {
			if old.Email != t.Email && t.Activated {
				r.AddNotification("Email", RequiredFieldNotification{})
			}
		}
	})
}

func TestGetUpdatable_ClosureCapturesPreMutationState(t *testing.T) {
	e := &transitionEntity{Email: "alice@x.com", Activated: true}
	e.SetID(NewID(uuid.NewString()))

	apply := func(x *transitionEntity) error { x.Email = "alice2@x.com"; return nil }

	_, err := GetUpdatable(e, apply, nil, "GetUpdatable")
	if err == nil {
		t.Fatal("expected GetUpdatable to fail because Email was locked after activation")
	}
	// The notification is emitted by BuildRules — Old() must have surfaced
	// the pre-mutation Email="alice@x.com" for the comparison to fire.
}

func TestGetUpdatable_NoFailureWhenInvariantSatisfied(t *testing.T) {
	e := &transitionEntity{Email: "alice@x.com", Activated: false}
	e.SetID(NewID(uuid.NewString()))

	apply := func(x *transitionEntity) error { x.Email = "alice2@x.com"; return nil }

	if _, err := GetUpdatable(e, apply, nil, "GetUpdatable"); err != nil {
		t.Fatalf("expected GetUpdatable to succeed when Activated=false, got %v", err)
	}
}

func TestGetUpdatable_AppliesMutation(t *testing.T) {
	e := &transitionEntity{Email: "alice@x.com"}
	e.SetID(NewID(uuid.NewString()))

	apply := func(x *transitionEntity) error { x.Email = "new@x.com"; return nil }

	_, err := GetUpdatable(e, apply, nil, "GetUpdatable")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if e.Email != "new@x.com" {
		t.Errorf("expected closure to mutate entity, got Email=%q", e.Email)
	}
}

func TestGetPartialUpdatable_RunsClosureAndSnapshot(t *testing.T) {
	e := &transitionEntity{Email: "alice@x.com", Activated: true}
	e.SetID(NewID(uuid.NewString()))

	apply := func(x *transitionEntity) error { x.Email = "patched@x.com"; return nil }

	_, err := GetPartialUpdatable(e, apply, nil, "GetPartialUpdatable")
	if err == nil {
		t.Fatal("expected GetPartialUpdatable to surface the same transition invariant as GetUpdatable")
	}
}

func TestGetDeletable_CapturesOld(t *testing.T) {
	e := &simpleEntity{Name: "alice"}
	e.SetID(NewID(uuid.NewString()))

	if _, err := GetDeletable(e, nil, "GetDeletable"); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if e.Old() == nil {
		t.Error("expected GetDeletable to set Old() for forensic snapshot")
	}
}

func TestGetArchivable_CapturesOld(t *testing.T) {
	e := &simpleEntity{Name: "alice"}
	e.SetID(NewID(uuid.NewString()))

	if _, err := GetArchivable(e, nil, "GetArchivable"); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if e.Old() == nil {
		t.Error("expected GetArchivable to set Old() (pre-archive snapshot)")
	}
}

func TestGetUnarchivable_CapturesOld(t *testing.T) {
	e := &simpleEntity{Name: "alice"}
	e.SetID(NewID(uuid.NewString()))

	if _, err := GetUnarchivable(e, nil, "GetUnarchivable"); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if e.Old() == nil {
		t.Error("expected GetUnarchivable to set Old() (archived-state snapshot)")
	}
}

func TestGetInsertable_DoesNotCaptureOld(t *testing.T) {
	e := &simpleEntity{Name: "alice"}

	if _, err := GetInsertable(e, nil, "GetInsertable"); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if e.Old() != nil {
		t.Error("expected GetInsertable NOT to set Old() — Insert has no prior state by definition")
	}
}
