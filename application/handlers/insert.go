package handlers

import (
	"fmt"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/persistence"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/domain"
)

// InsertCommandHandler is the generic handler for Insert of Entity T.
// Cmd hydrates the entity via ToEntity(ctx); the framework calls
// domain.GetInsertable (which triggers validations via BuildRules) and
// Repo.Insert(ctx, insertable, opts...), sets the persisted ID back on
// the entity, then asks Cmd to project the entity into TResult via
// FromEntity(ctx, entity).
//
// TResult is the application-layer projection the wire layer will render. For
// endpoints that do not want to expose anything beyond the success envelope,
// declare TResult = results.None and have FromEntity return results.None{}.
//
// ToEntity AND FromEntity both receive the request *AppContext — the Command
// is the only layer below the web boundary allowed to consume ctx, and the
// symmetry at input + output lets identity-derived translation happen in one
// place. Domain entity sees only business-named fields.
//
// In-TX side effects: when Cmd implements persistence.AfterBeginHookProvider[T]
// or persistence.BeforeCommitHookProvider[T], the handler detects each via
// type assertion at the top of Handle and threads the matching closure to
// Repo.Insert as a WriteOption[T]. Slot semantics are documented on
// persistence.AfterBeginHook[T] / BeforeCommitHook[T] — both fire INSIDE the
// persister's TX (positions A and D); a non-nil error rolls the TX back and
// the NotificationCarrier identity propagates verbatim to the wire envelope.
// Cmds that need neither slot pay only the two type-assertions per request
// (~10ns each, negligible).
//
// Service is optional — when the entity declares RequiresService() true,
// BuildRules receives the Service injected here; leave nil to keep the
// default behavior for handlers that do not need a domain service.
type InsertCommandHandler[T domain.Entity, Cmd pipeline.InsertCommand[T, TResult], TResult any] struct {
	Repo    persistence.ScopedRepository[T]
	Service domain.Service
}

func (h *InsertCommandHandler[T, Cmd, TResult]) Handle(ctx *configuration.AppContext, cmd Cmd) (TResult, error) {
	var zero TResult
	// Direction 1 of the SharedBase marriage: a SharedBase-backed entity is an
	// upsert (load-first) and must not go through the blind insert path.
	if _, ok := any(h.Repo).(persistence.SharedBaseInsertLoader[T]); ok {
		return zero, fmt.Errorf(
			"InsertCommandHandler: %T has a SharedBase — use SharedBaseInsertCommandHandler", h.Repo)
	}
	entity, err := cmd.ToEntity(ctx)
	if err != nil {
		return zero, err
	}
	insertable, err := domain.GetInsertable(entity, persistence.ScopeService(h.Service, ctx), "GetInsertable")
	if err != nil {
		return zero, err
	}
	opts := collectWriteOptions[T, Cmd](cmd)
	id, err := h.Repo.Scope(ctx, opts...).Insert(insertable)
	if err != nil {
		return zero, err
	}
	entity.SetID(id)
	result, err := cmd.FromEntity(ctx, entity)
	if err != nil {
		return zero, err
	}
	return result, nil
}
