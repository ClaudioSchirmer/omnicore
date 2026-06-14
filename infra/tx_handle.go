package infra

import (
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/ClaudioSchirmer/omnicore/application/persistence"
)

// pgxTxHandle is the framework's only implementation of
// persistence.TxHandle. It carries the live pgx.Tx privately so that
// only this package can recover it via UnwrapPgxTx. The persister
// builds one per lifecycle-hook firing and discards it when the TX
// ends (Commit OR Rollback).
//
// The struct deliberately exposes no SQL surface: every Exec / Query /
// QueryRow call on the in-TX side effect is owned by an infra/ adapter
// that calls UnwrapPgxTx to obtain the underlying pgx.Tx. Application
// code never touches pgx directly.
//
// persistence.SealedTxHandle is embedded so that pgxTxHandle inherits
// the unexported sealing method that persistence.TxHandle requires —
// Go's canonical "package-private interface satisfier" pattern. The
// embed lets the concrete adapter live here in infra/ (where it can
// import pgx) while the seal is enforced by application/persistence/.
type pgxTxHandle struct {
	persistence.SealedTxHandle
	tx pgx.Tx
}

// newPgxTxHandle is the only constructor for the TxHandle adapter.
// Returns the sealed interface — callers see no methods other than the
// private sealing marker.
func newPgxTxHandle(tx pgx.Tx) persistence.TxHandle {
	return &pgxTxHandle{tx: tx}
}

// UnwrapPgxTx recovers the underlying pgx.Tx from a persistence.TxHandle.
// Used by infra-layer adapters that implement application/domain ports
// receiving a TxHandle parameter: the adapter owns the SQL + table
// name, so it needs the live pgx.Tx to execute the side effect inside
// the framework's TX.
//
// Because persistence.TxHandle is a sealed interface — only this
// package's pgxTxHandle satisfies it — the type assertion below is
// guaranteed to succeed against any handle the framework hands out.
// The panic exists as a defense for a foreign implementation that
// somehow surfaces (test fakes that bypass the sealing intent, or a
// future caller that mocks the interface incorrectly): failing loudly
// at the unwrap site is better than producing a nil pgx.Tx that would
// later NPE inside a SQL call.
func UnwrapPgxTx(tx persistence.TxHandle) pgx.Tx {
	h, ok := tx.(*pgxTxHandle)
	if !ok {
		panic(fmt.Sprintf("infra.UnwrapPgxTx: foreign persistence.TxHandle implementation %T; only the framework's own pgxTxHandle is supported", tx))
	}
	return h.tx
}
