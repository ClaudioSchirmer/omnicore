package handlers

import (
	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/domain"
)

// FindByIDQueryHandler is the generic read handler for by-id lookups.
// Delegates ToCriteria(ctx) to the Query — the same DTO that drives
// FindByParamsQueryHandler, so a single Query type owns its persistence
// shape end to end. ReadByID honors criteria.Filter (security overlays
// from AppContext, e.g. tenant id) merged with the {_id: id} +
// deleted_at gate; the pagination knobs on the same DTO are ignored by
// design (they only make sense on a paged read).
//
// On a hit, the document is filled into a TResult (ResultFromDoc) and
// passed through the Query's FromQueryResult hook — the handler returns the
// typed value, never the raw document.
//
// Returns a *DomainError carrying RecordNotFoundNotification when the
// ViewReader reports the document does not exist — that becomes a 404 at
// the wire via the kernel's SemanticNotFound mapping. An overlay filter
// that filters the doc out is indistinguishable from absence at this
// layer: both surface as 404.
//
// View is the materialized view name. The NotificationContext of the 404 is
// seeded by q.ContextName(); when the query returns "", the handler falls
// back to View. The Query is the natural owner of that identity — the same
// way BaseRepository derives its ContextName from T on the write side.
//
//	users.Get("/:id", fwweb.QueryByID(d.Pipeline,
//	    requests.FindUserByIDRequest{},
//	    requests.FindUserByIDResponse{}.FromResult,
//	    &handlers.FindByIDQueryHandler[*queries.FindUserByIDQuery, appqueries.FindUserResult]{
//	        Reader: d.ViewReader, View: view.Name(),
//	    }))
//
// The response projection (third constructor arg) is the web-side
// TResult→TResp mapping — typically the Response's FromResult method,
// mirroring the command wrappers' responseProjection seat.
type FindByIDQueryHandler[TQ queries.QueryByID[TResult], TResult any] struct {
	pipeline.PathIDRequired
	Reader queries.ViewReader
	View   string
}

func (h *FindByIDQueryHandler[TQ, TResult]) Handle(ctx *configuration.AppContext, q TQ) (TResult, error) {
	var zero TResult
	RequirePathID(q.PathID().Value(), "FindByIDQueryHandler")
	id := q.PathID().String()
	crit, err := q.ToCriteria(ctx)
	if err != nil {
		return zero, err
	}
	doc, found, err := h.Reader.ReadByID(ctx, h.View, id, crit)
	if err != nil {
		return zero, err
	}
	if !found {
		ctxName := q.ContextName()
		if ctxName == "" {
			ctxName = h.View
		}
		return zero, domain.NotFoundError(ctxName, "id", id)
	}
	r := queries.ResultFromDoc[TResult](doc)
	return q.FromQueryResult(ctx, r)
}
