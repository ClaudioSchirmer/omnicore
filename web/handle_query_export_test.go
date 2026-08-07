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

type expCSVQuery struct {
	pipeline.QueryBase
	Criteria queries.ReadCriteria
}

func (q *expCSVQuery) ToCriteria(ctx *configuration.AppContext) (queries.ReadCriteria, error) {
	_ = ctx
	return q.Criteria, nil
}

func (r expCSVReq) ToQuery(c queries.ReadCriteria) *expCSVQuery {
	return &expCSVQuery{Criteria: c}
}

type expCSVHandler struct {
	got  *expCSVQuery
	page queries.Page
	err  error
}

func (h *expCSVHandler) Handle(ctx *configuration.AppContext, q *expCSVQuery) (queries.Page, error) {
	h.got = q
	if h.err != nil {
		return queries.Page{}, h.err
	}
	crit, err := q.ToCriteria(ctx)
	if err != nil {
		return queries.Page{}, err
	}
	// Mirror MongoViewReader.ReadPage: echo the effective projection so the
	// export plan pruning narrows to the same fields the read used.
	page := h.page
	page.Projection = crit.Projection
	return page, nil
}

// fakeExportView satisfies web.ExportView with no infra import — proving the
// "accept interfaces" decoupling: the wrapper resolves plan / ceiling / filename
// off this fake exactly as it would off a *infra.ViewDefinition.
type fakeExportView struct {
	plan *queries.ExportPlan
	max  int64
	name string
}

func (f fakeExportView) ExportPlan() *queries.ExportPlan { return f.plan }
func (f fakeExportView) ResolveMaxExportRows(yamlDefault int64) int64 {
	if f.max > 0 {
		return f.max
	}
	return yamlDefault
}
func (f fakeExportView) Name() string { return f.name }

func expCSVPlan() *queries.ExportPlan {
	return &queries.ExportPlan{Root: &queries.ExportNode{
		Columns: []queries.ExportColumn{
			{GoField: "Name", WireLeaf: "name", LabelKey: "UserNameField"},
			{GoField: "Email", WireLeaf: "email"},
		},
		Children: []*queries.ExportNode{{
			GoSegment: "Addresses", WireSegment: "addresses",
			Columns: []queries.ExportColumn{{GoField: "City", WireLeaf: "city"}},
		}},
	}}
}

func newExportHandler() *expCSVHandler {
	return &expCSVHandler{page: queries.Page{Items: []map[string]any{
		{"Name": "John", "Email": "j@x", "Addresses": []map[string]any{{"City": "NYC"}, {"City": "LA"}}},
		{"Name": "Jane", "Email": "j@y"},
	}}}
}

func mountCSV(app *fiber.App, h *expCSVHandler) {
	tr := translation.Default()
	tr.Import(expLabelModule{})
	pipe := pipeline.New(tr)
	// The view carries no per-view override (max=0), so the yaml default
	// (ExportDeps.MaxExportRows=100) wins through ResolveMaxExportRows; the
	// filename base comes from the view's Name() ("users").
	view := fakeExportView{plan: expCSVPlan(), name: "users"}
	deps := ExportDeps{Translator: tr, MaxExportRows: 100}
	app.Get("/users.csv", QueryAsCSV(pipe, expCSVReq{}, view, deps, h, export.WithDelimiter(';')))
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
	view := fakeExportView{plan: expCSVPlan(), name: "users"}
	deps := ExportDeps{Translator: tr, MaxExportRows: 100}
	app.Get("/users.csv", QueryAsCSV(pipe, expShadowReq{}, view, deps, h))

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
	if len(recs) != 6 {
		t.Fatalf("records=%d: %v", len(recs), recs)
	}
	if recs[0][0] != "Full Name" || recs[0][1] != "Email" {
		t.Fatalf("header (labelKey-rendered) = %v", recs[0])
	}
	if recs[1][0] != "John" || recs[1][1] != "j@x" {
		t.Fatalf("root data = %v", recs[1])
	}
	if len(recs[2]) != 2 || recs[2][0] != "" || recs[2][1] != "City" {
		t.Fatalf("addresses header should be offset one column: %v", recs[2])
	}
	if recs[3][0] != "" || recs[3][1] != "NYC" || recs[4][1] != "LA" {
		t.Fatalf("addresses data offset wrong: %v / %v", recs[3], recs[4])
	}
	if recs[5][0] != "Jane" {
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

type expHideReq struct{}

func (expHideReq) ToQuery(c queries.ReadCriteria) *expHideQuery { return &expHideQuery{Criteria: c} }

type expHideHandler struct{ page queries.Page }

func (h *expHideHandler) Handle(ctx *configuration.AppContext, q *expHideQuery) (queries.Page, error) {
	crit, err := q.ToCriteria(ctx)
	if err != nil {
		return queries.Page{}, err
	}
	page := h.page
	page.Projection = crit.Projection // mirror MongoViewReader.ReadPage
	return page, nil
}

func TestHandleQueryAsCSV_ToCriteriaExclusionDropsColumnAndHeader(t *testing.T) {
	app := fiber.New()
	tr := translation.Default()
	tr.Import(expLabelModule{})
	pipe := pipeline.New(tr)
	view := fakeExportView{plan: expCSVPlan(), name: "users"}
	deps := ExportDeps{Translator: tr, MaxExportRows: 100}
	h := &expHideHandler{page: queries.Page{Items: []map[string]any{
		{"Name": "John", "Email": "j@x"},
		{"Name": "Jane", "Email": "j@y"},
	}}}
	app.Get("/users.csv", QueryAsCSV(pipe, expHideReq{}, view, deps, h, export.WithDelimiter(';')))

	resp, _ := app.Test(httptest.NewRequest("GET", "/users.csv", nil))
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	recs := parseSemicolonCSV(t, resp.Body)
	// Email must be gone entirely — header AND values — across every row.
	for _, r := range recs {
		for _, cell := range r {
			if cell == "Email" || cell == "j@x" || cell == "j@y" {
				t.Fatalf("Email leaked despite ToCriteria exclusion: %v", recs)
			}
		}
	}
	if len(recs) == 0 || recs[0][0] != "Full Name" {
		t.Fatalf("expected surviving Name header 'Full Name', got %v", recs)
	}
}

// The export *Spec wrapper lists the reserved pagination keys on
// RouteSpec.OmittedQueryParams so the OpenAPI generator strips them — the
// export accepts-but-ignores them at runtime, so the spec must not advertise
// them. Filters and the honored control keys stay (RequestType is reflected).
func TestHandleQueryAsCSVSpec_OmitsPaginationFromSpec(t *testing.T) {
	tr := translation.Default()
	pipe := pipeline.New(tr)
	view := fakeExportView{plan: expCSVPlan(), name: "users"}
	deps := ExportDeps{Translator: tr, MaxExportRows: 100}

	_, spec := QueryAsCSVSpec(pipe, expCSVReq{}, view, deps,
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
