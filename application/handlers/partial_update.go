package handlers

import (
	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/persistence"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/domain"
)

// PartialUpdateCommandHandler runs Repo.FindByID +
// domain.GetPartialUpdatable(current, ApplyPartiallyTo, …) +
// Repo.Update(ctx, updatable, opts...) + cmd.FromEntity(ctx, current).
// PATCH-shaped — does NOT embed pipeline.FullBody, so the wrapper does not
// enforce field presence.
//
// The handler binds the request *AppContext into a closure wrapping
// cmd.ApplyPartiallyTo, so the apply callback received by GetPartialUpdatable
// still has the `func(T)` shape the domain expects. Same ctx semantics as
// UpdateCommandHandler — the Command consumes ctx for any identity-derived
// transient field population that BuildRules' IfUpdate will validate, and
// FromEntity gets ctx + the cmd itself for the projection step.
//
// In-TX side effects: same provider-interface detection as
// UpdateCommandHandler — Cmd implementing AfterBeginHookProvider[T] /
// BeforeCommitHookProvider[T] threads the matching closures as
// persistence.WriteOption[T]. Persister's Update slot is shared between
// PUT and PATCH (same SQL fingerprint, single firing position).
//
// TResult is the application-layer projection the wire layer will render
// after the partial update completes.
type PartialUpdateCommandHandler[T domain.Entity, Cmd pipeline.PartialUpdateCommand[T, TResult], TResult any] struct {
	pipeline.PathIDRequired
	Repo    persistence.ScopedRepository[T]
	Service domain.Service
}

func (h *PartialUpdateCommandHandler[T, Cmd, TResult]) Handle(ctx *configuration.AppContext, cmd Cmd) (TResult, error) {
	var zero TResult
	RequirePathID(cmd.PathID(), "PartialUpdateCommandHandler")
	current, err := h.Repo.FindByID(domain.NewID(cmd.PathID()))
	if err != nil {
		return zero, err
	}
	apply := func(entity T) { cmd.ApplyPartiallyTo(ctx, entity) }
	updatable, err := domain.GetPartialUpdatable(current, apply, h.Service, "GetPartialUpdatable")
	if err != nil {
		return zero, err
	}
	opts := collectWriteOptions[T, Cmd](cmd)
	if err := h.Repo.Scope(ctx, opts...).Update(updatable); err != nil {
		return zero, err
	}
	return cmd.FromEntity(ctx, current), nil
}
