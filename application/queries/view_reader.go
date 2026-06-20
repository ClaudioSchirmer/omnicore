package queries

import "context"

// SortField is one sort term in a ReadCriteria.
// Desc=true → descending; false → ascending.
type SortField struct {
	Field string
	Desc  bool
}

// ReadCriteria is the transport-agnostic input for a paged read.
// Filter values are passed through to the underlying ViewReader as-is.
//
// Projection carries the per-field include (value 1) or exclude (value 0)
// flags Mongo accepts. Empty map → no projection (whole doc). When the
// caller wants the response to drop `_id` (the wrapper does this when the
// consumer-requested `fields` list does not include `id`), it adds an
// explicit `_id: 0` entry alongside the include-1 entries — Mongo allows
// `_id: 0` mixed with include-1 entries (the single permitted exclusion in
// otherwise-inclusion projections).
//
// OnlyTotal switches the read into a count-only mode: the implementation
// runs the underlying store's count primitive against Filter (with the
// archived gate + Search still honored) and returns Page{OnlyTotal: true,
// Total: n} — no items materialized, no cursor walk, no projection applied.
// Limit/Sort/After/Before/Projection are IGNORED when OnlyTotal=true. The
// wire wrapper rejects requests that combine onlyTotal=true with any of
// those parameters at the schema layer (400 SchemaViolationNotification),
// so the application layer never observes a contradictory criteria; the
// reader's own "ignore" posture is defense in depth. Filter leaves +
// Search + IncludeArchived remain valid in count mode — the use case is
// "how many docs match this filtered subset".
type ReadCriteria struct {
	Filter          map[string]any
	Sort            []SortField
	Projection      map[string]int
	Limit           int64
	After           string
	Before          string
	Search          string
	IncludeArchived bool
	OnlyTotal       bool

	// BypassMaxLimit skips the per-view `?limit=` ceiling enforcement in
	// ReadPage and uses Limit verbatim. It is for trusted internal callers that
	// enforce their OWN, operator-set ceiling — the tabular-export wrapper sets
	// Limit to the resolved maxExportRows (which is deliberately larger than the
	// page-read MaxLimit) and ignores the user's `?limit`. It is never set from
	// a wire parameter, so a client cannot use it to lift the page ceiling.
	BypassMaxLimit bool
}

// Page is the transport-agnostic result of a paged read. The wire wrapper
// (web.HandleQueryWithParams) decomposes Page into Response.Data (items) and
// Response.Pagination (cursor envelope), so Page itself is not marshalled
// directly and carries no json tags.
//
// OnlyTotal=true signals that the upstream ReadCriteria asked for the
// count-only mode — Items/HasNext/HasPrev/NextCursor/PrevCursor are zero
// by construction, only Total carries information. The wire wrapper
// reads this flag to emit a dedicated envelope shape (pagination =
// {total: n}, no data field) instead of the regular listing envelope.
type Page struct {
	Items      []map[string]any
	HasNext    bool
	HasPrev    bool
	NextCursor string
	PrevCursor string
	Total      int64
	OnlyTotal  bool
}

// ViewReader is the read-side port of CQRS. Implementations live in infra
// (e.g. MongoViewReader) and adapt the store's native types to the plain
// map[string]any documents the application layer consumes.
//
// Both ReadPage and ReadByID accept ReadCriteria so a Query owns its
// persistence shape end to end. ReadByID honors criteria.Filter (security
// overlays from AppContext, e.g. tenant id) merged with the {_id: id} +
// deleted_at gate. The pagination knobs on ReadCriteria
// (Limit/Sort/After/Before/Search/Projection) are ignored by ReadByID by
// design — they only make sense on a paged read.
type ViewReader interface {
	ReadPage(ctx context.Context, view string, criteria ReadCriteria) (Page, error)
	ReadByID(ctx context.Context, view, id string, criteria ReadCriteria) (map[string]any, bool, error)
}
