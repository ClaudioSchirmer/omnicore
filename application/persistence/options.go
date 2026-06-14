package persistence

// WriteOption is a functional option consumed by the Writer[T] port. It
// carries the optional AfterBeginHook[T] / BeforeCommitHook[T] closures
// the persister fires at positions A and D of the TX.
//
// Auto handlers populate options by detecting the Cmd's optional
// AfterBegin / BeforeCommit methods via type assertion against the
// provider interfaces. Manual handlers populate options by passing
// WithAfterBegin / WithBeforeCommit directly at the call site. Both
// paths converge on the same internal writeOptions value the persister
// resolves.
type WriteOption[T any] func(*writeOptions[T])

// writeOptions is the internal accumulator. The persister reads it via
// ResolveWriteOptions so flat and aggregate paths share a single
// resolution helper.
type writeOptions[T any] struct {
	afterBegin   AfterBeginHook[T]
	beforeCommit BeforeCommitHook[T]
}

// WithAfterBegin registers an AfterBeginHook[T] on the Writer call. nil
// fn is silently dropped — keeps Auto handler call sites trivial when a
// Cmd does not implement the provider.
func WithAfterBegin[T any](fn AfterBeginHook[T]) WriteOption[T] {
	return func(o *writeOptions[T]) {
		if fn != nil {
			o.afterBegin = fn
		}
	}
}

// WithBeforeCommit registers a BeforeCommitHook[T] on the Writer call.
// nil fn is silently dropped.
func WithBeforeCommit[T any](fn BeforeCommitHook[T]) WriteOption[T] {
	return func(o *writeOptions[T]) {
		if fn != nil {
			o.beforeCommit = fn
		}
	}
}

// ResolveWriteOptions folds a slice of WriteOption[T] into the
// resolved (afterBegin, beforeCommit) pair. Exposed so infra adapters
// can resolve once per call without re-implementing the loop. Last
// non-nil wins for each slot (consistent with the silent-drop policy on
// nil fn).
func ResolveWriteOptions[T any](opts []WriteOption[T]) (AfterBeginHook[T], BeforeCommitHook[T]) {
	var o writeOptions[T]
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	return o.afterBegin, o.beforeCommit
}
