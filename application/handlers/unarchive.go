package handlers

import (
	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/persistence"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/domain"
)

// UnarchiveCommandHandler is asymmetric to the other Auto handlers in the
// load path (not in the wire-up): instead of calling FindByID (which filters
// WHERE deleted_at IS NULL), it looks for an ArchivedFinder implementation
// on the Repository to hydrate the archived aggregate (needed so the cascade
// SQL sees children typeNames via root.AllAggregateItems()). When the
// Repository does not implement ArchivedFinder, the handler falls back to
// Repo.New() + SetID for flat aggregates without children.
//
// cmd.ApplyTo runs AFTER the entity is hydrated and BEFORE GetUnarchivable
// so the Command can translate the request *AppContext into business-named
// transient fields. GetUnarchivable then runs BuildRules in ModeUpdate with
// actionName = "GetUnarchivable" — IfUpdate fires and the service can
// validate. The Unarchive state-transition checks (Modes() / ID validity)
// still run after the BuildRules pass.
//
// cmd.FromEntity runs after the unarchive completes — same ctx + cmd
// available for the projection.
//
// In-TX side effects: same provider-interface detection as the other Auto
// handlers — Cmd implementing AfterBeginHookProvider[T] /
// BeforeCommitHookProvider[T] threads the matching closures as
// persistence.WriteOption[T] on the Repo.Unarchive call.
//
// TResult is the application-layer projection the wire layer will render
// after the unarchive completes.
type UnarchiveCommandHandler[T domain.Entity, Cmd pipeline.UnarchiveCommand[T, TResult], TResult any] struct {
	pipeline.PathIDRequired
	Repo    persistence.ScopedRepository[T]
	Service domain.Service
}

func (h *UnarchiveCommandHandler[T, Cmd, TResult]) Handle(ctx *configuration.AppContext, cmd Cmd) (TResult, error) {
	var zero TResult
	RequirePathID(cmd.PathID(), "UnarchiveCommandHandler")
	id := domain.NewID(cmd.PathID())

	var sample T
	if finder, ok := any(h.Repo).(domain.ArchivedFinder[T]); ok {
		loaded, err := finder.FindArchivedByID(id)
		if err != nil {
			return zero, err
		}
		sample = loaded
	} else {
		sample = h.Repo.New()
		sample.SetID(id)
	}

	cmd.ApplyTo(ctx, sample)
	unarchivable, err := domain.GetUnarchivable(sample, h.Service, "GetUnarchivable")
	if err != nil {
		return zero, err
	}
	opts := collectWriteOptions[T, Cmd](cmd)
	if err := h.Repo.Scope(ctx, opts...).Unarchive(unarchivable); err != nil {
		return zero, err
	}
	return cmd.FromEntity(ctx, sample), nil
}
