package persistence

import (
	"errors"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/google/uuid"
)

// The write-side load helpers are the net for a repository that does NOT go
// through the framework's relational loader (the hand-rolled escape hatch):
// they must stamp the birth-time snapshot themselves, so domain.Old answers the
// persisted state on every verb regardless of which repository shape a service
// wired up.

type entityRow struct {
	domain.BaseEntity
	Status string
}

func (e *entityRow) Modes() []domain.EntityMode {
	return []domain.EntityMode{domain.ModeUpdate, domain.ModeArchive, domain.ModeUnarchive, domain.ModeDelete}
}
func (e *entityRow) BuildRules(string, domain.Service, *domain.Rules) {}

func newEntityRow(status string) *entityRow {
	e := &entityRow{Status: status}
	e.SetID(domain.NewID(uuid.NewString()))
	return e
}

// entityRepo is the ctx-less escape-hatch shape: no ScopedReader, no
// ScopedArchivedReader — LoadForWrite/LoadArchivedForWrite degrade to these.
type entityRepo struct {
	ret *entityRow
	err error
}

func (r *entityRepo) FindByID(domain.ID) (*entityRow, error)         { return r.ret, r.err }
func (r *entityRepo) FindArchivedByID(domain.ID) (*entityRow, error) { return r.ret, r.err }
func (r *entityRepo) New() *entityRow                                { return &entityRow{} }
func (r *entityRepo) Scope(*configuration.AppContext, ...WriteOption[*entityRow]) domain.Writer {
	return nil
}

func TestLoadForWrite_StampsBirthSnapshot(t *testing.T) {
	repo := &entityRepo{ret: newEntityRow("trial")}

	loaded, err := LoadForWrite[*entityRow](repo, ctx(), domain.NewRandomID())
	if err != nil {
		t.Fatalf("LoadForWrite: %v", err)
	}

	loaded.Status = "mutated-after-load"
	old := domain.Old(loaded)
	if old == nil {
		t.Fatal("LoadForWrite must stamp the old-state snapshot on the loaded entity")
	}
	if old.Status != "trial" {
		t.Errorf("snapshot must hold the persisted state: Status = %q, want %q", old.Status, "trial")
	}
}

func TestLoadArchivedForWrite_StampsBirthSnapshot(t *testing.T) {
	repo := &entityRepo{ret: newEntityRow("suspended")}

	loaded, found, err := LoadArchivedForWrite[*entityRow](repo, ctx(), domain.NewRandomID())
	if err != nil || !found {
		t.Fatalf("LoadArchivedForWrite: found=%v err=%v", found, err)
	}

	loaded.Status = "mutated-after-load"
	old := domain.Old(loaded)
	if old == nil {
		t.Fatal("LoadArchivedForWrite must stamp the old-state snapshot")
	}
	if old.Status != "suspended" {
		t.Errorf("snapshot must hold the persisted archived state: Status = %q, want %q", old.Status, "suspended")
	}
}

// A failed load must pass the error through untouched and never snapshot the
// zero value it returns alongside it.
func TestLoadForWrite_FailedLoadIsNotSnapshotted(t *testing.T) {
	boom := errors.New("boom")
	repo := &entityRepo{ret: newEntityRow("trial"), err: boom}

	loaded, err := LoadForWrite[*entityRow](repo, ctx(), domain.NewRandomID())
	if !errors.Is(err, boom) {
		t.Fatalf("expected the load error to pass through, got %v", err)
	}
	if loaded != nil && domain.Old(loaded) != nil {
		t.Error("a failed load must not stamp a snapshot")
	}
}

// The helpers are generic on `any`: a T that is not a domain.Entity must thread
// through untouched (the existing readEntity tests cover the happy path; this
// pins the nil-typed case that would panic on a careless assertion).
func TestLoadForWrite_NonEntityTypeIsUntouched(t *testing.T) {
	repo := &plainRepo{ret: &readEntity{tag: "x"}}

	got, err := LoadForWrite[*readEntity](repo, ctx(), domain.NewRandomID())
	if err != nil {
		t.Fatalf("LoadForWrite: %v", err)
	}
	if got.tag != "x" {
		t.Errorf("value must thread through unchanged, got %q", got.tag)
	}
}
