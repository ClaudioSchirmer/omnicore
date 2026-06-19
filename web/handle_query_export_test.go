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
	Limit *int64  `query:"limit"`
}

type expCSVQuery struct {
	pipeline.QueryBase
	Criteria queries.ReadCriteria
}

func (q *expCSVQuery) ToCriteria(ctx *configuration.AppContext) queries.ReadCriteria {
	_ = ctx
	return q.Criteria
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
	return h.page, h.err
}

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
	app.Get("/users.csv", HandleQueryAsCSV(pipe, expCSVReq{}, expCSVPlan(), tr, 100, "users", h, export.WithDelimiter(';')))
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

	resp, err := app.Test(httptest.NewRequest("GET", "/users.csv?email=j@x&limit=5", nil))
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
