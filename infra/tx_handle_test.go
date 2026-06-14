package infra

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/ClaudioSchirmer/omnicore/application/persistence"
)

// stubTx is a pgx.Tx implementation only used as a marker payload. The
// unit test does not exercise any of its methods — UnwrapPgxTx returns
// the same pointer it was constructed with, so identity comparison is
// enough.
type stubTx struct {
	pgx.Tx // embed the interface so we satisfy it without listing every method
}

// TestUnwrapPgxTx_ReturnsUnderlyingTx covers the happy path: a handle
// the framework itself constructed unwraps to the exact pgx.Tx it was
// built with.
func TestUnwrapPgxTx_ReturnsUnderlyingTx(t *testing.T) {
	wanted := &stubTx{}
	handle := newPgxTxHandle(wanted)

	got := UnwrapPgxTx(handle)
	if got != wanted {
		t.Fatalf("UnwrapPgxTx returned a different pgx.Tx; want=%p got=%p", wanted, got)
	}
}

// foreignTxHandle stands in for a non-framework implementation of
// persistence.TxHandle — the case UnwrapPgxTx defends against. The
// shape is achievable only by embedding SealedTxHandle and bypassing
// the framework's own newPgxTxHandle constructor, so in practice it
// never appears in production code; the test reaches the panic branch
// by writing exactly that shape on purpose.
type foreignTxHandle struct {
	persistence.SealedTxHandle
}

// TestUnwrapPgxTx_PanicsOnForeignImpl proves that a TxHandle that did
// not come from newPgxTxHandle (only achievable from inside this test
// package, because the sealing method is unexported) panics with a
// descriptive message instead of returning a nil pgx.Tx that would
// later NPE inside a SQL call.
func TestUnwrapPgxTx_PanicsOnForeignImpl(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("UnwrapPgxTx did not panic on a foreign TxHandle implementation")
		}
		msg, ok := r.(string)
		if !ok {
			t.Fatalf("expected panic value to be a string; got %T (%v)", r, r)
		}
		if !strings.Contains(msg, "foreign persistence.TxHandle implementation") {
			t.Fatalf("panic message missing diagnostic; got %q", msg)
		}
	}()

	var foreign persistence.TxHandle = foreignTxHandle{}
	_ = UnwrapPgxTx(foreign) // expected to panic
}
