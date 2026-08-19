package domain

import (
	"testing"

	"github.com/google/uuid"
)

// The one capture rule, proven verb by verb: Old() is the state the entity was
// BORN with (hydrated from the system of record), never a state produced on the
// way to the write — not by a Command's ApplyTo, not by a BuildRules closure.
// Identical for Update, PartialUpdate, Archive, Unarchive and Delete; Insert is
// the documented exception (no prior state exists).

// statusEntity mirrors the real-world shape that exposed the asymmetry: a
// lifecycle field the domain flips FROM INSIDE the archive/unarchive rules.
type statusEntity struct {
	BaseEntity
	Name   string
	Status string
}

func (e *statusEntity) Modes() []EntityMode {
	return []EntityMode{ModeInsert, ModeUpdate, ModeDelete, ModeArchive, ModeUnarchive}
}

func (e *statusEntity) BuildRules(_ string, _ Service, r *Rules) {
	r.IfArchive(func() { e.Status = "suspended" })
	r.IfUnarchive(func() { e.Status = "active" })
}

// bornStatusEntity returns an entity as the loader hands it over: hydrated and
// already snapshotted.
func bornStatusEntity(t *testing.T) *statusEntity {
	t.Helper()
	e := &statusEntity{Name: "acme", Status: "trial"}
	e.SetID(NewID(uuid.NewString()))
	CaptureOld(e)
	return e
}

func oldStatus(t *testing.T, e *statusEntity) *statusEntity {
	t.Helper()
	old := Old(e)
	if old == nil {
		t.Fatal("Old() must not be nil after the entity was captured at birth")
	}
	return old
}

func TestCaptureOld_IsIdempotentEarliestSnapshotWins(t *testing.T) {
	e := bornStatusEntity(t)

	e.Status = "suspended"
	e.Name = "renamed"
	CaptureOld(e) // a late call must NOT replace the birth snapshot

	old := oldStatus(t, e)
	if old.Status != "trial" || old.Name != "acme" {
		t.Errorf("late CaptureOld overwrote the birth snapshot: got Status=%q Name=%q, want %q/%q",
			old.Status, old.Name, "trial", "acme")
	}
}

func TestCaptureOld_NilEntityIsNoOp(t *testing.T) {
	CaptureOld(nil) // must not panic
}

func TestCaptureOld_InitializesUncapturedEntity(t *testing.T) {
	// CaptureOld is the birth hook, so it must work on an entity the framework
	// has never seen (no prior ensureInit).
	e := &statusEntity{Name: "acme", Status: "trial"}
	e.SetID(NewID(uuid.NewString()))

	CaptureOld(e)

	if Old(e) == nil {
		t.Fatal("CaptureOld must snapshot an entity that was never initialized")
	}
}

// TestOldSnapshot_SurvivesMutationBeforeEveryVerb is the core guarantee: for all
// five state-changing verbs, a mutation applied between the load and the Get*
// call (what a Command's ApplyTo does) stays OUT of Old().
func TestOldSnapshot_SurvivesMutationBeforeEveryVerb(t *testing.T) {
	svc := (Service)(nil)
	cases := []struct {
		verb string
		get  func(e *statusEntity) error
	}{
		{"Update", func(e *statusEntity) error {
			_, err := GetUpdatable(e, nil, svc, "GetUpdatable")
			return err
		}},
		{"PartialUpdate", func(e *statusEntity) error {
			_, err := GetPartialUpdatable(e, nil, svc, "GetPartialUpdatable")
			return err
		}},
		{"Archive", func(e *statusEntity) error {
			_, err := GetArchivable(e, svc, "GetArchivable")
			return err
		}},
		{"Unarchive", func(e *statusEntity) error {
			_, err := GetUnarchivable(e, svc, "GetUnarchivable")
			return err
		}},
		{"Delete", func(e *statusEntity) error {
			_, err := GetDeletable(e, svc, "GetDeletable")
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.verb, func(t *testing.T) {
			e := bornStatusEntity(t)

			// The handler's cmd.ApplyTo lands here — after the load, before the verb.
			e.Status = "mutated-by-applyto"

			if err := tc.get(e); err != nil {
				t.Fatalf("%s: %v", tc.verb, err)
			}
			if got := oldStatus(t, e).Status; got != "trial" {
				t.Errorf("%s: Old().Status = %q, want the persisted %q — the pre-verb mutation leaked into the snapshot",
					tc.verb, got, "trial")
			}
		})
	}
}

// The apply closure of the update verbs must be excluded from the snapshot too,
// so the closure form and the birth form agree.
func TestOldSnapshot_ExcludesApplyClosureOfUpdateVerbs(t *testing.T) {
	for _, tc := range []struct {
		verb string
		get  func(e *statusEntity, apply func(*statusEntity) error) error
	}{
		{"Update", func(e *statusEntity, apply func(*statusEntity) error) error {
			_, err := GetUpdatable(e, apply, nil, "GetUpdatable")
			return err
		}},
		{"PartialUpdate", func(e *statusEntity, apply func(*statusEntity) error) error {
			_, err := GetPartialUpdatable(e, apply, nil, "GetPartialUpdatable")
			return err
		}},
	} {
		t.Run(tc.verb, func(t *testing.T) {
			e := bornStatusEntity(t)
			applied := false
			apply := func(x *statusEntity) error {
				applied = true
				x.Status = "mutated-by-apply-closure"
				return nil
			}

			if err := tc.get(e, apply); err != nil {
				t.Fatalf("%s: %v", tc.verb, err)
			}
			if !applied {
				t.Fatal("apply closure never ran — the test proves nothing")
			}
			if got := oldStatus(t, e).Status; got != "trial" {
				t.Errorf("Old().Status = %q, want %q", got, "trial")
			}
			if e.Status != "mutated-by-apply-closure" {
				t.Errorf("live entity lost the applied mutation: Status = %q", e.Status)
			}
		})
	}
}

// The scenario that exposed the bug: the domain flips a business field from
// inside IfArchive. The mutation must reach the LIVE entity (so the write path
// can act on it) and must stay out of Old() (so rules and the auditor can still
// see what was persisted).
func TestOldSnapshot_ExcludesIfArchiveMutation(t *testing.T) {
	e := bornStatusEntity(t)

	if _, err := GetArchivable(e, nil, "GetArchivable"); err != nil {
		t.Fatalf("GetArchivable: %v", err)
	}

	if e.Status != "suspended" {
		t.Fatalf("IfArchive must mutate the live entity: Status = %q, want %q", e.Status, "suspended")
	}
	if got := oldStatus(t, e).Status; got != "trial" {
		t.Errorf("Old().Status = %q, want the persisted %q — IfArchive's mutation leaked into the snapshot", got, "trial")
	}
}

func TestOldSnapshot_ExcludesIfUnarchiveMutation(t *testing.T) {
	e := &statusEntity{Name: "acme", Status: "suspended"}
	e.SetID(NewID(uuid.NewString()))
	CaptureOld(e)

	if _, err := GetUnarchivable(e, nil, "GetUnarchivable"); err != nil {
		t.Fatalf("GetUnarchivable: %v", err)
	}

	if e.Status != "active" {
		t.Fatalf("IfUnarchive must mutate the live entity: Status = %q, want %q", e.Status, "active")
	}
	if got := oldStatus(t, e).Status; got != "suspended" {
		t.Errorf("Old().Status = %q, want the persisted %q", got, "suspended")
	}
}

// Insert is the one verb outside the contract.
func TestOldSnapshot_InsertLeavesOldNil(t *testing.T) {
	e := &statusEntity{Name: "acme", Status: "trial"}

	if _, err := GetInsertable(e, nil, "GetInsertable"); err != nil {
		t.Fatalf("GetInsertable: %v", err)
	}

	if Old(e) != nil {
		t.Errorf("Insert must leave Old() nil (no prior state by definition), got %+v", Old(e))
	}
}

// The floor: an entity no framework path ever snapshotted still gets one at the
// Get* call, so Old() never regresses to nil for a hand-built entity.
func TestOldSnapshot_FallsBackToGetWhenNeverCaptured(t *testing.T) {
	e := &statusEntity{Name: "acme", Status: "trial"}
	e.SetID(NewID(uuid.NewString()))
	// No CaptureOld: this entity was assembled in memory, never loaded.

	if _, err := GetArchivable(e, nil, "GetArchivable"); err != nil {
		t.Fatalf("GetArchivable: %v", err)
	}

	old := oldStatus(t, e)
	if old.Status != "trial" {
		t.Errorf("fallback snapshot must hold the state at the Get* call: Status = %q, want %q", old.Status, "trial")
	}
}

// Aggregates: the birth snapshot owns the children as they were loaded, even
// when the verb's rules reshape the collection afterwards.
func TestOldSnapshot_AggregateChildrenAtBirth(t *testing.T) {
	e := &aggEntity{Name: "root"}
	e.SetID(NewID(uuid.NewString()))
	ensureInit(e)
	e.AggregateConstructor([]AggregateValueObject{
		aggChild{Label: "home"},
		aggChild{Label: "work"},
	})

	CaptureOld(e)

	// Post-birth reshaping (what ApplyTo or a rule would do).
	RemoveAggregateChild(e, aggChild{Label: "home"})
	if _, err := GetArchivable(e, nil, "GetArchivable"); err != nil {
		t.Fatalf("GetArchivable: %v", err)
	}

	old := Old(e)
	if old == nil {
		t.Fatal("Old() must not be nil for a captured aggregate")
	}
	if got := len(GetCurrentItemsOf[aggChild](&old.AggregateRoot)); got != 2 {
		t.Errorf("snapshot must hold the 2 children as loaded, got %d", got)
	}
	if got := len(GetCurrentItemsOf[aggChild](&e.AggregateRoot)); got != 1 {
		t.Errorf("live aggregate should carry 1 active child after the removal, got %d", got)
	}
}
