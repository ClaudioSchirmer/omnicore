package query

import (
	"context"
	"fmt"

	"github.com/ClaudioSchirmer/omnicore/application/queries"
)

// ViewReaderEngine is the read-side dispatch seam: it implements the neutral
// queries.ViewReader port and routes each read to the store the view is backed
// by. It mirrors core.RelationalEngine on the write side — the upper layers
// depend only on the port; the backing is selected behind it, here per view
// rather than per process.
//
// The seam knows NOTHING about any particular store. It holds a fallback reader
// (the backing a view uses unless it says otherwise) plus an optional per-view
// override, both typed as the neutral port. Which store a reader speaks to, and
// which capabilities it can serve, are that reader's own business: a backing
// refuses what it cannot do at its own entry point, before any IO. There is no
// capability table here and no branch naming a store.
//
// It is built EARLY (wrapping the fallback) and installed as deps.ViewReader, so
// every handler captures the one seam; per-view backings are filled in later by
// mutation (Register) — never a pointer swap — so a handler that captured
// deps.ViewReader before wiring finished still dispatches correctly. A service
// with a single backing keeps a seam whose override map is never populated, at
// no cost.
type ViewReaderEngine struct {
	fallback queries.ViewReader
	byView   map[string]queries.ViewReader
}

// NewViewReaderEngine wraps the fallback reader — the backing every view uses
// unless Register overrides it per view. A nil fallback is tolerated: it means
// the service booted with no default read store, and is replaced by
// unbackedReader, which returns an actionable error if it ever IS dispatched to
// — the honest safety net, never a nil panic.
func NewViewReaderEngine(fallback queries.ViewReader) *ViewReaderEngine {
	if fallback == nil {
		fallback = unbackedReader{}
	}
	return &ViewReaderEngine{fallback: fallback}
}

// unbackedReader stands in for an absent fallback reader. A boot guard rejects
// declaring views with no store to serve them, so this is unreachable in a
// booted service — it exists to satisfy the type and, as a safety net, to fail
// with a clear message rather than panic if a read somehow reaches it.
type unbackedReader struct{}

func (unbackedReader) ReadPage(_ context.Context, view string, _ queries.ReadCriteria) (queries.Page, error) {
	return queries.Page{}, fmt.Errorf("view %q has no read backing installed", view)
}

func (unbackedReader) ReadByID(_ context.Context, view, _ string, _ queries.ReadCriteria) (map[string]any, bool, error) {
	return nil, false, fmt.Errorf("view %q has no read backing installed", view)
}

// Fallback returns the wrapped fallback reader so the bootstrap's run-phase
// mutations reach it through the seam without reassigning deps.ViewReader. The
// caller type-asserts to the concrete reader it wired.
func (e *ViewReaderEngine) Fallback() queries.ViewReader { return e.fallback }

// Register installs a per-view backing: every named view dispatches to reader
// instead of the fallback. Called during run-phase wiring, by mutation, once per
// backing. A name already registered is overwritten — last registration wins, so
// a duplicate view name is a wiring error the boot guards catch, never a silent
// merge here.
func (e *ViewReaderEngine) Register(reader queries.ViewReader, views map[string]bool) {
	if reader == nil || len(views) == 0 {
		return
	}
	if e.byView == nil {
		e.byView = make(map[string]queries.ViewReader, len(views))
	}
	for name := range views {
		e.byView[name] = reader
	}
}

// backing picks the reader for a view: its registered override when it has one,
// else the fallback.
func (e *ViewReaderEngine) backing(view string) queries.ViewReader {
	if r, ok := e.byView[view]; ok {
		return r
	}
	return e.fallback
}

func (e *ViewReaderEngine) ReadPage(ctx context.Context, view string, crit queries.ReadCriteria) (queries.Page, error) {
	return e.backing(view).ReadPage(ctx, view, crit)
}

func (e *ViewReaderEngine) ReadByID(ctx context.Context, view, id string, crit queries.ReadCriteria) (map[string]any, bool, error) {
	return e.backing(view).ReadByID(ctx, view, id, crit)
}

var _ queries.ViewReader = (*ViewReaderEngine)(nil)
