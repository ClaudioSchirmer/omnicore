package persistence

import (
	"context"

	"github.com/google/uuid"
)

// RequestContext is the request-scoped carrier the persistence + audit
// pipelines consume. It is an APPLICATION concern, not a domain one: it
// embeds context.Context (cancellation/timeout that infra forwards to
// pgx) AND the authenticated-principal accessors (actor subject/issuer/
// claims) the audit + event sinks stamp on every emitted artifact.
//
// The domain layer does NOT know this type — domain ports (Reader,
// Writer, Repository) are pure and carry no ctx. The request scope binds
// BELOW the domain port, via ScopedRepository[T].Scope(ctx, opts...),
// where the infra adapter closes over the ctx and the lifecycle hooks.
//
// Concrete implementation: *application/configuration.AppContext.
type RequestContext interface {
	context.Context
	ID() uuid.UUID
	ActorSubject() string
	ActorIssuer() string
	ActorClaims() map[string]any
}

// AnonymousActor is the ActorSubject value returned when no Identity is
// attached to the request (auth disabled, public route, background job,
// test fixture). Exposed so callers and tests can compare against it
// without re-typing the literal.
const AnonymousActor = "anonymous"
