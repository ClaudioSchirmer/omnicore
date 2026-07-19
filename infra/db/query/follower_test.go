package query

import (
	"context"
	"errors"
	"testing"
)

// A held rebuild lock must surface ErrRebuildLockHeld so the boot path treats the
// pod as a follower (serve the active slot, wait for the driver's flip) instead
// of aborting.
func TestExecuteRebuild_LockHeldReturnsFollowerSentinel(t *testing.T) {
	view := rebuildView()
	eng := newFakeEngine(rebuildQuerier(nil, aliveRoot))
	eng.lockHeld = true
	eng.lockHolder = "pod-b@42"
	s := scriptSyncEngine(eng, &scriptStore{fakeStore: newFakeMongo(&fakeColl{})}, []*ViewDefinition{view})

	err := s.ExecuteRebuild(context.Background(), DriftPlan{View: view}, RebuildConfig{Orphan: "delete"})
	if !errors.Is(err, ErrRebuildLockHeld) {
		t.Fatalf("a held lock must return ErrRebuildLockHeld, got %v", err)
	}
}
