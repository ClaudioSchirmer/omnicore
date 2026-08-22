package queries

import (
	"context"

	"github.com/ClaudioSchirmer/omnicore/application/exception"
	"github.com/ClaudioSchirmer/omnicore/application/notifications"
)

// OrderByField is one ordering term in a ReadCriteria.
// Desc=true → descending; false → ascending.
type OrderByField struct {
	Field string
	Desc  bool
}

// ReadCriteria is the transport-agnostic input for a paged read.
// Filter values are passed through to the underlying ViewReader as-is.
//
// Projection is the store-neutral field selection, keyed by Go field path (see
// the Projection type). Its zero value is the whole document. It names no store's
// convention: how an include-list is rendered — and what a backing does about its
// own identity key when the caller did not ask for the id — is that backing's
// private business, decided below the read seam.
//
// OnlyTotal switches the read into the only-total mode: the implementation
// runs the underlying store's count primitive against Filter (with the
// archived gate + Search still honored) and returns Page{OnlyTotal: true,
// TotalCount: n} — no items materialized, no cursor walk, no projection
// applied. Limit/OrderBy/After/Before/Projection are IGNORED when
// OnlyTotal=true. The canonical control gateway (queryschema.ValidateControls)
// rejects requests that combine onlyTotal with any of those controls before
// the handler runs (400 SchemaViolationNotification), so the application
// layer never observes a contradictory criteria; the reader's own "ignore"
// posture is defense in depth. Filter leaves + Search + IncludeArchived
// remain valid in only-total mode — the use case is "how many docs match
// this filtered subset".
type ReadCriteria struct {
	Filter     map[string]any
	OrderBy    []OrderByField
	Projection Projection
	// Limit is the internal page size. The wire speaks the Relay pair — a
	// `first=N` maps to Limit=N (forward), a `last=N` maps to Limit=N with
	// Backward=true — and the directional exclusivity is enforced by the
	// control gateway before the criteria is built.
	Limit  int64
	After  string
	Before string
	// Backward requests the page that PRECEDES the window in canonical order —
	// the keyset walk runs in inverted sort order and the slice is restored to
	// canonical order before returning. It is the explicit direction signal
	// every surface sets for `last` — `last=N` with no cursor yields the LAST
	// N of the set; with a Before cursor, the N rows before that edge. The
	// reader also infers backward from a non-empty Before cursor (defense in
	// depth). Ignored when OnlyTotal=true (like Limit/After/Before).
	Backward        bool
	Search          string
	IncludeArchived bool
	OnlyTotal       bool

	// BypassMaxLimit skips the per-view page-size ceiling enforcement in
	// ReadPage and uses Limit verbatim. It is for trusted internal callers that
	// enforce their OWN, operator-set ceiling — the tabular-export wrapper sets
	// Limit to the resolved maxExportRows (which is deliberately larger than the
	// page-read MaxLimit) and ignores the user's `?first`/`?last`. It is never
	// set from a wire parameter, so a client cannot use it to lift the ceiling.
	BypassMaxLimit bool
}

// Restrict removes a field from this read entirely — it is not projected (so it
// never surfaces in the JSON or the tabular export, header included), not sorted
// by, and not filtered on. goFieldPath is the Go field path ("Salary",
// "Addresses.ZipCode"). It is the field-level read-authorization primitive a
// Query calls inside ToCriteria after deciding (from the AppContext identity)
// that the caller may not see the field.
//
// If the request ACTIVELY referenced the field — an ?orderBy=, a filter, or an
// explicit ?fields= on it — Restrict returns a 403 *ApplicationError
// (FieldAccessForbiddenNotification): the caller tried to use a field it may not
// see, so refusing is more honest than silently ignoring the knob — and it
// closes the inference leak a silently-dropped sort/filter would leave. A
// passive read (the field simply not requested) gets the silent omission. The
// field is scrubbed from the criteria either way, so nothing leaks even on the
// 403 path.
func (c *ReadCriteria) Restrict(goFieldPath string) error {
	active := c.referencesField(goFieldPath)
	c.scrubField(goFieldPath)
	if active {
		return exception.SingleNotificationError("Query", goFieldPath, notifications.FieldAccessForbiddenNotification{})
	}
	return nil
}

// referencesField reports whether the request actively named the field — in an
// ordering term, a filter clause, or an explicit ?fields= inclusion.
func (c *ReadCriteria) referencesField(goFieldPath string) bool {
	if _, ok := c.Filter[goFieldPath]; ok {
		return true
	}
	if c.Projection.IsInclusion() && c.Projection.Selects(goFieldPath) {
		return true
	}
	for _, s := range c.OrderBy {
		if s.Field == goFieldPath {
			return true
		}
	}
	return false
}

// scrubField removes the field from the projection (mode-aware), the ordering,
// and the filter so it reaches neither the store nor the wire.
func (c *ReadCriteria) scrubField(goFieldPath string) {
	c.Projection.Drop(goFieldPath)
	if len(c.OrderBy) > 0 {
		kept := c.OrderBy[:0]
		for _, s := range c.OrderBy {
			if s.Field != goFieldPath {
				kept = append(kept, s)
			}
		}
		c.OrderBy = kept
	}
	delete(c.Filter, goFieldPath)
}

// Page is the transport-agnostic result of a paged read. The wire wrapper
// (web.QueryWithParams) decomposes Page into Response.Data (items) and
// Response.Pagination (cursor envelope), so Page itself is not marshalled
// directly and carries no json tags. Field names follow the Relay framing —
// the cursors are WINDOW EDGES (StartCursor/EndCursor address the first and
// last row of THIS page; echo them into `before`/`after` to walk), and
// HasNextPage/HasPreviousPage state whether rows exist beyond each edge.
//
// An edge cursor is emitted ONLY where its neighbouring page exists: EndCursor
// is set exactly when HasNextPage, StartCursor exactly when HasPreviousPage.
// So the first page of a forward walk carries no StartCursor and the last page
// carries no EndCursor — the pair never contradicts the flag beside it, and a
// consumer can treat an empty edge cursor as "nothing to walk to" on either
// side. Every reader obeys this, so flipping a view's backing (projected Mongo
// ⇄ relational) leaves the envelope shape unchanged. Per-ROW addressing
// is ItemCursors, which is populated for every returned row regardless.
//
// OnlyTotal=true signals that the upstream ReadCriteria asked for the
// only-total mode — Items/HasNextPage/HasPreviousPage/StartCursor/EndCursor
// are zero by construction, only TotalCount carries information. The wire
// wrapper reads this flag to emit a dedicated envelope shape (pagination =
// {totalCount: n}, no data field) instead of the regular listing envelope.
type Page struct {
	Items           []map[string]any
	HasNextPage     bool
	HasPreviousPage bool
	StartCursor     string
	EndCursor       string
	TotalCount      int64
	OnlyTotal       bool

	// ItemCursors is the per-row keyset cursor, positionally aligned with
	// Items (ItemCursors[i] addresses Items[i]). It exists for transports that
	// need a cursor per element rather than only the page-edge StartCursor /
	// EndCursor — the GraphQL endpoint's Relay connection populates
	// edges[].cursor from it. The cursor is built from the same keyset tuple +
	// context hash the edge cursors use, so it round-trips through the reader's
	// ?after / ?before path unchanged.
	//
	// It MUST be built by the reader (the layer that owns the physical keyset
	// tuple — the ordering-field values and _id are stripped from the returned
	// Go-field-keyed Items, so no upper layer can reconstruct it). The REST
	// wrapper ignores this field; it stays nil for only-total reads.
	ItemCursors []string

	// Projection is the effective field selection the read used — the
	// post-ToCriteria ReadCriteria.Projection echoed back. The tabular-export
	// wrapper prunes its column plan to this (export.Plan.PruneToProjection), so a
	// field a Query removed from the criteria (e.g. via ReadCriteria.Restrict)
	// disappears from the CSV/XLSX columns — header included — not just from the
	// JSON, which keeps ToCriteria the single source of truth for which fields
	// surface across all formats. Empty/nil = whole-doc read (every column).
	Projection Projection
}

// ViewReader is the read-side port of CQRS. Implementations live in infra
// (e.g. MongoViewReader) and adapt the store's native types to the plain
// map[string]any documents the application layer consumes.
//
// Both ReadPage and ReadByID accept ReadCriteria so a Query owns its
// persistence shape end to end. ReadByID honors criteria.Filter (security
// overlays from AppContext, e.g. tenant id) merged with the {_id: id} +
// deleted_at gate. The pagination knobs on ReadCriteria
// (Limit/OrderBy/After/Before/Search/Projection) are ignored by ReadByID by
// design — they only make sense on a paged read.
type ViewReader interface {
	ReadPage(ctx context.Context, view string, criteria ReadCriteria) (Page, error)
	ReadByID(ctx context.Context, view, id string, criteria ReadCriteria) (map[string]any, bool, error)
}
