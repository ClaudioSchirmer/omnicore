package handlers

import (
	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/persistence"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/domain"
)

// DeleteCommandHandler runs persistence.LoadForWrite + cmd.ApplyTo(ctx, current) +
// GetDeletable + Repo.Delete(ctx, deletable, opts...) +
// cmd.FromEntity(ctx, current). The request-ctx-bound load (under
// http.requestTimeoutSeconds when the repo provides ScopedReader) guarantees
// audit consistency (snapshot of the entity before the hard delete) and gives
// FromEntity a populated in-memory entity to read from even though the row is
// gone post-commit. The cascade of children is owned by the framework in Go:
// the persister issues an explicit DELETE per declared child table (by ParentID)
// before the root DELETE, in the same TX — a database ON DELETE CASCADE on the
// FKs is an optional integrity safety-net, not the mechanism.
//
// cmd.ApplyTo runs AFTER the load and BEFORE GetDeletable so the Command can
// translate the request *AppContext into business-named transient fields.
// GetDeletable runs BuildRules in ModeDelete — service uses IfDelete for
// delete-specific rules (e.g., "cannot delete primary address") and can read
// any transient set by ApplyTo. cmd.FromEntity runs after the SQL delete
// returns, projecting the in-memory snapshot into TResult.
//
// In-TX side effects: same provider-interface detection as the other Auto
// handlers — Cmd implementing AfterBeginHookProvider[T] /
// BeforeCommitHookProvider[T] threads the matching closures as
// persistence.WriteOption[T] on the Repo.Delete call.
//
// TResult is the application-layer projection the wire layer will render
// after the delete completes.
type DeleteCommandHandler[T domain.Entity, Cmd pipeline.DeleteCommand[T, TResult], TResult any] struct {
	pipeline.PathIDRequired
	Repo    persistence.ScopedRepository[T]
	Service domain.Service
}

func (h *DeleteCommandHandler[T, Cmd, TResult]) Handle(ctx *configuration.AppContext, cmd Cmd) (TResult, error) {
	var zero TResult
	RequirePathID(cmd.PathID(), "DeleteCommandHandler")
	current, err := persistence.LoadForWrite(h.Repo, ctx, domain.NewID(cmd.PathID()))
	if err != nil {
		return zero, err
	}
	if err := cmd.ApplyTo(ctx, current); err != nil {
		return zero, err
	}
	deletable, err := domain.GetDeletable(current, h.Service, "GetDeletable")
	if err != nil {
		return zero, err
	}
	opts := collectWriteOptions[T, Cmd](cmd)
	if err := h.Repo.Scope(ctx, opts...).Delete(deletable); err != nil {
		return zero, err
	}
	result, err := cmd.FromEntity(ctx, current)
	if err != nil {
		return zero, err
	}
	return result, nil
}
