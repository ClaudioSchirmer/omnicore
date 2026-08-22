package query

import (
	"context"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/queries"
)

// fakeReader is a queries.ViewReader that reports which backing served the read,
// so the seam's dispatch can be asserted without a real store.
type fakeReader struct{ tag string }

func (f *fakeReader) ReadPage(_ context.Context, _ string, _ queries.ReadCriteria) (queries.Page, error) {
	return queries.Page{Items: []map[string]any{{"backing": f.tag}}}, nil
}

func (f *fakeReader) ReadByID(_ context.Context, _, _ string, _ queries.ReadCriteria) (map[string]any, bool, error) {
	return map[string]any{"backing": f.tag}, true, nil
}

// A nil fallback installs the unbackedReader: a view that reaches it gets an
// actionable error, never a nil panic. A view WITH a registered backing is
// unaffected — the missing fallback is not a missing seam.
func TestViewReaderEngine_NilFallbackUsesUnbackedReader(t *testing.T) {
	e := NewViewReaderEngine(nil)

	if _, err := e.ReadPage(context.Background(), "unbacked", queries.ReadCriteria{}); err == nil {
		t.Error("ReadPage on a view with no backing must error, not panic")
	}
	if _, _, err := e.ReadByID(context.Background(), "unbacked", "id", queries.ReadCriteria{}); err == nil {
		t.Error("ReadByID on a view with no backing must error, not panic")
	}

	e.Register(&fakeReader{tag: "other"}, map[string]bool{"fresh": true})
	page, err := e.ReadPage(context.Background(), "fresh", queries.ReadCriteria{})
	if err != nil {
		t.Fatalf("a registered backing must serve even with no fallback: %v", err)
	}
	if page.Items[0]["backing"] != "other" {
		t.Errorf("registered view must route to its own backing, got %v", page.Items[0])
	}
}

func TestViewReaderEngine_DispatchesByRegisteredBacking(t *testing.T) {
	e := NewViewReaderEngine(&fakeReader{tag: "fallback"})
	e.Register(&fakeReader{tag: "other"}, map[string]bool{"fresh": true})

	page, _ := e.ReadPage(context.Background(), "fresh", queries.ReadCriteria{})
	if page.Items[0]["backing"] != "other" {
		t.Errorf("a registered view must route to its backing, got %v", page.Items[0])
	}
	unregistered, _ := e.ReadPage(context.Background(), "other_view", queries.ReadCriteria{})
	if unregistered.Items[0]["backing"] != "fallback" {
		t.Errorf("an unregistered view must route to the fallback, got %v", unregistered.Items[0])
	}
	byID, _, _ := e.ReadByID(context.Background(), "fresh", "x", queries.ReadCriteria{})
	if byID["backing"] != "other" {
		t.Errorf("ReadByID must dispatch by backing too, got %v", byID)
	}
}

func TestViewReaderEngine_NoRegistrationAllFallback(t *testing.T) {
	e := NewViewReaderEngine(&fakeReader{tag: "fallback"})
	page, _ := e.ReadPage(context.Background(), "anything", queries.ReadCriteria{})
	if page.Items[0]["backing"] != "fallback" {
		t.Errorf("with nothing registered every view routes to the fallback, got %v", page.Items[0])
	}
}

// Register is a no-op on a nil reader or an empty name set — a backing that
// serves nothing must not shadow the fallback.
func TestViewReaderEngine_RegisterIgnoresEmptyInput(t *testing.T) {
	e := NewViewReaderEngine(&fakeReader{tag: "fallback"})
	e.Register(nil, map[string]bool{"fresh": true})
	e.Register(&fakeReader{tag: "other"}, nil)

	page, _ := e.ReadPage(context.Background(), "fresh", queries.ReadCriteria{})
	if page.Items[0]["backing"] != "fallback" {
		t.Errorf("an empty registration must leave the fallback in place, got %v", page.Items[0])
	}
}

func TestViewReaderEngine_FallbackExposed(t *testing.T) {
	m := &fakeReader{tag: "fallback"}
	e := NewViewReaderEngine(m)
	if e.Fallback() != queries.ViewReader(m) {
		t.Error("Fallback must return the wrapped reader for the run-phase mutations")
	}
}
