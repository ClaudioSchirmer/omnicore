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

func TestViewReaderEngine_DispatchesByBacking(t *testing.T) {
	e := NewViewReaderEngine(&fakeReader{tag: "mongo"})
	e.SetRelational(&fakeReader{tag: "relational"}, map[string]bool{"fresh": true})

	page, _ := e.ReadPage(context.Background(), "fresh", queries.ReadCriteria{})
	if page.Items[0]["backing"] != "relational" {
		t.Errorf("a relational-backed view must route to the relational reader, got %v", page.Items[0])
	}
	other, _ := e.ReadPage(context.Background(), "other", queries.ReadCriteria{})
	if other.Items[0]["backing"] != "mongo" {
		t.Errorf("a Mongo-backed view must route to the Mongo reader, got %v", other.Items[0])
	}
	byID, _, _ := e.ReadByID(context.Background(), "fresh", "x", queries.ReadCriteria{})
	if byID["backing"] != "relational" {
		t.Errorf("ReadByID must dispatch by backing too, got %v", byID)
	}
}

func TestViewReaderEngine_NoRelationalInstalledAllMongo(t *testing.T) {
	e := NewViewReaderEngine(&fakeReader{tag: "mongo"})
	page, _ := e.ReadPage(context.Background(), "anything", queries.ReadCriteria{})
	if page.Items[0]["backing"] != "mongo" {
		t.Errorf("with no relational side installed every view routes to Mongo, got %v", page.Items[0])
	}
}

func TestViewReaderEngine_MongoReaderExposed(t *testing.T) {
	m := &fakeReader{tag: "mongo"}
	e := NewViewReaderEngine(m)
	if e.MongoReader() != queries.ViewReader(m) {
		t.Error("MongoReader must return the wrapped reader for the run-phase mutations")
	}
}
