package handlers

import (
	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/queries"
)

// FindByParamsQueryHandler is the generic read handler for paged listings.
// It delegates ToCriteria(ctx) to the Query — the Query is the only layer
// below the web boundary that may consume *AppContext, and ToCriteria is
// where identity-derived overlays (tenant id, owner id) layer onto the
// wire criteria — and forwards the result to the configured ViewReader's
// ReadPage.
//
// View is the materialized view name (Mongo collection in the canonical
// MongoViewReader). Provided at construction so a single Query type can
// serve multiple views by wiring different handlers.
//
//	users.Get("/", fwweb.QueryWithParams(d.Pipeline,
//	    requests.FindUsersByParamsRequest{},
//	    requests.FindUsersByParamsResponse{}.FromDoc,
//	    &handlers.FindByParamsQueryHandler[*queries.FindUserByParamsQuery]{
//	        Reader: d.ViewReader, View: view.Name(),
//	    }))
//
// The projector (third arg) is mandatory — pass fwresponses.RawDoc to keep
// the raw view doc shape on the wire, or a consumer-defined R{}.FromDoc to
// declare a typed wire contract.
type FindByParamsQueryHandler[Q queries.QueryWithParams] struct {
	Reader queries.ViewReader
	View   string
}

func (h *FindByParamsQueryHandler[Q]) Handle(ctx *configuration.AppContext, q Q) (queries.Page, error) {
	crit, err := q.ToCriteria(ctx)
	if err != nil {
		return queries.Page{}, err
	}
	return h.Reader.ReadPage(ctx, h.View, crit)
}
