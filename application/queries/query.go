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
//
// The read side mirrors the write side's Result anatomy: the Query declares
// an application-layer Result type (TResult — pure data, no wire tags, the
// twin of a command's Result), the framework fills it from the canonical
// Go-keyed view document (ResultFromDoc), and the Query's FromQueryResult hook
// receives the already-converted value — compute/derive/alter there, or
// return it unchanged. The web layer never sees a raw document: it maps
// TResult to the wire Response via the constructor's response projection,
// exactly like the command wrappers' responseProjection seat.

// QueryWithParams is the role interface consumed by
// handlers.FindByParamsQueryHandler and the QueryWithParams constructors on
// every transport. The handler delegates ToCriteria(ctx) to the Query and
// forwards the result to ViewReader.ReadPage; each returned document is
// filled into a TResult and passed through FromQueryResult. ctx is the
// request-scoped *AppContext — the Query is the only layer below the web
// boundary that may consume it: ToCriteria is where identity-derived
// overlays (tenant id, owner id) layer onto the wire criteria, and
// FromQueryResult is where read-side computation (derived fields, ctx-aware
// shaping) happens.
//
// FromQueryResult is MANDATORY — the symmetric twin of a command's FromEntity.
// The framework hands it the TResult already filled from the canonical
// document; the trivial implementation returns it unchanged:
//
//	func (q FindUsersQuery) FromQueryResult(_ *configuration.AppContext, r FindUsersResult) (FindUsersResult, error) {
//	    return r, nil
//	}
//
// Why a separate type from the Request DTO: Request lives in web/ and owns
// wire format (query-string tags); Query lives in application/ and owns
// vocabulary (criteria, security filters injected from AppContext) plus the
// Result projection. TResult owns field EXISTENCE on the read side — a
// field absent from TResult can never reach any wire surface.
type QueryWithParams[TResult any] interface {
	pipeline.Query
	ToCriteria(ctx *configuration.AppContext) (ReadCriteria, error)
	FromQueryResult(ctx *configuration.AppContext, r TResult) (TResult, error)
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
// owns its persistence shape end to end. The wire half arrives the same way
// it does on the paged side: the wrapper hands the parsed ReadCriteria to
// the Request's ToQuery(criteria), the Query stores it, and ToCriteria
// returns it (plus overlays) — a by-id read's wire vocabulary is just
// smaller, exactly one control. Limit/Sort/After/Before/Search/Projection on
// the returned criteria are ignored by ReadByID by design.
//
// FromQueryResult mirrors QueryWithParams — mandatory, receives the TResult the
// framework filled from the document, returns the (possibly adjusted) value.
//
// ContextName() seeds the NotificationContext of the 404 emitted when the
// document is missing — returning a non-empty value lets the read side
// align with the singular domain identity the write side already produces
// (e.g. "User") instead of the view/collection name. Returning "" lets the
// handler fall back to the view name.
type QueryByID[TResult any] interface {
	pipeline.Query
	SetPathID(id string)
	PathID() domain.ID
	ToCriteria(ctx *configuration.AppContext) (ReadCriteria, error)
	FromQueryResult(ctx *configuration.AppContext, r TResult) (TResult, error)
	ContextName() string
}

// QueryByIDBase is the embeddable helper that satisfies the id-carrying
// half of QueryByID (the query still implements ToCriteria, FromQueryResult and
// ContextName). Mirrors pipeline.CommandByIDBase on the write side; stores
// the path id as a domain.ID and exposes it to handlers via PathID().
//
//	type FindUserByIDQuery struct {
//	    queries.QueryByIDBase
//	    Criteria queries.ReadCriteria
//	}
type QueryByIDBase struct {
	pipeline.QueryBase
	id domain.ID
}

func (q *QueryByIDBase) SetPathID(id string) { q.id = domain.NewID(id) }
func (q QueryByIDBase) PathID() domain.ID    { return q.id }
