package domain

import (
	"context"

	"github.com/google/uuid"
)

// Context is the minimal contract that any request-scoped context must
// satisfy to participate in the persistence + audit pipelines.
//
// Concrete implementation: *application/configuration.AppContext.
//
// Embeds context.Context so the same value carries the Go cancellation
// signal AND the request-scoped audit fields (ID + actor + claims). The
// persistence layer (infra.Postgres) passes the value to pgx unchanged,
// propagating client disconnects + request timeouts down to the database
// driver without each handler having to forward a separate ctx.
//
// The actor methods (ActorSubject / ActorIssuer / ActorClaims) carry the
// authenticated principal of the current request into the audit and event
// pipelines so logs answer "who did this" without each handler having to
// thread the JWT data manually. When the request is unauthenticated (auth
// disabled, public route, background job, test fixture), ActorSubject
// returns the sentinel "anonymous" while ActorIssuer and ActorClaims return
// their empty values — log consumers can filter on `actor:anonymous` to
// surface unauthenticated writes.
type Context interface {
	context.Context
	ID() uuid.UUID
	ActorSubject() string
	ActorIssuer() string
	ActorClaims() map[string]any
}

// AnonymousActor is the ActorSubject value returned when no Identity is
// attached to the context. Exposed so callers and tests can compare against
// it without re-typing the literal.
const AnonymousActor = "anonymous"
