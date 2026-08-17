package handlers

import (
	"errors"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/queries"
)

func TestFindByParamsQueryHandler_DelegatesToReader(t *testing.T) {
	reader := &spyReader{
		pageToReturn: queries.Page{
			Items:       []map[string]any{{"ID": "a", "Name": "Jane"}},
			HasNextPage: true,
			TotalCount:  1,
		},
	}
	h := &FindByParamsQueryHandler[*testFindParamsQuery, testFindResult]{Reader: reader, View: "users"}

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
		t.Errorf("expected page envelope roundtrip, got %+v", page)
	}
	if len(page.Items) != 1 || page.Items[0].ID != "a" || page.Items[0].Name != "Jane" {
		t.Errorf("expected the doc filled into the typed Result, got %+v", page.Items)
	}
}

func TestFindByParamsQueryHandler_PropagatesReaderError(t *testing.T) {
	want := errors.New("mongo down")
	reader := &spyReader{pageErr: want}
	h := &FindByParamsQueryHandler[*testFindParamsQuery, testFindResult]{Reader: reader, View: "users"}

	_, err := h.Handle(testCtx(), &testFindParamsQuery{})
	if !errors.Is(err, want) {
		t.Errorf("expected reader error to propagate, got %v", err)
	}
}

// FromQueryResult is the mandatory read-side hook: the handler must invoke it once
// per returned document, handing in the TResult already filled from the doc,
// and surface whatever the hook returns (derived-field shaping shows on the
// typed page).
func TestFindByParamsQueryHandler_FromQueryInvokedPerItem(t *testing.T) {
	reader := &spyReader{
		pageToReturn: queries.Page{
			Items: []map[string]any{
				{"ID": "a", "Name": "jane"},
				{"ID": "b", "Name": "john"},
			},
			TotalCount: 2,
		},
	}
	h := &FindByParamsQueryHandler[*testFindParamsQuery, testFindResult]{Reader: reader, View: "users"}

	q := &testFindParamsQuery{
		mutate: func(r testFindResult) testFindResult {
			r.Name = strings.ToUpper(r.Name)
			return r
		},
	}
	page, err := h.Handle(testCtx(), q)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if q.fromQueryCalls != 2 {
		t.Errorf("expected FromQueryResult called once per item, got %d", q.fromQueryCalls)
	}
	if len(q.fromQuerySeen) != 2 || q.fromQuerySeen[0].ID != "a" || q.fromQuerySeen[1].ID != "b" {
		t.Errorf("expected FromQueryResult to receive the filled Results in order, got %+v", q.fromQuerySeen)
	}
	if len(page.Items) != 2 || page.Items[0].Name != "JANE" || page.Items[1].Name != "JOHN" {
		t.Errorf("expected FromQueryResult's return values on the page, got %+v", page.Items)
	}
}

func TestFindByParamsQueryHandler_FromQueryErrorPropagates(t *testing.T) {
	want := errors.New("from-query boom")
	reader := &spyReader{
		pageToReturn: queries.Page{Items: []map[string]any{{"ID": "a"}}, TotalCount: 1},
	}
	h := &FindByParamsQueryHandler[*testFindParamsQuery, testFindResult]{Reader: reader, View: "users"}

	_, err := h.Handle(testCtx(), &testFindParamsQuery{fromQueryErr: want})
	if !errors.Is(err, want) {
		t.Errorf("expected FromQueryResult error to propagate, got %v", err)
	}
}
