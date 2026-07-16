//go:build oracle

package oracle

import (
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/persistence"
)

// TestUnwrapOracleTx_AcceptsEngineHandle is the Oracle twin of the
// UnwrapPgxTx/UnwrapMySQLTx/UnwrapSQLServerTx happy path: a handle the
// framework itself constructed passes the type gate (does not panic) and
// returns its stored *sql.Tx. *sql.Tx is a concrete struct, so identity is
// asserted against the stored field rather than an injected stub.
func TestUnwrapOracleTx_AcceptsEngineHandle(t *testing.T) {
	h := &oracleTxHandle{} // tx left nil — a real *sql.Tx needs a live DB
	var th persistence.TxHandle = h

	got := UnwrapOracleTx(th)
	if got != h.tx {
		t.Fatalf("UnwrapOracleTx returned a different *sql.Tx than the handle stored")
	}
}

// foreignTxHandle stands in for a non-framework persistence.TxHandle — the case
// UnwrapOracleTx defends against. The shape is reachable only by embedding the
// sealed marker and bypassing newOracleTxHandle (the sealing method is
// unexported), so it never appears in production; the test forces it on
// purpose.
type foreignTxHandle struct {
	persistence.SealedTxHandle
}

// TestUnwrapOracleTx_PanicsOnForeign proves a handle that did not come from
// the Oracle engine panics with a descriptive message instead of returning a
// nil *sql.Tx that would later NPE inside a SQL call.
func TestUnwrapOracleTx_PanicsOnForeign(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("UnwrapOracleTx did not panic on a foreign persistence.TxHandle")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, "foreign persistence.TxHandle") {
			t.Fatalf("panic message missing diagnostic; got %v", r)
		}
	}()

	var foreign persistence.TxHandle = foreignTxHandle{}
	_ = UnwrapOracleTx(foreign) // expected to panic
}
