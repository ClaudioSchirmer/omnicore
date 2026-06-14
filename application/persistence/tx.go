// Package persistence holds the application-layer port types for the
// persistence pipeline.
//
// The package carries three families of declarations consumed across the
// stack:
//
//   - TxHandle — sealed marker handed to in-TX lifecycle hooks. The
//     hook never reads SQL through it: TxHandle exposes no public
//     methods, only a private sealing method that pins the implementation
//     to the framework's infra adapter. Application code receives the
//     handle and threads it to a port; the port's adapter in infra/
//     unwraps the underlying pgx.Tx via infra.UnwrapPgxTx and owns the
//     SQL + table name. Sealing the interface prevents any code path
//     where the application layer writes SQL.
//   - AfterBeginHook[T] / BeforeCommitHook[T] — function types declaring the
//     hook shape. Slot semantics: afterBegin fires INSIDE the TX before any
//     framework write; beforeCommit fires INSIDE the TX after all framework
//     writes (data + outbox + audit) and before COMMIT. A non-nil error
//     returned by either rolls the whole TX back; type identity preserved
//     end-to-end so domain.NotificationCarrier reaches the wire envelope
//     verbatim.
//   - AfterBeginHookProvider[T] / BeforeCommitHookProvider[T] — interfaces
//     detected by the Auto Command Handlers via type assertion. A Cmd that
//     declares AfterBegin / BeforeCommit satisfies the matching provider
//     automatically; the handler reads the methods and forwards them to the
//     Writer as WriteOption[T] closures.
//   - WriteOption[T] / WithAfterBegin / WithBeforeCommit — functional
//     options consumed by the Writer[T] port. Auto and manual handlers both
//     end up at the same Writer call; the only difference is how the
//     closures originate (Cmd method vs explicit closure).
//   - Writer[T] — typed write port that carries the variadic options. The
//     domain.Repository[T] interface still describes the read+write
//     contract pure to the domain layer; Writer[T] is the
//     application-layer surface infra.BaseRepository[T] also implements so
//     the Auto + manual handlers can pass the variadic without pulling
//     application/persistence imports into the domain.
package persistence

// TxHandle is the sealed marker handed to in-TX lifecycle hooks. The
// interface intentionally carries no public methods — application code
// cannot read or write through it directly. The canonical shape for an
// in-TX side effect is:
//
//  1. Declare a port in application/ (or domain/) — a pure Go interface
//     whose method receives a persistence.TxHandle parameter.
//  2. Implement the port in infra/ — the adapter calls
//     infra.UnwrapPgxTx(tx) to recover the underlying pgx.Tx and owns
//     the SQL + table name.
//  3. Inject the port on the Cmd (constructor / wire) and call it from
//     the hook closure. The framework's TX is honored end to end; the
//     application layer never pronounces SQL.
//
// The sealing method (txHandle) is unexported, so only types embedding
// SealedTxHandle (declared in this same package) satisfy the interface.
// The framework's own infra/pgxTxHandle embeds SealedTxHandle to inherit
// the method via promotion — a Go-idiomatic seal that prevents foreign
// implementations without forcing the concrete adapter to live in this
// package (the adapter imports pgx, which application/ must not).
//
// Any future transport (a Mongo-aware TxHandle, an in-memory test handle)
// plugs in by embedding SealedTxHandle on its own adapter struct — the
// application contract stays unchanged.
type TxHandle interface {
	// txHandle is the sealing method. Unexported on purpose: only types
	// embedding SealedTxHandle (defined in this package) satisfy it.
	txHandle()
}

// SealedTxHandle is the embed token framework adapters use to satisfy
// TxHandle. The unexported sealing method is defined here so that any
// type embedding SealedTxHandle inherits it via promotion — Go's
// canonical "package-private interface satisfier" pattern. Callers
// outside the framework cannot embed this type into a TxHandle of their
// own without also writing an infra-layer adapter, by convention; the
// type exists to keep the seal mechanical, not to be a public surface.
type SealedTxHandle struct{}

// txHandle satisfies the TxHandle marker. The method is intentionally a
// no-op; its existence is the proof that an embedding type is a
// framework-issued TxHandle.
func (SealedTxHandle) txHandle() {}
