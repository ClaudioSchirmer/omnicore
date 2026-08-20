package handlers

import (
	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/queries"
)

// FindByParamsQueryHandler is the generic read handler for paged listings.
// It delegates ToCriteria(ctx) to the Query — the Query is the only layer
// below the web boundary that may consume *AppContext, and ToCriteria is
// where identity-derived overlays (tenant id, owner id) layer onto the
// wire criteria — forwards the result to the configured ViewReader's
// ReadPage, fills one TResult per returned document (ResultFromDoc) and
// passes each through the Query's FromQueryResult hook. The typed PageOf it
// returns is what every transport surface consumes — raw documents never
// leave the application layer.
//
// View is the materialized view name (Mongo collection in the canonical
// MongoViewReader). Provided at construction so a single Query type can
// serve multiple views by wiring different handlers.
//
//	users.Get("/", fwweb.QueryWithParams(d.Pipeline,
//	    requests.FindUsersByParamsRequest{},
//	    requests.FindUsersByParamsResponse{}.FromResult,
//	    &handlers.FindByParamsQueryHandler[*queries.FindUserByParamsQuery, appqueries.FindUsersResult]{
//	        Reader: d.ViewReader, View: view.Name(),
//	    }))
//
// The response projection (third constructor arg) is the web-side
// TResult→TResp mapping — typically the Response's FromResult method,
// mirroring the command wrappers' responseProjection seat.
type FindByParamsQueryHandler[TQ queries.QueryWithParams[TResult], TResult any] struct {
	Reader queries.ViewReader
	View   string
}

// ViewName exposes the view this handler reads. The web wrappers type-assert
// for it at route registration so a declaration made on the Request DTO — one
// that only the VIEW can honor, such as accepting `?search=` — can be checked
// against that view's own declarations at boot, instead of failing on the first
// request that exercises it.
func (h *FindByParamsQueryHandler[TQ, TResult]) ViewName() string { return h.View }

func (h *FindByParamsQueryHandler[TQ, TResult]) Handle(ctx *configuration.AppContext, q TQ) (queries.PageOf[TResult], error) {
	crit, err := q.ToCriteria(ctx)
	if err != nil {
		return queries.PageOf[TResult]{}, err
	}
	page, err := h.Reader.ReadPage(ctx, h.View, crit)
	if err != nil {
		return queries.PageOf[TResult]{}, err
	}
	return queries.PageOfFrom(page, queries.FromQueryResultFiller[TResult](ctx, q))
}
