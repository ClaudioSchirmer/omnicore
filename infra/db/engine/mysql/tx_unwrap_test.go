//go:build mysql

package mysql

import (
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/persistence"
)

// TestUnwrapMySQLTx_AcceptsEngineHandle is the MySQL twin of the Postgres
// UnwrapPgxTx happy path: a handle the framework itself constructed passes the
// type gate (does not panic) and returns its stored *sql.Tx. Unlike pgx.Tx
// (an interface, stubbable), *sql.Tx is a concrete struct, so identity is
// asserted against the stored field rather than an injected stub.
func TestUnwrapMySQLTx_AcceptsEngineHandle(t *testing.T) {
	h := &mysqlTxHandle{} // tx left nil — a real *sql.Tx needs a live DB
	var th persistence.TxHandle = h

	got := UnwrapMySQLTx(th)
	if got != h.tx {
		t.Fatalf("UnwrapMySQLTx returned a different *sql.Tx than the handle stored")
	}
}

// foreignTxHandle stands in for a non-framework persistence.TxHandle — the case
// UnwrapMySQLTx defends against. The shape is reachable only by embedding the
// sealed marker and bypassing newMySQLTxHandle (the sealing method is
// unexported), so it never appears in production; the test forces it on purpose.
type foreignTxHandle struct {
	persistence.SealedTxHandle
}

// TestUnwrapMySQLTx_PanicsOnForeign proves a handle that did not come from the
// MySQL engine panics with a descriptive message instead of returning a nil
// *sql.Tx that would later NPE inside a SQL call. Mirrors UnwrapPgxTx.
func TestUnwrapMySQLTx_PanicsOnForeign(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("UnwrapMySQLTx did not panic on a foreign persistence.TxHandle")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, "foreign persistence.TxHandle") {
			t.Fatalf("panic message missing diagnostic; got %v", r)
		}
	}()

	var foreign persistence.TxHandle = foreignTxHandle{}
	_ = UnwrapMySQLTx(foreign) // expected to panic
}
