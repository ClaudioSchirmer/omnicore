package graphql

import (
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/queries"
)

// The count-only path: a connection selection of just `totalCount` (no edges,
// no pageInfo) maps to ReadCriteria.OnlyTotal — the GraphQL idiom for REST's
// ?onlyTotal=true. These tests assert the flag the resolver hands the reader
// (captured via fakeReadHandler from execute_test.go), since the reader's
// short-circuit to CountDocuments is keyed on it.

func TestCountOnly_TotalCountAloneSetsOnlyTotal(t *testing.T) {
	h := &fakeReadHandler{page: queries.Page{OnlyTotal: true, Total: 5}}
	reg, ctx := newExecRegistry(h)

	resp := reg.Execute(ctx, `{ users { totalCount } }`, nil, "")
	if len(resp.Errors) != 0 {
		t.Fatalf("unexpected errors: %+v", resp.Errors)
	}
	if !h.captured.OnlyTotal {
		t.Error("selecting only totalCount must set ReadCriteria.OnlyTotal")
	}
	if got := resp.Data["users"].(map[string]any)["totalCount"]; got != int64(5) {
		t.Errorf("totalCount = %v, want 5", got)
	}
}

func TestCountOnly_EdgesSelectionKeepsFullRead(t *testing.T) {
	h := &fakeReadHandler{page: queries.Page{
		Items:       []map[string]any{{"ID": "u1", "Name": "alice"}},
		ItemCursors: []string{"c1"},
		Total:       1,
	}}
	reg, ctx := newExecRegistry(h)

	resp := reg.Execute(ctx, `{ users { edges { node { id } } totalCount } }`, nil, "")
	if len(resp.Errors) != 0 {
		t.Fatalf("unexpected errors: %+v", resp.Errors)
	}
	if h.captured.OnlyTotal {
		t.Error("selecting edges must NOT set OnlyTotal (items are materialized)")
	}
}

func TestCountOnly_PageInfoSelectionKeepsFullRead(t *testing.T) {
	h := &fakeReadHandler{page: queries.Page{Total: 1}}
	reg, ctx := newExecRegistry(h)

	// pageInfo cursors derive from the page items, so totalCount + pageInfo
	// cannot short-circuit.
	resp := reg.Execute(ctx, `{ users { pageInfo { hasNextPage } totalCount } }`, nil, "")
	if len(resp.Errors) != 0 {
		t.Fatalf("unexpected errors: %+v", resp.Errors)
	}
	if h.captured.OnlyTotal {
		t.Error("selecting pageInfo must NOT set OnlyTotal")
	}
}

func TestCountOnly_CountsFilteredSubset(t *testing.T) {
	h := &fakeReadHandler{page: queries.Page{OnlyTotal: true, Total: 3}}
	reg, ctx := newExecRegistry(h)

	resp := reg.Execute(ctx,
		`{ users(where: { name: { eq: "alice" } }, search: "x", includeArchived: true) { totalCount } }`,
		nil, "")
	if len(resp.Errors) != 0 {
		t.Fatalf("unexpected errors: %+v", resp.Errors)
	}
	// Count-only still bounds the count by filter / search / archived gate.
	if !h.captured.OnlyTotal {
		t.Error("count-only must remain set alongside where/search/includeArchived")
	}
	if h.captured.Filter["Name"] != "alice" {
		t.Errorf("where eq did not fold; Filter = %v", h.captured.Filter)
	}
	if h.captured.Search != "x" {
		t.Errorf("Search = %q, want x", h.captured.Search)
	}
	if !h.captured.IncludeArchived {
		t.Error("IncludeArchived must be honored under count-only")
	}
}

// TestCountOnly_PaginationArgConflictRejected — a pagination/sort argument
// (first/last/after/before/orderBy) alongside a totalCount-only selection is a
// conflict: there is no page to order or seek into. The resolver rejects it
// with a SchemaViolation (semantic Schema), REST parity with onlyTotalConflicts,
// and the handler never runs.
func TestCountOnly_PaginationArgConflictRejected(t *testing.T) {
	for _, q := range []string{
		`{ users(first: 10) { totalCount } }`,
		`{ users(orderBy: ["-name"]) { totalCount } }`,
		`{ users(after: "abc") { totalCount } }`,
	} {
		h := &fakeReadHandler{page: queries.Page{}}
		reg, ctx := newExecRegistry(h)

		resp := reg.Execute(ctx, q, nil, "")
		if len(resp.Errors) == 0 {
			t.Fatalf("%s: count-only + pagination arg must be rejected", q)
		}
		if got := resp.Errors[0].Extensions["semantic"]; got != "Schema" {
			t.Errorf("%s: semantic = %v, want Schema", q, got)
		}
		if got := resp.Errors[0].Extensions["notificationKey"]; got != "SchemaViolationNotification" {
			t.Errorf("%s: notificationKey = %v, want SchemaViolationNotification", q, got)
		}
		if h.captured.OnlyTotal {
			t.Errorf("%s: handler must not run on a rejected conflict", q)
		}
	}
}
