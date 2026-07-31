package query

import (
	"context"

	"github.com/ClaudioSchirmer/omnicore/application/queries"
)

// ViewReaderEngine is the read-side dispatch seam: it implements the neutral
// queries.ViewReader port and routes each read to the store the view is backed
// by — the Mongo projection reader by default, the relational reader for a view
// marked RelationalSource. It mirrors core.RelationalEngine on the write side:
// the upper layers depend only on the port; the backing is selected behind it,
// here per view rather than per process.
//
// It is built EARLY (wrapping the Mongo reader) and installed as deps.ViewReader,
// so every handler captures the one seam; the relational reader and the per-view
// route are filled in later by mutation (SetRelational) — never a pointer swap —
// so a handler that captured deps.ViewReader before wiring finished still
// dispatches correctly. A pure-Mongo service keeps a seam whose relational side
// is never installed, at no cost.
type ViewReaderEngine struct {
	mongo        queries.ViewReader
	relational   queries.ViewReader
	isRelational map[string]bool
}

// NewViewReaderEngine wraps the Mongo reader — the always-present default
// backing. The relational side is installed later via SetRelational.
func NewViewReaderEngine(mongo queries.ViewReader) *ViewReaderEngine {
	return &ViewReaderEngine{mongo: mongo}
}

// MongoReader returns the wrapped Mongo reader so the bootstrap's run-phase
// mutations (SetViews / SetComposedViews / the max-limit resolver) reach it
// through the seam without reassigning deps.ViewReader.
func (e *ViewReaderEngine) MongoReader() queries.ViewReader { return e.mongo }

// SetRelational installs the relational reader and the set of view names it
// serves (the per-view route). Called once during run-phase wiring, by mutation.
func (e *ViewReaderEngine) SetRelational(reader queries.ViewReader, relationalViews map[string]bool) {
	e.relational = reader
	e.isRelational = relationalViews
}

// backing picks the reader for a view: the relational reader when the view is
// marked and the relational side is installed, else the Mongo reader.
func (e *ViewReaderEngine) backing(view string) queries.ViewReader {
	if e.relational != nil && e.isRelational[view] {
		return e.relational
	}
	return e.mongo
}

func (e *ViewReaderEngine) ReadPage(ctx context.Context, view string, crit queries.ReadCriteria) (queries.Page, error) {
	return e.backing(view).ReadPage(ctx, view, crit)
}

func (e *ViewReaderEngine) ReadByID(ctx context.Context, view, id string, crit queries.ReadCriteria) (map[string]any, bool, error) {
	return e.backing(view).ReadByID(ctx, view, id, crit)
}

var _ queries.ViewReader = (*ViewReaderEngine)(nil)
