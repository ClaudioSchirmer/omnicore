package web

import (
	"encoding/csv"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/application/translation"
	"github.com/ClaudioSchirmer/omnicore/web/export"
	"github.com/ClaudioSchirmer/omnicore/web/responses"
	"github.com/gofiber/fiber/v3"
)

type expLabelModule struct{}

func (expLabelModule) Language() configuration.Language { return configuration.LangENG }
func (expLabelModule) Translations() map[string]string {
	return map[string]string{"UserNameField": "Full Name"}
}

type expCSVReq struct {
	Email *string `query:"email" filter:"eq"`

	First           *int64  `query:"first"`
	OrderBy         *string `query:"orderBy"`
	Fields          *string `query:"fields"`
	Search          *string `query:"search"`
	IncludeArchived *bool   `query:"includeArchived"`
}

// ─── the three-type read fixture backing every export case ─────────────────
//
// The export column plan is derived from the RESPONSE DTO — the same wire
// authority the JSON listing consumes — not from the view. So the fixtures
// below deliberately keep a Result field (Phone) OFF the Response: it is a
// column of the underlying document that must never reach a CSV/XLSX cell
// and must not be a legal `?fields=` token. Conversely ID IS declared on the
// Response, so it is both a real column and a legal `?fields=` token.

// expAddressResult is the nested Result segment (1:N) behind the child node.
type expAddressResult struct {
	City *string
}

// expUserResult is the application-layer Result: tagless, every field *T or
// slice (the Result-side sparse contract enforced when the Request opts into
// `?fields=`).
type expUserResult struct {
	ID        *string
	Name      *string
	Email     *string
	Phone     *string
	Addresses []expAddressResult
}

type expAddressResponse struct {
	City *string `json:"city,omitempty"`
}

// expUserResponse is the wire Response — the single authority for the export
// column set, the `?fields=`/`?orderBy=` vocabulary and the header labels.
// Name carries an exportLabelKey so its header renders through the
// Translator; every other column falls back to its json wire name.
type expUserResponse struct {
	responses.Auto
	ID        *string              `json:"id,omitempty"`
	Name      *string              `json:"name,omitempty" exportLabelKey:"UserNameField"`
	Email     *string              `json:"email,omitempty"`
	Addresses []expAddressResponse `json:"addresses,omitempty"`
}

func (expUserResponse) FromResult(r expUserResult) expUserResponse {
	return responses.AutoFromResult[expUserResponse](r)
}

type expCSVQuery struct {
	pipeline.QueryBase
	Criteria queries.ReadCriteria
}

func (q *expCSVQuery) ToCriteria(ctx *configuration.AppContext) (queries.ReadCriteria, error) {
	_ = ctx
	return q.Criteria, nil
}

func (q *expCSVQuery) FromQueryResult(_ *configuration.AppContext, r expUserResult) (expUserResult, error) {
	return r, nil
}

func (r expCSVReq) ToQuery(c queries.ReadCriteria) *expCSVQuery {
	return &expCSVQuery{Criteria: c}
}

type expCSVHandler struct {
	got  *expCSVQuery
	page queries.PageOf[expUserResult]
	err  error
}

func (h *expCSVHandler) Handle(ctx *configuration.AppContext, q *expCSVQuery) (queries.PageOf[expUserResult], error) {
	h.got = q
	if h.err != nil {
		return queries.PageOf[expUserResult]{}, h.err
	}
	crit, err := q.ToCriteria(ctx)
	if err != nil {
		return queries.PageOf[expUserResult]{}, err
	}
	// Mirror the framework query handler: echo the effective projection so the
	// export plan pruning narrows to the same fields the read used.
	page := h.page
	page.Projection = crit.Projection
	return page, nil
}

// fakeExportView satisfies web.ExportView with no infra import — proving the
// "accept interfaces" decoupling: the wrapper resolves ceiling / filename off
// this fake exactly as it would off a *infra.ViewDefinition. The interface
// shrank to those two members: the COLUMN plan comes from the Response DTO.
type fakeExportView struct {
	max  int64
	name string
}

func (f fakeExportView) ResolveMaxExportRows(yamlDefault int64) int64 {
	if f.max > 0 {
		return f.max
	}
	return yamlDefault
}
func (f fakeExportView) Name() string { return f.name }

var _ ExportView = fakeExportView{}

func newExportHandler() *expCSVHandler {
	return &expCSVHandler{page: queries.PageOf[expUserResult]{Items: []expUserResult{
		{
			ID: strPtr("u1"), Name: strPtr("John"), Email: strPtr("j@x"), Phone: strPtr("555-0001"),
			Addresses: []expAddressResult{{City: strPtr("NYC")}, {City: strPtr("LA")}},
		},
		{ID: strPtr("u2"), Name: strPtr("Jane"), Email: strPtr("j@y"), Phone: strPtr("555-0002")},
	}}}
}

func mountCSV(app *fiber.App, h *expCSVHandler) {
	tr := translation.Default()
	tr.Import(expLabelModule{})
	pipe := pipeline.New(tr)
	// The view carries no per-view override (max=0), so the yaml default
	// (ExportDeps.MaxExportRows=100) wins through ResolveMaxExportRows; the
	// filename base comes from the view's Name() ("users").
	view := fakeExportView{name: "users"}
	deps := ExportDeps{Translator: tr, MaxExportRows: 100}
	app.Get("/users.csv", QueryAsCSV(pipe, expCSVReq{}, expUserResponse{}.FromResult, view, deps, h, export.WithDelimiter(';')))
}

// expShadowReq declares the reserved `search` spelling as a FILTER leaf —
// the carve-out contract: an explicit declaration is never shadowed by the
// reserved vocabulary, on the export exactly as on the JSON listing.
type expShadowReq struct {
	Search *string `query:"search" filter:"eq"`
}

func (r expShadowReq) ToQuery(c queries.ReadCriteria) *expCSVQuery {
	return &expCSVQuery{Criteria: c}
}

// TestHandleQueryAsCSV_FilterLeafShadowsReservedSpelling proves the export
// honors the same carve-out buildCriteria applies on the listing: a DTO that
// declares `query:"search" filter:"eq"` keeps `?search=x` as a FILTER (not
// the search control, not a NotDeclared 400) — the same URL means the same
// thing on both route families.
func TestHandleQueryAsCSV_FilterLeafShadowsReservedSpelling(t *testing.T) {
	h := newExportHandler()
	app := fiber.New()
	tr := translation.Default()
	pipe := pipeline.New(tr)
	view := fakeExportView{name: "users"}
	deps := ExportDeps{Translator: tr, MaxExportRows: 100}
	app.Get("/users.csv", QueryAsCSV(pipe, expShadowReq{}, expUserResponse{}.FromResult, view, deps, h))

	resp, _ := app.Test(httptest.NewRequest("GET", "/users.csv?search=Drill", nil))
	if resp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("shadowed search must be accepted as a filter, got %d (body=%s)", resp.StatusCode, b)
	}
	crit, err := h.got.ToCriteria(nil)
	if err != nil {
		t.Fatalf("ToCriteria: %v", err)
	}
	if crit.Search != "" {
		t.Fatalf("shadowed spelling must NOT feed the search control, got %q", crit.Search)
	}
	if got := crit.Filter["Search"]; got != "Drill" {
		t.Fatalf("shadowed spelling must land as the declared filter, got Filter=%v", crit.Filter)
	}
}

func parseSemicolonCSV(t *testing.T, body io.Reader) [][]string {
	t.Helper()
	r := csv.NewReader(body)
	r.Comma = ';'
	r.FieldsPerRecord = -1
	recs, err := r.ReadAll()
	if err != nil {
		t.Fatalf("re-parse CSV: %v", err)
	}
	return recs
}

func TestHandleQueryAsCSV_FullHierarchy(t *testing.T) {
	app := fiber.New()
	h := newExportHandler()
	mountCSV(app, h)

	resp, err := app.Test(httptest.NewRequest("GET", "/users.csv?email=j@x&first=5", nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, b)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/csv; charset=utf-8" {
		t.Fatalf("content-type=%q", ct)
	}
	if cd := resp.Header.Get("Content-Disposition"); cd != `attachment; filename="users.csv"` {
		t.Fatalf("content-disposition=%q", cd)
	}
	// user pagination ignored; limit forced to the export ceiling, and the
	// per-view page ceiling is bypassed (the export enforces its own).
	if h.got.Criteria.Limit != 100 {
		t.Fatalf("expected forced Limit=100, got %d", h.got.Criteria.Limit)
	}
	if !h.got.Criteria.BypassMaxLimit {
		t.Fatal("expected BypassMaxLimit=true so the export ceiling isn't capped by the page ceiling")
	}
	if h.got.Criteria.Filter["Email"] != "j@x" {
		t.Fatalf("expected Email filter applied, got %v", h.got.Criteria.Filter["Email"])
	}

	recs := parseSemicolonCSV(t, resp.Body)
	// header / John / addr header(depth1) / NYC / LA / Jane
	// (the blank separator after John's cascade is skipped by encoding/csv)
	if len(recs) != 6 {
		t.Fatalf("records=%d: %v", len(recs), recs)
	}
	// The column set is the Response's, in declaration order: id (declared →
	// exported), name (labelKey-rendered header), email (json-name fallback).
	if len(recs[0]) != 3 || recs[0][0] != "id" || recs[0][1] != "Full Name" || recs[0][2] != "email" {
		t.Fatalf("header (Response-derived, labelKey-rendered) = %v", recs[0])
	}
	if recs[1][0] != "u1" || recs[1][1] != "John" || recs[1][2] != "j@x" {
		t.Fatalf("root data = %v", recs[1])
	}
	if len(recs[2]) != 2 || recs[2][0] != "" || recs[2][1] != "city" {
		t.Fatalf("addresses header should be offset one column: %v", recs[2])
	}
	if recs[3][0] != "" || recs[3][1] != "NYC" || recs[4][1] != "LA" {
		t.Fatalf("addresses data offset wrong: %v / %v", recs[3], recs[4])
	}
	if recs[5][0] != "u2" || recs[5][1] != "Jane" {
		t.Fatalf("second root row = %v", recs[5])
	}
}

func TestHandleQueryAsCSV_FieldsNarrowing(t *testing.T) {
	app := fiber.New()
	mountCSV(app, newExportHandler())

	resp, _ := app.Test(httptest.NewRequest("GET", "/users.csv?fields=name", nil))
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	recs := parseSemicolonCSV(t, resp.Body)
	// only the Name column survives → header + 2 data rows, no addresses
	if len(recs) != 3 {
		t.Fatalf("records=%d: %v", len(recs), recs)
	}
	for _, r := range recs {
		if len(r) != 1 {
			t.Fatalf("expected single column rows, got %v", r)
		}
	}
	if recs[0][0] != "Full Name" || recs[1][0] != "John" || recs[2][0] != "Jane" {
		t.Fatalf("narrowed rows = %v", recs)
	}
}

// TestHandleQueryAsCSV_ResponseDeclaredIDIsExportable pins the read-side
// symmetry behavior change: the export vocabulary is the Response DTO's, so
// `id` — declared on expUserResponse — is a legal `?fields=` token AND a real
// column, exactly like on the JSON listing. It used to be neither (the plan
// came from the view, which never published the identity column).
func TestHandleQueryAsCSV_ResponseDeclaredIDIsExportable(t *testing.T) {
	app := fiber.New()
	mountCSV(app, newExportHandler())

	resp, _ := app.Test(httptest.NewRequest("GET", "/users.csv?fields=id", nil))
	if resp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("?fields=id must be accepted, got %d (body=%s)", resp.StatusCode, b)
	}
	recs := parseSemicolonCSV(t, resp.Body)
	if len(recs) != 3 {
		t.Fatalf("records=%d: %v", len(recs), recs)
	}
	if recs[0][0] != "id" {
		t.Fatalf("expected the id column header, got %v", recs[0])
	}
	if recs[1][0] != "u1" || recs[2][0] != "u2" {
		t.Fatalf("expected the identity values as cells, got %v / %v", recs[1], recs[2])
	}
}

// TestHandleQueryAsCSV_ResultFieldAbsentFromResponseNeverExports is the other
// half of the same change: a document/Result column the Response does NOT
// declare (Phone) is outside the export vocabulary — it is neither a column
// nor a legal `?fields=` token.
func TestHandleQueryAsCSV_ResultFieldAbsentFromResponseNeverExports(t *testing.T) {
	app := fiber.New()
	mountCSV(app, newExportHandler())

	resp, _ := app.Test(httptest.NewRequest("GET", "/users.csv?fields=phone", nil))
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("expected 400 for a token absent from the Response, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "fields[phone]") {
		t.Errorf("expected the canonical fields[phone] spelling, got %s", body)
	}

	// And the full export never materializes the column either.
	app2 := fiber.New()
	mountCSV(app2, newExportHandler())
	full, _ := app2.Test(httptest.NewRequest("GET", "/users.csv", nil))
	if full.StatusCode != fiber.StatusOK {
		t.Fatalf("status=%d", full.StatusCode)
	}
	for _, r := range parseSemicolonCSV(t, full.Body) {
		for _, cell := range r {
			if cell == "phone" || cell == "555-0001" || cell == "555-0002" {
				t.Fatalf("a field absent from the Response must never export, got row %v", r)
			}
		}
	}
}

// expHideQuery simulates a Query that removes a field from the read via the
// criteria (the ReadCriteria.Hide / field-access case): ToCriteria excludes
// Email, so the export plan pruning must drop the Email column AND its header,
// not just blank the cells.
type expHideQuery struct {
	pipeline.QueryBase
	Criteria queries.ReadCriteria
}

func (q *expHideQuery) ToCriteria(_ *configuration.AppContext) (queries.ReadCriteria, error) {
	crit := q.Criteria
	if crit.Projection == nil {
		crit.Projection = map[string]int{}
	}
	crit.Projection["Email"] = 0 // exclusion overlay — whole-doc read minus Email
	return crit, nil
}

func (q *expHideQuery) FromQueryResult(_ *configuration.AppContext, r expUserResult) (expUserResult, error) {
	return r, nil
}

type expHideReq struct{}

func (expHideReq) ToQuery(c queries.ReadCriteria) *expHideQuery { return &expHideQuery{Criteria: c} }

type expHideHandler struct{ page queries.PageOf[expUserResult] }

func (h *expHideHandler) Handle(ctx *configuration.AppContext, q *expHideQuery) (queries.PageOf[expUserResult], error) {
	crit, err := q.ToCriteria(ctx)
	if err != nil {
		return queries.PageOf[expUserResult]{}, err
	}
	page := h.page
	page.Projection = crit.Projection // mirror the framework query handler
	return page, nil
}

func TestHandleQueryAsCSV_ToCriteriaExclusionDropsColumnAndHeader(t *testing.T) {
	app := fiber.New()
	tr := translation.Default()
	tr.Import(expLabelModule{})
	pipe := pipeline.New(tr)
	view := fakeExportView{name: "users"}
	deps := ExportDeps{Translator: tr, MaxExportRows: 100}
	h := &expHideHandler{page: queries.PageOf[expUserResult]{Items: []expUserResult{
		{ID: strPtr("u1"), Name: strPtr("John"), Email: strPtr("j@x")},
		{ID: strPtr("u2"), Name: strPtr("Jane"), Email: strPtr("j@y")},
	}}}
	app.Get("/users.csv", QueryAsCSV(pipe, expHideReq{}, expUserResponse{}.FromResult, view, deps, h, export.WithDelimiter(';')))

	resp, _ := app.Test(httptest.NewRequest("GET", "/users.csv", nil))
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	recs := parseSemicolonCSV(t, resp.Body)
	// Email must be gone entirely — header AND values — across every row.
	for _, r := range recs {
		for _, cell := range r {
			if cell == "email" || cell == "j@x" || cell == "j@y" {
				t.Fatalf("Email leaked despite ToCriteria exclusion: %v", recs)
			}
		}
	}
	if len(recs) == 0 || len(recs[0]) != 2 || recs[0][0] != "id" || recs[0][1] != "Full Name" {
		t.Fatalf("expected the surviving [id, Full Name] header, got %v", recs)
	}
}

// The export *Spec wrapper lists the reserved pagination keys on
// RouteSpec.OmittedQueryParams so the OpenAPI generator strips them — the
// export accepts-but-ignores them at runtime, so the spec must not advertise
// them. Filters and the honored control keys stay (RequestType is reflected).
func TestHandleQueryAsCSVSpec_OmitsPaginationFromSpec(t *testing.T) {
	tr := translation.Default()
	pipe := pipeline.New(tr)
	view := fakeExportView{name: "users"}
	deps := ExportDeps{Translator: tr, MaxExportRows: 100}

	_, spec := QueryAsCSVSpec(pipe, expCSVReq{}, expUserResponse{}.FromResult, view, deps,
		&expCSVHandler{}, export.WithDelimiter(';'))

	got := map[string]bool{}
	for _, k := range spec.OmittedQueryParams {
		got[k] = true
	}
	for _, want := range []string{"first", "last", "after", "before", "onlyTotal"} {
		if !got[want] {
			t.Fatalf("export spec must omit %q; OmittedQueryParams=%v", want, spec.OmittedQueryParams)
		}
	}
	if spec.RequestType == nil {
		t.Fatal("export spec must still reflect RequestType so filters render in Swagger")
	}
}

func TestHandleQueryAsCSV_UnknownQueryKeyReturns400(t *testing.T) {
	app := fiber.New()
	h := newExportHandler()
	mountCSV(app, h)

	resp, _ := app.Test(httptest.NewRequest("GET", "/users.csv?bogus=1", nil))
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	if h.got != nil {
		t.Fatal("handler must not run on a rejected query string")
	}
}

func TestHandleQueryAsCSV_UnknownFieldsTokenReturns400(t *testing.T) {
	app := fiber.New()
	mountCSV(app, newExportHandler())

	resp, _ := app.Test(httptest.NewRequest("GET", "/users.csv?fields=nope", nil))
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("expected 400 for unknown ?fields token, got %d", resp.StatusCode)
	}
}

func TestHandleQueryAsCSV_HandlerErrorNoCSV(t *testing.T) {
	app := fiber.New()
	h := newExportHandler()
	h.err = errors.New("boom")
	mountCSV(app, h)

	resp, _ := app.Test(httptest.NewRequest("GET", "/users.csv", nil))
	if resp.StatusCode == fiber.StatusOK {
		t.Fatal("expected a non-200 error envelope")
	}
	if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(ct, "text/csv") {
		t.Fatalf("error path must not emit CSV, got content-type %q", ct)
	}
}
