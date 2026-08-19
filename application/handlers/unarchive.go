package handlers

import (
	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/persistence"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/domain"
)

// UnarchiveCommandHandler is asymmetric to the other Auto handlers in the
// load path (not in the wire-up): instead of calling FindByID (which filters
// WHERE deleted_at IS NULL), it hydrates the archived aggregate via
// persistence.LoadArchivedForWrite — the request-ctx-bound ScopedArchivedReader
// when the repo provides it (so the load honors http.requestTimeoutSeconds),
// else the ctx-less domain.ArchivedFinder. The archived hydration is needed so
// the cascade SQL sees children typeNames via root.AllAggregateItems(). When
// the Repository provides neither, the handler falls back to Repo.New() + SetID
// for flat aggregates without children.
//
// cmd.ApplyTo runs AFTER the entity is hydrated and BEFORE GetUnarchivable
// so the Command can translate the request *AppContext into business-named
// transient fields. GetUnarchivable then runs BuildRules in ModeUnarchive with
// actionName = "GetUnarchivable" — IfUnarchive fires and the service can
// validate. The Unarchive state-transition checks (Modes() / ID validity)
// still run after the BuildRules pass.
//
// The hydration snapshots the entity (domain.CaptureOld), so domain.Old[T]
// inside IfUnarchive answers the PERSISTED archived state — never the state
// ApplyTo produced. The empty-sample fallback below snapshots too, yielding
// the degenerate ID-only ghost that path has always produced.
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
	if loaded, found, err := persistence.LoadArchivedForWrite(h.Repo, ctx, id); found {
		if err != nil {
			return zero, err
		}
		sample = loaded
	} else {
		sample = h.Repo.New()
		sample.SetID(id)
		domain.CaptureOld(sample)
	}

	if err := cmd.ApplyTo(ctx, sample); err != nil {
		return zero, err
	}
	unarchivable, err := domain.GetUnarchivable(sample, persistence.ScopeService(h.Service, ctx), "GetUnarchivable")
	if err != nil {
		return zero, err
	}
	opts := collectWriteOptions[T, Cmd](cmd)
	if err := h.Repo.Scope(ctx, opts...).Unarchive(unarchivable); err != nil {
		return zero, err
	}
	result, err := cmd.FromEntity(ctx, sample)
	if err != nil {
		return zero, err
	}
	return result, nil
}
