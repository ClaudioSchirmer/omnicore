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

// TestViewReaderEngine_NilMongoUsesAbsentReader proves the infra-free posture
// (Item 11): NewViewReaderEngine(nil) installs an absentMongoReader that returns
// an actionable error — never a nil panic — when a Mongo-backed view is
// dispatched to it, while relational views still route correctly.
func TestViewReaderEngine_NilMongoUsesAbsentReader(t *testing.T) {
	e := NewViewReaderEngine(nil)

	// A view with no relational backing dispatches to the absent Mongo reader.
	if _, err := e.ReadPage(context.Background(), "needs_mongo", queries.ReadCriteria{}); err == nil {
		t.Error("ReadPage on a Mongo-backed view with no Mongo must error, not panic")
	}
	if _, _, err := e.ReadByID(context.Background(), "needs_mongo", "id", queries.ReadCriteria{}); err == nil {
		t.Error("ReadByID on a Mongo-backed view with no Mongo must error, not panic")
	}

	// A relational view still routes to the relational reader — infra-free is the
	// relational-only posture, and those reads succeed.
	e.SetRelational(&fakeReader{tag: "relational"}, map[string]bool{"fresh": true})
	page, err := e.ReadPage(context.Background(), "fresh", queries.ReadCriteria{})
	if err != nil {
		t.Fatalf("relational read under infra-free posture failed: %v", err)
	}
	if page.Items[0]["backing"] != "relational" {
		t.Errorf("relational view must route to the relational reader, got %v", page.Items[0])
	}
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
