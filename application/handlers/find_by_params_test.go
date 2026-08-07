package handlers

import (
	"errors"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/queries"
)

func TestFindByParamsQueryHandler_DelegatesToReader(t *testing.T) {
	reader := &spyReader{
		pageToReturn: queries.Page{
			Items:   []map[string]any{{"id": "a"}},
			HasNextPage: true,
			TotalCount:   1,
		},
	}
	h := &FindByParamsQueryHandler[*testFindParamsQuery]{Reader: reader, View: "users"}

	q := &testFindParamsQuery{
		ReadCriteria: queries.ReadCriteria{
			Filter: map[string]any{"name": "Jane"},
			Limit:  10,
		},
	}
	ctx := testCtx()
	page, err := h.Handle(ctx, q)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reader.readPageCalled != 1 {
		t.Errorf("expected ReadPage called once, got %d", reader.readPageCalled)
	}
	if reader.gotView != "users" {
		t.Errorf("expected view 'users', got %q", reader.gotView)
	}
	if got := reader.gotCriteria.Filter["name"]; got != "Jane" {
		t.Errorf("expected criteria filter 'name'=Jane, got %v", got)
	}
	if got := reader.gotCriteria.Limit; got != 10 {
		t.Errorf("expected criteria Limit=10, got %d", got)
	}
	if q.gotCtx != ctx {
		t.Error("expected ToCriteria to receive the request ctx")
	}
	if !page.HasNextPage || page.TotalCount != 1 {
		t.Errorf("expected page roundtrip, got %+v", page)
	}
}

func TestFindByParamsQueryHandler_PropagatesReaderError(t *testing.T) {
	want := errors.New("mongo down")
	reader := &spyReader{pageErr: want}
	h := &FindByParamsQueryHandler[*testFindParamsQuery]{Reader: reader, View: "users"}

	_, err := h.Handle(testCtx(), &testFindParamsQuery{})
	if !errors.Is(err, want) {
		t.Errorf("expected reader error to propagate, got %v", err)
	}
}
