package web

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/application/translation"
	"github.com/ClaudioSchirmer/omnicore/web/responses"
	"github.com/gofiber/fiber/v3"
)

// pagFindRequest opts into the full pagination control surface — both `after`
// and `before` plus `sort` — so the tests exercise the post-loop conflict
// matrix and cursor-tuple alignment checks.
type pagFindRequest struct {
	Name *string `query:"name" filter:"eq"`

	Limit  *int64  `query:"limit"`
	After  *string `query:"after"`
	Before *string `query:"before"`
	Sort   *string `query:"sort"`
}

type pagQuery struct {
	pipeline.QueryBase
	Criteria queries.ReadCriteria
}

func (q *pagQuery) ToCriteria(_ *configuration.AppContext) (queries.ReadCriteria, error) {
	return q.Criteria, nil
}

func (r pagFindRequest) ToQuery(crit queries.ReadCriteria) *pagQuery {
	return &pagQuery{Criteria: crit}
}

type pagHandler struct {
	got *pagQuery
}

func (h *pagHandler) Handle(_ *configuration.AppContext, q *pagQuery) (queries.Page, error) {
	h.got = q
	return queries.Page{}, nil
}

func mountPaginationWrapper() (*fiber.App, *pagHandler) {
	app := fiber.New()
	pipe := pipeline.New(translation.Default())
	h := &pagHandler{}
	app.Get("/users", HandleQueryWithParams(pipe, pagFindRequest{}, responses.RawDoc, h))
	return app, h
}

// validCursor encodes a tuple of the given length with an empty context
// hash (default context). Useful when the test needs a cursor whose tuple
// shape matches a declared `?sort=` AND the request carries no filter,
// sort, search, or includeArchived (default context hashes to empty).
func validCursor(t *testing.T, tuple []any) string {
	t.Helper()
	s, err := queries.EncodeCursor(tuple, "")
	if err != nil {
		t.Fatalf("EncodeCursor: %v", err)
	}
	return s
}

// validCursorWithContext encodes a tuple bound to a specific listing
// context. Use when the test request carries non-default state (sort,
// search, archived flag, filter) so the wrapper's HashContext check passes.
func validCursorWithContext(t *testing.T, tuple []any, filter map[string]any, sortFields []queries.SortField, search string, includeArchived bool) string {
	t.Helper()
	hash := queries.HashContext(filter, sortFields, search, includeArchived)
	s, err := queries.EncodeCursor(tuple, hash)
	if err != nil {
		t.Fatalf("EncodeCursor: %v", err)
	}
	return s
}

// ---------------------------------------------------------------------------
// Strict ?limit= validation — non-numeric, zero, negative all 400.
// ---------------------------------------------------------------------------

func TestPagination_Limit_NonNumeric_Rejected(t *testing.T) {
	app, h := mountPaginationWrapper()
	resp, _ := app.Test(httptest.NewRequest("GET", "/users?limit=abc", nil))
	if resp.StatusCode != fiber.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("want 400, got %d (body=%s)", resp.StatusCode, body)
	}
	if h.got != nil {
		t.Errorf("handler must NOT be called on schema rejection")
	}
}

func TestPagination_Limit_Zero_Rejected(t *testing.T) {
	app, h := mountPaginationWrapper()
	resp, _ := app.Test(httptest.NewRequest("GET", "/users?limit=0", nil))
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
	if h.got != nil {
		t.Errorf("handler must NOT be called")
	}
}

func TestPagination_Limit_Negative_Rejected(t *testing.T) {
	app, h := mountPaginationWrapper()
	resp, _ := app.Test(httptest.NewRequest("GET", "/users?limit=-5", nil))
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
	if h.got != nil {
		t.Errorf("handler must NOT be called")
	}
}

func TestPagination_Limit_Numeric_Passes(t *testing.T) {
	app, h := mountPaginationWrapper()
	resp, _ := app.Test(httptest.NewRequest("GET", "/users?limit=15", nil))
	if resp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("want 200, got %d (body=%s)", resp.StatusCode, body)
	}
	if h.got == nil || h.got.Criteria.Limit != 15 {
		t.Errorf("expected criteria.Limit=15, got %v", h.got)
	}
}

// ---------------------------------------------------------------------------
// Strict ?after= / ?before= validation — malformed base64, malformed JSON,
// wrong schema version all 400.
// ---------------------------------------------------------------------------

func TestPagination_After_MalformedBase64_Rejected(t *testing.T) {
	app, h := mountPaginationWrapper()
	resp, _ := app.Test(httptest.NewRequest("GET", "/users?after=not-base64---", nil))
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
	if h.got != nil {
		t.Errorf("handler must NOT be called")
	}
}

func TestPagination_Before_MalformedBase64_Rejected(t *testing.T) {
	app, h := mountPaginationWrapper()
	resp, _ := app.Test(httptest.NewRequest("GET", "/users?before=---bad---", nil))
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
	if h.got != nil {
		t.Errorf("handler must NOT be called")
	}
}

func TestPagination_After_ValidShape_Passes(t *testing.T) {
	app, h := mountPaginationWrapper()
	cursor := validCursor(t, []any{"abc-123"})
	resp, _ := app.Test(httptest.NewRequest("GET", "/users?after="+cursor, nil))
	if resp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("want 200, got %d (body=%s)", resp.StatusCode, body)
	}
	if h.got == nil || h.got.Criteria.After != cursor {
		t.Errorf("expected criteria.After=%q, got %v", cursor, h.got)
	}
}

// ---------------------------------------------------------------------------
// ?after= + ?before= mutually exclusive.
// ---------------------------------------------------------------------------

func TestPagination_AfterAndBefore_MutuallyExclusive(t *testing.T) {
	app, h := mountPaginationWrapper()
	c := validCursor(t, []any{"abc-123"})
	url := "/users?after=" + c + "&before=" + c
	resp, _ := app.Test(httptest.NewRequest("GET", url, nil))
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("want 400 on after+before, got %d", resp.StatusCode)
	}
	if h.got != nil {
		t.Errorf("handler must NOT be called")
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "after,before") {
		t.Errorf("envelope should surface the after,before field name, got %s", body)
	}
}

// ---------------------------------------------------------------------------
// Cursor ↔ Sort tuple-length alignment.
// ---------------------------------------------------------------------------

func TestPagination_Cursor_TupleLenMismatchWithSort_Rejected(t *testing.T) {
	app, h := mountPaginationWrapper()
	// Cursor encodes a 1-element tuple (just _id) but request declares 1 sort
	// field — alignment requires len(K)-1 == 1, i.e. len(K) == 2. Mismatch.
	cursor := validCursor(t, []any{"abc-123"})
	url := "/users?sort=name&after=" + cursor
	resp, _ := app.Test(httptest.NewRequest("GET", url, nil))
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("want 400 on tuple/sort mismatch, got %d", resp.StatusCode)
	}
	if h.got != nil {
		t.Errorf("handler must NOT be called")
	}
}

func TestPagination_Cursor_TupleLenMatchesSort_Passes(t *testing.T) {
	app, h := mountPaginationWrapper()
	// Sort=name contributes to the hash; the cursor must carry the matching
	// HashContext to pass the wrapper's full alignment check.
	cursor := validCursorWithContext(t,
		[]any{"Alice", "abc-123"},
		nil,
		[]queries.SortField{{Field: "name"}},
		"", false)
	url := "/users?sort=name&after=" + cursor
	resp, _ := app.Test(httptest.NewRequest("GET", url, nil))
	if resp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("want 200, got %d (body=%s)", resp.StatusCode, body)
	}
	if h.got == nil {
		t.Fatal("handler should be called")
	}
}

func TestPagination_Cursor_FilterHashMismatch_Rejected(t *testing.T) {
	app, h := mountPaginationWrapper()
	// Cursor issued for filter {name="Alice"}; current request carries
	// {name="Bob"}. Hash mismatch → 400 SchemaViolation.
	cursorHash := queries.HashContext(map[string]any{"Name": "Alice"}, nil, "", false)
	encoded, err := queries.EncodeCursor([]any{"abc-123"}, cursorHash)
	if err != nil {
		t.Fatalf("EncodeCursor: %v", err)
	}
	url := "/users?name=Bob&after=" + encoded
	resp, _ := app.Test(httptest.NewRequest("GET", url, nil))
	if resp.StatusCode != fiber.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("want 400 on filter mismatch, got %d (body=%s)", resp.StatusCode, body)
	}
	if h.got != nil {
		t.Errorf("handler must NOT be called")
	}
}

func TestPagination_Cursor_FilterHashMatch_Passes(t *testing.T) {
	app, h := mountPaginationWrapper()
	// Cursor issued for filter {name="Alice"}; current request carries the
	// same filter. Hash match → request flows through.
	filter := map[string]any{"Name": "Alice"}
	cursorHash := queries.HashContext(filter, nil, "", false)
	encoded, err := queries.EncodeCursor([]any{"abc-123"}, cursorHash)
	if err != nil {
		t.Fatalf("EncodeCursor: %v", err)
	}
	url := "/users?name=Alice&after=" + encoded
	resp, _ := app.Test(httptest.NewRequest("GET", url, nil))
	if resp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("want 200 on filter match, got %d (body=%s)", resp.StatusCode, body)
	}
	if h.got == nil {
		t.Fatal("handler should be called when filter matches the cursor's hash")
	}
}

func TestPagination_Cursor_FilterAddedMidNavigation_Rejected(t *testing.T) {
	app, h := mountPaginationWrapper()
	// Cursor issued without filter (H=""); current request adds ?name=Bob.
	// Same rejection — consumer must request page 1 of the filtered query.
	encoded, err := queries.EncodeCursor([]any{"abc-123"}, "")
	if err != nil {
		t.Fatalf("EncodeCursor: %v", err)
	}
	url := "/users?name=Bob&after=" + encoded
	resp, _ := app.Test(httptest.NewRequest("GET", url, nil))
	if resp.StatusCode != fiber.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("want 400, got %d (body=%s)", resp.StatusCode, body)
	}
	if h.got != nil {
		t.Errorf("handler must NOT be called")
	}
}

func TestPagination_Cursor_SortChangeMidNavigation_Rejected(t *testing.T) {
	app, h := mountPaginationWrapper()
	// Cursor issued for sort=name; current request switches to sort=email.
	// Both have same tuple length so the structural check passes; the hash
	// check catches the silent direction/field change.
	cursor := validCursorWithContext(t,
		[]any{"Alice", "abc-123"},
		nil,
		[]queries.SortField{{Field: "name"}},
		"", false)
	url := "/users?sort=email&after=" + cursor
	resp, _ := app.Test(httptest.NewRequest("GET", url, nil))
	if resp.StatusCode != fiber.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("want 400 on sort field change, got %d (body=%s)", resp.StatusCode, body)
	}
	if h.got != nil {
		t.Errorf("handler must NOT be called when sort field changed")
	}
}

func TestPagination_Cursor_SortDirectionFlipMidNavigation_Rejected(t *testing.T) {
	app, h := mountPaginationWrapper()
	// Cursor issued for sort=name (asc); current request flips to sort=-name.
	cursor := validCursorWithContext(t,
		[]any{"Alice", "abc-123"},
		nil,
		[]queries.SortField{{Field: "name", Desc: false}},
		"", false)
	url := "/users?sort=-name&after=" + cursor
	resp, _ := app.Test(httptest.NewRequest("GET", url, nil))
	if resp.StatusCode != fiber.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("want 400 on sort direction flip, got %d (body=%s)", resp.StatusCode, body)
	}
	if h.got != nil {
		t.Errorf("handler must NOT be called when sort direction flipped")
	}
}

func TestPagination_Cursor_NoSort_OnlyIDTuple_Passes(t *testing.T) {
	app, h := mountPaginationWrapper()
	cursor := validCursor(t, []any{"abc-123"})
	resp, _ := app.Test(httptest.NewRequest("GET", "/users?after="+cursor, nil))
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	if h.got == nil {
		t.Fatal("handler should be called")
	}
}
