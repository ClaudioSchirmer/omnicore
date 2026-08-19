package read

import (
	"context"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/criteria"
)

// FindOne is the framework's BIRTH point for a write-side entity, and every
// single-entity load funnels through it — the canonical repository's FindByID /
// FindArchivedByID and any CUSTOM repository finder built on this loader. That
// is what gives a hand-written handler the same domain.Old guarantee an Auto
// handler gets, without the handler doing anything.

func TestFindOne_StampsBirthSnapshot(t *testing.T) {
	l := newCovAggLoader(fakeEngine(covAggQuery(covAggRootRow("r1", "Ana"), covChildRow("r1", "c1", "L1"), nil)), covAggSchema)

	e, err := l.FindOne(context.Background(), criteria.ByID(domain.NewID("r1")))
	if err != nil {
		t.Fatalf("FindOne: %v", err)
	}

	// Everything below this line is what a handler (Auto or manual) does next.
	e.Name = "mutated-by-applyto"
	domain.RemoveAggregateChild(e, covChild{Label: "L1"})

	old := domain.Old(e)
	if old == nil {
		t.Fatal("FindOne must stamp the old-state snapshot on the hydrated aggregate")
	}
	if old.Name != "Ana" {
		t.Errorf("snapshot must hold the persisted root state: Name = %q, want %q", old.Name, "Ana")
	}
	if got := len(domain.GetCurrentItemsOf[covChild](&old.AggregateRoot)); got != 1 {
		t.Errorf("snapshot must hold the hydrated children, got %d want 1", got)
	}
}

// The archived scope is the unarchive path's load — it must snapshot too, or
// domain.Old inside IfUnarchive would answer nil.
func TestFindOne_ArchivedScopeStampsBirthSnapshot(t *testing.T) {
	l := newCovAggLoader(fakeEngine(covAggQuery(covAggRootRow("r1", "Ana"), covChildRow("r1", "c1", "L1"), nil)), covAggSchema)

	e, err := l.FindOne(context.Background(), criteria.ByID(domain.NewID("r1")).OnlyArchived())
	if err != nil {
		t.Fatalf("FindOne(OnlyArchived): %v", err)
	}

	e.Name = "mutated"
	old := domain.Old(e)
	if old == nil || old.Name != "Ana" {
		t.Errorf("archived load must stamp the snapshot with the persisted state, got %+v", old)
	}
}

// FindAll is the read-side list path: no verb mutates there, so it deliberately
// does NOT pay a per-row clone. Pinning it keeps a future "make it symmetric"
// refactor from silently taxing every list read.
func TestFindAll_DoesNotSnapshot(t *testing.T) {
	l := newCovAggLoader(fakeEngine(covAggQuery(covAggRootRow("r1", "Ana"), covChildRow("r1", "c1", "L1"), nil)), covAggSchema)

	all, err := l.FindAll(context.Background(), criteria.Where(nil))
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("expected 1 root, got %d", len(all))
	}
	if domain.Old(all[0]) != nil {
		t.Error("FindAll must not snapshot — it is the read-side list path")
	}
}
