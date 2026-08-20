package graphql

import (
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/queries"
)

// TestPagination_FirstIsForward — `first: N` sets the page size and leaves the
// criteria forward (Backward stays false; the reader pages from the start).
func TestPagination_FirstIsForward(t *testing.T) {
	h := &fakeReadHandler{}
	reg, ctx := newExecRegistry(h)

	resp := reg.Execute(ctx, `{ users(first: 5) { edges { node { id } } } }`, nil, "")
	if len(resp.Errors) != 0 {
		t.Fatalf("unexpected errors: %+v", resp.Errors)
	}
	if h.captured.Limit != 5 {
		t.Errorf("Limit = %d, want 5", h.captured.Limit)
	}
	if h.captured.Backward {
		t.Error("first must page forward (Backward=false)")
	}
}

// TestPagination_LastIsBackward — `last: N` is the only argument that carries
// direction on its own: it sets the page size AND Backward, so the reader walks
// back from the end and returns the LAST N (Relay semantics), even with no cursor.
func TestPagination_LastIsBackward(t *testing.T) {
	h := &fakeReadHandler{}
	reg, ctx := newExecRegistry(h)

	resp := reg.Execute(ctx, `{ users(last: 5) { edges { node { id } } } }`, nil, "")
	if len(resp.Errors) != 0 {
		t.Fatalf("unexpected errors: %+v", resp.Errors)
	}
	if h.captured.Limit != 5 {
		t.Errorf("Limit = %d, want 5", h.captured.Limit)
	}
	if !h.captured.Backward {
		t.Error("last must page backward (Backward=true)")
	}
}

// TestPagination_BeforeStaysCursorDriven — `before:` reaches the reader as a
// cursor; the cursor itself implies backward there, so the read path does NOT
// set Backward (only `last` does). This keeps REST — which has no `last` and
// infers direction purely from the cursor — behaving identically.
//
// The cursor is a REAL one: every surface runs the same structure check (it
// must decode, and its key tuple must be one longer than the ordering), so a
// placeholder string is a 400 here exactly as it is on REST.
func TestPagination_BeforeStaysCursorDriven(t *testing.T) {
	h := &fakeReadHandler{}
	reg, ctx := newExecRegistry(h)

	cur, err := queries.EncodeCursor([]any{"id"}, "")
	if err != nil {
		t.Fatalf("EncodeCursor: %v", err)
	}
	resp := reg.Execute(ctx, `{ users(before: "`+cur+`") { edges { node { id } } } }`, nil, "")
	if len(resp.Errors) != 0 {
		t.Fatalf("unexpected errors: %+v", resp.Errors)
	}
	if h.captured.Before != cur {
		t.Errorf("Before = %q, want the minted cursor", h.captured.Before)
	}
	if h.captured.Backward {
		t.Error("before alone must not set the explicit Backward flag (reader infers from the cursor)")
	}
}

// TestPagination_DirectionMixRejected — forward (first/after) and backward
// (last/before) arguments are mutually exclusive; every mix is rejected with a
// SchemaViolation before dispatch and the handler never runs. The after+before
// case is included: it is a 400 here, not the reader's defense-in-depth 500.
func TestPagination_DirectionMixRejected(t *testing.T) {
	for _, q := range []string{
		`{ users(first: 5, last: 5) { edges { node { id } } } }`,
		`{ users(first: 5, before: "c") { edges { node { id } } } }`,
		`{ users(last: 5, after: "c") { edges { node { id } } } }`,
		`{ users(after: "a", before: "b") { edges { node { id } } } }`,
	} {
		h := &fakeReadHandler{page: queries.PageOf[execResult]{Items: []execResult{{ID: sp("u1")}}}}
		reg, ctx := newExecRegistry(h)

		resp := reg.Execute(ctx, q, nil, "")
		if len(resp.Errors) == 0 {
			t.Fatalf("%s: a forward+backward mix must be rejected", q)
		}
		if got := resp.Errors[0].Extensions["semantic"]; got != "Schema" {
			t.Errorf("%s: semantic = %v, want Schema", q, got)
		}
		if got := resp.Errors[0].Extensions["notificationKey"]; got != "SchemaViolationNotification" {
			t.Errorf("%s: notificationKey = %v, want SchemaViolationNotification", q, got)
		}
		if resp.Data["users"] != nil {
			t.Errorf("%s: handler must not run on a rejected mix", q)
		}
	}
}

// TestPagination_NonPositivePageSizeRejected — `first`/`last` map to the page
// size, so a non-positive value is rejected exactly as REST rejects `?first=` <= 0.
func TestPagination_NonPositivePageSizeRejected(t *testing.T) {
	for _, q := range []string{
		`{ users(first: 0) { edges { node { id } } } }`,
		`{ users(last: -1) { edges { node { id } } } }`,
	} {
		h := &fakeReadHandler{page: queries.PageOf[execResult]{Items: []execResult{{ID: sp("u1")}}}}
		reg, ctx := newExecRegistry(h)

		resp := reg.Execute(ctx, q, nil, "")
		if len(resp.Errors) == 0 {
			t.Fatalf("%s: a non-positive page size must be rejected", q)
		}
		if got := resp.Errors[0].Extensions["semantic"]; got != "Schema" {
			t.Errorf("%s: semantic = %v, want Schema", q, got)
		}
		if resp.Data["users"] != nil {
			t.Errorf("%s: handler must not run on a rejected page size", q)
		}
	}
}
