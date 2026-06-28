package read

import (
	"context"

	"github.com/ClaudioSchirmer/omnicore/infra/audit"
)

// auditQuerier adapts the engine's neutral read seam (db.Querier) to the
// minimal Queryer the audit reader consumes. The audit package declares its own
// Queryer/Rows rather than importing infra/db — db already imports audit for the
// Build*Event helpers, so the reverse edge would cycle — and this thin shim is
// the one place that legitimately sees both. db.Rows satisfies audit.Rows
// method-for-method, so the returned cursor passes straight through.
type auditQuerier struct{ q Querier }

func (a auditQuerier) Query(ctx context.Context, sql string, args ...any) (audit.Rows, error) {
	return a.q.Query(ctx, sql, args...)
}

// NewAuditReader builds the backend-neutral audit reader over a relational
// engine: the engine's Querier runs the SELECTs and its Dialect renders the
// positional placeholders, exactly as the AggregateLoader and the Mongo-view
// registry are wired. The audit trail stores ids as UUID text on every dialect
// (Postgres UUID / MySQL CHAR(36)), so the reader binds them verbatim — the only
// engine-specific input it needs is Dialect().Placeholder, never a value codec.
//
// This is the audit-side parallel of db.NewAggregateLoader: a free constructor
// taking the neutral engine, so a service exposes audit reads with one line and
// the same code path serves whichever backend booted.
func NewAuditReader(eng RelationalEngine) audit.Reader {
	return audit.NewReader(auditQuerier{q: eng.Querier()}, eng.Dialect().Placeholder)
}
