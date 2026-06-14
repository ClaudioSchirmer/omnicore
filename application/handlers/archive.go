package handlers

import (
	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/persistence"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/domain"
)

// ArchiveCommandHandler runs Repo.FindByID + cmd.ApplyTo(ctx, current) +
// GetArchivable + Repo.Archive(ctx, archivable, opts...) +
// cmd.FromEntity(ctx, current). The internal FindByID loads the full
// aggregate (root + children), ensuring GetArchivable cascades children
// correctly per the root's AggregateMapping.
//
// cmd.ApplyTo lands AFTER FindByID and BEFORE GetArchivable so the Command
// can translate the request *AppContext into business-named transient fields
// on the loaded entity (e.g., u.SetRequestingOwnerID(ctx.Identity().Subject)).
// GetArchivable then runs BuildRules in ModeUpdate with actionName =
// "GetArchivable" — IfUpdate fires and the service can validate the transient
// against the persistent owner field. Archive's own state-transition checks
// (Modes() / ID validity) still run after the BuildRules pass.
//
// cmd.FromEntity runs after the archive completes, with the same ctx — typical
// bodyless verb shape is TResult = results.None and FromEntity returns
// results.None{}.
//
// In-TX side effects: same provider-interface detection as the other Auto
// handlers — Cmd implementing AfterBeginHookProvider[T] /
// BeforeCommitHookProvider[T] threads the matching closures as
// persistence.WriteOption[T] on the Repo.Archive call.
//
// TResult is the application-layer projection the wire layer will render
// after the archive completes.
type ArchiveCommandHandler[T domain.Entity, Cmd pipeline.ArchiveCommand[T, TResult], TResult any] struct {
	pipeline.PathIDRequired
	Repo    persistence.Writer[T]
	Service domain.Service
}

func (h *ArchiveCommandHandler[T, Cmd, TResult]) Handle(ctx *configuration.AppContext, cmd Cmd) (TResult, error) {
	var zero TResult
	RequirePathID(cmd.PathID(), "ArchiveCommandHandler")
	current, err := h.Repo.FindByID(domain.NewID(cmd.PathID()))
	if err != nil {
		return zero, err
	}
	cmd.ApplyTo(ctx, current)
	archivable, err := domain.GetArchivable(current, h.Service, "GetArchivable")
	if err != nil {
		return zero, err
	}
	opts := collectWriteOptions[T, Cmd](cmd)
	if err := h.Repo.Archive(ctx, archivable, opts...); err != nil {
		return zero, err
	}
	return cmd.FromEntity(ctx, current), nil
}
