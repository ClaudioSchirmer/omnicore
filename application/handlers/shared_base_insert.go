package handlers

import (
	"fmt"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/persistence"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/domain"
)

// SharedBaseInsertCommandHandler is the Auto handler for a POST of an entity
// backed by a SharedBase (Party-Role). It is an UPSERT: load-first like an update,
// the command applies the request on top, then insert. The shared identity is
// loaded by the natural key; the existing native children arrive as Constructor
// items so the command's domain logic (e.g. AddAddress) can dedup against them —
// the infra never dedups.
//
// Two-way marriage: this handler REQUIRES a SharedBase-backed repository (one that
// implements persistence.SharedBaseInsertLoader[T]); a repo without a SharedBase
// is refused with a directive error pointing at InsertCommandHandler. The reverse
// (a SharedBase entity wired on the plain InsertCommandHandler) is refused there,
// and a manual blind insert against an existing identity is refused by the
// persister (the actionName guard).
type SharedBaseInsertCommandHandler[T domain.Entity, Cmd pipeline.SharedBaseInsertCommand[T, TResult], TResult any] struct {
	Repo    persistence.ScopedRepository[T]
	Service domain.Service
}

func (h *SharedBaseInsertCommandHandler[T, Cmd, TResult]) Handle(ctx *configuration.AppContext, cmd Cmd) (TResult, error) {
	var zero TResult

	// Direction 2 of the marriage: this handler is only for SharedBase-backed repos.
	loader, ok := any(h.Repo).(persistence.SharedBaseInsertLoader[T])
	if !ok {
		return zero, fmt.Errorf(
			"SharedBaseInsertCommandHandler: %T has no SharedBase — use InsertCommandHandler for entities without a shared base", h.Repo)
	}

	// Throwaway apply onto a fresh entity to read the natural key. ApplyTo is a
	// pure mapper (no side effects), so this extra call is safe and invisible.
	fresh := h.Repo.New()
	if err := cmd.ApplyTo(ctx, fresh); err != nil {
		return zero, err
	}

	// Load the existing shared identity (base fields + base-children as Constructor)
	// by the natural key. existed=false → cold insert; fresh is reused.
	entity, existed, err := loader.LoadForSharedBaseInsert(ctx, fresh)
	if err != nil {
		return zero, err
	}

	action := "GetInsertable"
	if existed {
		// warm: apply the request onto the loaded identity — the command's domain
		// logic dedups its native children against the loaded Constructor items.
		if err := cmd.ApplyTo(ctx, entity); err != nil {
			return zero, err
		}
		action = "GetUpsertable" // same ModeInsert, distinct actionName (BuildRules can branch)
	}

	insertable, err := domain.GetInsertable(entity, h.Service, action)
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
