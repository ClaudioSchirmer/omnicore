package handlers

import (
	"github.com/ClaudioSchirmer/omnicore/application/persistence"
)

// collectWriteOptions detects the Cmd's optional AfterBeginHookProvider[T]
// / BeforeCommitHookProvider[T] implementations and folds the matching
// method values into the persistence.WriteOption[T] variadic the Writer
// consumes. Returns nil when the Cmd implements neither provider — the
// Writer's variadic absorbs the empty slice without allocation.
//
// Called from the top of every Auto Command Handler that fires a write
// (Insert / Update / PartialUpdate / Archive / Unarchive / Delete). The
// two type assertions cost roughly 10ns each on amd64 — negligible
// against the surrounding DB roundtrip.
//
// The function is generic on both T (the entity) and Cmd (the command
// type) so the type assertion against `any(cmd)` resolves the
// constrained Cmd to its concrete value without forcing the call site
// to do the conversion. Each Auto handler still passes the result to
// the matching Writer call as `opts...`.
func collectWriteOptions[T any, Cmd any](cmd Cmd) []persistence.WriteOption[T] {
	var opts []persistence.WriteOption[T]
	if p, ok := any(cmd).(persistence.AfterBeginHookProvider[T]); ok {
		opts = append(opts, persistence.WithAfterBegin[T](p.AfterBegin))
	}
	if p, ok := any(cmd).(persistence.BeforeCommitHookProvider[T]); ok {
		opts = append(opts, persistence.WithBeforeCommit[T](p.BeforeCommit))
	}
	return opts
}
