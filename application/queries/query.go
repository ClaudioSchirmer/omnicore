package queries

import (
	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/domain"
)

// Route-shape vocabulary, read side. Every transport constructor is named
// after one of the canonical query shapes — QueryWithParams, QueryByID —
// and each shape pairs with the base of the same name plus the Base suffix:
// embed the base, satisfy the constraint. Mirrors the command shapes in
// application/pipeline.

// QueryWithParams is the role interface consumed by
// handlers.FindByParamsQueryHandler and the QueryWithParams constructors on
// every transport. The handler delegates ToCriteria(ctx) to the Query and
// forwards the result to ViewReader.ReadPage. ctx is the request-scoped
// *AppContext — the Query is the only layer below the web boundary that may
// consume it, and it is where identity-derived overlays (tenant id, owner
// id) layer onto the wire criteria.
//
// Why a separate type from the Request DTO: Request lives in web/ and owns
// wire format (query-string tags); Query lives in application/ and owns
// vocabulary (criteria, security filters injected from AppContext).
type QueryWithParams interface {
	pipeline.Query
	ToCriteria(ctx *configuration.AppContext) (ReadCriteria, error)
}

// QueryWithParamsBase is the base to embed on QueryWithParams
// implementations. Typical usage:
//
//	type FindUserByParamsQuery struct {
//	    queries.QueryWithParamsBase
//	    Criteria queries.ReadCriteria
//	}
type QueryWithParamsBase = pipeline.QueryBase

// QueryByID is the role interface consumed by handlers.FindByIDQueryHandler
// and the QueryByID constructors on every transport, which inject
// q.SetPathID(id) after the mapper and before dispatch. Symmetric to
// pipeline.CommandByID on the write side (same SetPathID/PathID pair; the
// read side stores a domain.ID).
//
// ToCriteria(ctx) carries both IncludeArchived and any AppContext-derived
// overlay (tenant id, owner id) the consumer wants applied to the by-id
// lookup — the same DTO that drives QueryWithParams, so a single Query type
// owns its persistence shape end to end. Limit/Sort/After/Before/Search/
// Projection on the returned criteria are ignored by ReadByID by design.
//
// ContextName() seeds the NotificationContext of the 404 emitted when the
// document is missing — returning a non-empty value lets the read side
// align with the singular domain identity the write side already produces
// (e.g. "User") instead of the view/collection name. Returning "" lets the
// handler fall back to the view name.
type QueryByID interface {
	pipeline.Query
	SetPathID(id string)
	PathID() domain.ID
	ToCriteria(ctx *configuration.AppContext) (ReadCriteria, error)
	ContextName() string
}

// QueryByIDBase is the embeddable helper that satisfies the id-carrying
// half of QueryByID (the query still implements ToCriteria and
// ContextName). Mirrors pipeline.CommandByIDBase on the write side; stores
// the path id as a domain.ID and exposes it to handlers via PathID().
//
//	type FindUserByIDQuery struct {
//	    queries.QueryByIDBase
//	    Archived bool
//	}
type QueryByIDBase struct {
	pipeline.QueryBase
	id domain.ID
}

func (q *QueryByIDBase) SetPathID(id string) { q.id = domain.NewID(id) }
func (q QueryByIDBase) PathID() domain.ID    { return q.id }
