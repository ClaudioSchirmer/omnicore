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
//	users.Get("/:id", fwweb.HandleQueryWithID(d.Pipeline,
//	    requests.FindUserByIDRequest{},
//	    requests.FindUserByIDResponse{}.FromDoc,
//	    &handlers.FindByIDQueryHandler[*queries.FindUserByIDQuery]{
//	        Reader: d.ViewReader, View: view.Name(),
//	    }))
//
// The projector (third arg) is mandatory — pass fwresponses.RawDoc to keep
// the raw view doc shape on the wire, or a consumer-defined R{}.FromDoc to
// declare a typed wire contract.
type FindByIDQueryHandler[Q queries.FindByIDQuery] struct {
	pipeline.PathIDRequired
	Reader queries.ViewReader
	View   string
}

func (h *FindByIDQueryHandler[Q]) Handle(ctx *configuration.AppContext, q Q) (map[string]any, error) {
	RequirePathID(q.GetID().Value(), "FindByIDQueryHandler")
	id := q.GetID().String()
	crit, err := q.ToCriteria(ctx)
	if err != nil {
		return nil, err
	}
	doc, found, err := h.Reader.ReadByID(ctx, h.View, id, crit)
	if err != nil {
		return nil, err
	}
	if !found {
		ctxName := q.ContextName()
		if ctxName == "" {
			ctxName = h.View
		}
		return nil, domain.NotFoundError(ctxName, "id", id)
	}
	return doc, nil
}
