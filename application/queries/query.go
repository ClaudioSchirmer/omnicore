package queries

import (
	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/domain"
)

// QueryWithID is the contract for Queries that receive the ID via the URL
// path. Implemented through embedding QueryBaseWithID. Consumed by
// fwweb.HandleQueryWithID, which injects q.SetPathID(c.Params("id")) before
// dispatch. Symmetric to pipeline.CommandWithID on the write side.
type QueryWithID interface {
	pipeline.Query
	SetPathID(id string)
	GetID() domain.ID
}

// QueryBaseWithID is the embeddable helper that satisfies QueryWithID.
// Mirrors pipeline.CommandBaseWithID on the write side; stores the path id
// as a domain.ID and exposes it to handlers via GetID().
//
//	type FindUserByIDQuery struct {
//	    queries.QueryBaseWithID
//	    Archived bool
//	}
type QueryBaseWithID struct {
	pipeline.QueryBase
	id domain.ID
}

func (q *QueryBaseWithID) SetPathID(id string) { q.id = domain.NewID(id) }
func (q QueryBaseWithID) GetID() domain.ID     { return q.id }

// FindByParamsQuery is the role interface consumed by
// handlers.FindByParamsQueryHandler. The handler delegates ToCriteria(ctx) to
// the Query and forwards the result to ViewReader.ReadPage. ctx is the
// request-scoped *AppContext — the Query is the only layer below the web
// boundary that may consume it, and it is where identity-derived overlays
// (tenant id, owner id) layer onto the wire criteria.
//
// Why a separate type from the Request DTO: Request lives in web/ and owns
// wire format (query-string tags); Query lives in application/ and owns
// vocabulary (criteria, security filters injected from AppContext).
type FindByParamsQuery interface {
	pipeline.Query
	ToCriteria(ctx *configuration.AppContext) (ReadCriteria, error)
}

// FindByIDQuery is the role interface consumed by
// handlers.FindByIDQueryHandler. ToCriteria(ctx) carries both
// IncludeArchived and any AppContext-derived overlay (tenant id, owner id)
// the consumer wants applied to the by-id lookup — the same DTO that drives
// FindByParamsQuery, so a single Query type owns its persistence shape end
// to end. Limit/Sort/After/Before/Search/Projection on the returned criteria
// are ignored by ReadByID by design.
//
// ContextName() seeds the NotificationContext of the 404 emitted when the
// document is missing — returning a non-empty value lets the read side
// align with the singular domain identity the write side already produces
// (e.g. "User") instead of the view/collection name. Returning "" lets the
// handler fall back to the view name.
type FindByIDQuery interface {
	QueryWithID
	ToCriteria(ctx *configuration.AppContext) (ReadCriteria, error)
	ContextName() string
}
