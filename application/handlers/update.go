package handlers

import (
	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/persistence"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/domain"
)

// UpdateCommandHandler runs Repo.FindByID + domain.GetUpdatable(current, ApplyTo, …)
// + Repo.Update(ctx, updatable, opts...) + cmd.FromEntity(ctx, current). PUT-shaped
// (full replace) — embeds pipeline.FullBody, which makes the Fiber wrapper require
// ALL exported fields of Cmd present in the body JSON before dispatch.
//
// The handler binds the request *AppContext into a closure wrapping
// cmd.ApplyTo, so the apply callback received by GetUpdatable still has the
// `func(T)` shape the domain expects. The Command can translate ctx into
// business-named transient fields on the entity (e.g.,
// u.SetRequestingOwnerID(ctx.Identity().Subject)) for BuildRules' IfUpdate
// to validate against the persistent owner field. cmd.FromEntity runs after
// the Repo persists, with full access to the same ctx + the cmd itself.
//
// In-TX side effects: when Cmd implements AfterBeginHookProvider[T] or
// BeforeCommitHookProvider[T], the matching closures are threaded as
// persistence.WriteOption[T] on the Repo.Update call — same persister slot
// PartialUpdateCommandHandler fires (single SQL fingerprint shared between
// PUT and PATCH).
//
// TResult is the application-layer projection the wire layer will render
// after the update completes.
type UpdateCommandHandler[T domain.Entity, Cmd pipeline.UpdateCommand[T, TResult], TResult any] struct {
	pipeline.FullBody
	pipeline.PathIDRequired
	Repo    persistence.Writer[T]
	Service domain.Service
}

func (h *UpdateCommandHandler[T, Cmd, TResult]) Handle(ctx *configuration.AppContext, cmd Cmd) (TResult, error) {
	var zero TResult
	RequirePathID(cmd.PathID(), "UpdateCommandHandler")
	current, err := h.Repo.FindByID(domain.NewID(cmd.PathID()))
	if err != nil {
		return zero, err
	}
	apply := func(entity T) { cmd.ApplyTo(ctx, entity) }
	updatable, err := domain.GetUpdatable(current, apply, h.Service, "GetUpdatable")
	if err != nil {
		return zero, err
	}
	opts := collectWriteOptions[T, Cmd](cmd)
	if err := h.Repo.Update(ctx, updatable, opts...); err != nil {
		return zero, err
	}
	return cmd.FromEntity(ctx, current), nil
}
