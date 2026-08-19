package domain

import "testing"

// The wire contract of the concurrency guard: a stale write is a PRECONDITION
// failure, not a duplicate. SemanticStateConflict carries that distinction —
// 409 on HTTP, FailedPrecondition on gRPC — while SemanticConflict would say
// "already exists" on the gRPC surface.
func TestConcurrentModificationNotification_IsAStateConflict(t *testing.T) {
	n := ConcurrentModificationNotification{}

	if got := n.Semantic(); got != SemanticStateConflict {
		t.Errorf("Semantic() = %v, want SemanticStateConflict", got)
	}
	if got := NotificationKey(n); got != "ConcurrentModificationNotification" {
		t.Errorf("NotificationKey = %q — the translation catalogs key on this exact name", got)
	}
}
