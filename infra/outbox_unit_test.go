package infra

import (
	"context"
	"testing"
)

// writeOutbox marshals the payload then INSERTs the outbox row inside the
// caller's TX. These drive it directly against the fakeTx: the json.Marshal
// error branch (an unmarshalable payload) and the Exec error branch.

func TestWriteOutbox_MarshalError(t *testing.T) {
	tx := newFakeTx()
	// A channel cannot be JSON-marshaled, forcing the marshal error branch
	// before any Exec is attempted.
	if err := writeOutbox(context.Background(), tx, "users", "INSERTED", "id-1", make(chan int)); err == nil {
		t.Fatal("expected json.Marshal error for an unmarshalable payload")
	}
	if len(tx.execCalls) != 0 {
		t.Errorf("Exec must not run after a marshal error, got %d calls", len(tx.execCalls))
	}
}

func TestWriteOutbox_ExecError(t *testing.T) {
	tx := newFakeTx()
	tx.execErr = errFake
	if err := writeOutbox(context.Background(), tx, "users", "INSERTED", "id-1", map[string]any{"x": 1}); err == nil {
		t.Fatal("expected Exec error to surface")
	}
}

func TestWriteOutbox_HappyPath(t *testing.T) {
	tx := newFakeTx()
	if err := writeOutbox(context.Background(), tx, "users", "INSERTED", "id-1", map[string]any{"x": 1}); err != nil {
		t.Fatalf("writeOutbox: %v", err)
	}
	if len(tx.execCalls) != 1 {
		t.Errorf("expected one outbox Exec, got %d", len(tx.execCalls))
	}
}
