package audit

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

// ErrAuditNotFound is the sentinel Reader.FindByID returns when no audit_events
// row matches the supplied id. Callers branch with `errors.Is(err,
// ErrAuditNotFound)` to map the miss to whatever transport response shape
// suits them (HTTP 404, empty CLI output, etc.). Transport / SQL failures
// surface as the underlying error wrapped with a "audit: ..." prefix. It lives
// here, beside the port it belongs to, so a consumer reading the audit trail
// through Reader never has to import the infra reader for the miss sentinel.
var ErrAuditNotFound = errors.New("audit: event not found")

// Reader is the backend-neutral audit read port. It is the generic interface
// the transport/application layer depends on; the concrete reader runs over
// whatever relational engine the service booted (Postgres or MySQL), built by
// db.NewAuditReader from the engine's read seam and constructed in infra. The
// audit trail stores ids as UUID text (Postgres UUID / MySQL CHAR(36)) on every
// dialect, so the only engine-specific bit the reader needs is the positional
// placeholder — supplied by the dialect — never a value codec.
//
// The port lives in application/audit (not infra/audit) so an application
// handler reading the audit timeline depends on the abstraction, not the
// concrete infra reader. infra/audit supplies the sole implementation.
type Reader interface {
	// FindByID returns the audit_events row whose id matches the supplied UUID.
	// (nil, ErrAuditNotFound) on miss; (nil, err) on transport failure;
	// (*AuditEvent, nil) on hit.
	FindByID(ctx context.Context, id uuid.UUID) (*AuditEvent, error)
	// FindByAggregate returns the audit_events rows of one aggregate, newest
	// first, capped at limit. An aggregate with no rows yields an empty
	// (non-nil) slice + nil.
	//
	// limit must be positive: the cap is rendered into the SQL by the dialect,
	// so every timeline read leaves the database bounded — the read side's rule
	// that no unbounded page ever reaches an engine, applied here too. A
	// non-positive limit is a programming error at the caller and is refused.
	// Callers resolving the cap from configuration (the framework's own audit
	// endpoint reads audit.endpoint.maxLimit) pass the resolved value; a
	// caller holding no configuration picks its own explicit ceiling.
	FindByAggregate(ctx context.Context, entityType, aggregateID string, limit int) ([]*AuditEvent, error)
}
