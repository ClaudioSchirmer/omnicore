package persistence

import (
	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/domain"
)

// ScopedRepository is the application-layer port the Auto Command Handlers
// and the canonical manual path consume. It carries the domain read port
// directly (domain.Reader[T] — FindByID / New, pure and ctx-less) PLUS a
// Scope method that binds the request-scoped concerns and hands back a
// pure domain.Writer ready to persist a ValidEntity.
//
// Why the split between read and write:
//
//   - The domain ports (Reader, Writer, Repository) are pure — they carry
//     no context, no actor, no lifecycle hooks. Those are infrastructure
//     concerns and must not appear in a domain signature.
//   - Reads carry no request-scoped concern, so the handle exposes them
//     directly (FindByID / New).
//   - Writes need the request ctx (cancellation → pgx; actor → audit) and
//     the optional in-TX lifecycle hooks. Scope(ctx, opts...) binds both
//     and returns a domain.Writer whose Insert/Update/… are pure
//     (ValidEntity in, outcome out). The domain port never pronounces the
//     ctx; the infra adapter the Scope returns closes over it.
//
// infra.BaseRepository[T] implements Scope; the consumer's repository
// (embedding BaseRepository + a loader that provides FindByID) satisfies
// ScopedRepository[T] without extra code. A consumer that declares a richer
// domain port (e.g. domain.UserRepository with FindByEmail) pairs it with a
// matching scopable handle whose Scope returns that richer domain port.
type ScopedRepository[T any] interface {
	domain.Reader[T]
	Scope(ctx *configuration.AppContext, opts ...WriteOption[T]) domain.Writer
}
