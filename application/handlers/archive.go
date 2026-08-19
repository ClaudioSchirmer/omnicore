package handlers

import (
	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/persistence"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/domain"
)

// ArchiveCommandHandler runs persistence.LoadForWrite + cmd.ApplyTo(ctx, current) +
// GetArchivable + Repo.Archive(ctx, archivable, opts...) +
// cmd.FromEntity(ctx, current). The request-ctx-bound load (under
// http.requestTimeoutSeconds when the repo provides ScopedReader) loads the full
// aggregate (root + children), ensuring GetArchivable cascades children
// correctly per the root's AggregateMapping.
//
// cmd.ApplyTo lands AFTER the load and BEFORE GetArchivable so the Command
// can translate the request *AppContext into business-named transient fields
// on the loaded entity (e.g., u.SetRequestingOwnerID(ctx.Identity().Subject)).
// GetArchivable then runs BuildRules in ModeArchive with actionName =
// "GetArchivable" — IfArchive fires and the service can validate the transient
// against the persistent owner field. Archive's own state-transition checks
// (Modes() / ID validity) still run after the BuildRules pass.
//
// The load snapshots the entity (domain.CaptureOld), so domain.Old[T] inside
// IfArchive answers the PERSISTED state — never the state ApplyTo or an
// earlier rule produced. Same guarantee, same mechanism, on all five
// state-changing verbs.
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
	Repo    persistence.ScopedRepository[T]
	Service domain.Service
}

func (h *ArchiveCommandHandler[T, Cmd, TResult]) Handle(ctx *configuration.AppContext, cmd Cmd) (TResult, error) {
	var zero TResult
	RequirePathID(cmd.PathID(), "ArchiveCommandHandler")
	current, err := persistence.LoadForWrite(h.Repo, ctx, domain.NewID(cmd.PathID()))
	if err != nil {
		return zero, err
	}
	if err := cmd.ApplyTo(ctx, current); err != nil {
		return zero, err
	}
	archivable, err := domain.GetArchivable(current, persistence.ScopeService(h.Service, ctx), "GetArchivable")
	if err != nil {
		return zero, err
	}
	opts := collectWriteOptions[T, Cmd](cmd)
	if err := h.Repo.Scope(ctx, opts...).Archive(archivable); err != nil {
		return zero, err
	}
	result, err := cmd.FromEntity(ctx, current)
	if err != nil {
		return zero, err
	}
	return result, nil
}
