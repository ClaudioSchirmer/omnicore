package web

import (
	"bytes"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/application/translation"
	"github.com/ClaudioSchirmer/omnicore/web/export"
	"github.com/gofiber/fiber/v3"
	"github.com/xuri/excelize/v2"
)

// reuses fakeExportView / expCSVReq / expCSVHandler / expCSVPlan / newExportHandler
// / expLabelModule from handle_query_export_test.go (same package).

func mountXLSX(app *fiber.App, h *expCSVHandler) {
	tr := translation.Default()
	tr.Import(expLabelModule{})
	pipe := pipeline.New(tr)
	view := fakeExportView{plan: expCSVPlan(), name: "users"}
	deps := ExportDeps{Translator: tr, MaxExportRows: 100}
	app.Get("/users.xlsx", HandleQueryAsXLSX(pipe, expCSVReq{}, view, deps, h, export.WithSheetName("Users")))
}

func TestHandleQueryAsXLSX_FullHierarchy(t *testing.T) {
	app := fiber.New()
	h := newExportHandler()
	mountXLSX(app, h)

	resp, err := app.Test(httptest.NewRequest("GET", "/users.xlsx?email=j@x&limit=5", nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, b)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet" {
		t.Fatalf("content-type=%q", ct)
	}
	if cd := resp.Header.Get("Content-Disposition"); cd != `attachment; filename="users.xlsx"` {
		t.Fatalf("content-disposition=%q", cd)
	}
	if h.got.Criteria.Limit != 100 || !h.got.Criteria.BypassMaxLimit {
		t.Fatalf("export must force Limit=100 + BypassMaxLimit, got %+v", h.got.Criteria)
	}
	if h.got.Criteria.Filter["Email"] != "j@x" {
		t.Fatalf("expected Email filter applied, got %v", h.got.Criteria.Filter["Email"])
	}

	raw, _ := io.ReadAll(resp.Body)
	f, err := excelize.OpenReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("open xlsx: %v", err)
	}
	defer f.Close()
	// header rendered from labelKey via the Translator
	if got, _ := f.GetCellValue("Users", "A1"); got != "Full Name" {
		t.Fatalf("A1 (labelKey header) = %q, want Full Name", got)
	}
	if got, _ := f.GetCellValue("Users", "A2"); got != "John" {
		t.Fatalf("A2 = %q, want John", got)
	}
}

func TestHandleQueryAsXLSX_UnknownQueryKey_400(t *testing.T) {
	app := fiber.New()
	h := newExportHandler()
	mountXLSX(app, h)

	resp, _ := app.Test(httptest.NewRequest("GET", "/users.xlsx?bogus=1", nil))
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	if h.got != nil {
		t.Fatal("handler must not run on a rejected query string")
	}
}

func TestHandleQueryAsXLSXSpec_OmitsPaginationAndMarksFileResponse(t *testing.T) {
	tr := translation.Default()
	pipe := pipeline.New(tr)
	view := fakeExportView{plan: expCSVPlan(), name: "users"}
	deps := ExportDeps{Translator: tr, MaxExportRows: 100}

	_, spec := HandleQueryAsXLSXSpec(pipe, expCSVReq{}, view, deps,
		&expCSVHandler{}, export.WithSheetName("Users"))

	if spec.FileResponse == nil {
		t.Fatal("XLSX spec must mark FileResponse")
	}
	if spec.FileResponse.ContentType != "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet" {
		t.Fatalf("unexpected FileResponse content-type: %q", spec.FileResponse.ContentType)
	}
	if spec.RequestType == nil {
		t.Fatal("export spec must reflect RequestType so filters render in Swagger")
	}
	got := map[string]bool{}
	for _, k := range spec.OmittedQueryParams {
		got[k] = true
	}
	for _, want := range []string{"limit", "after", "before", "onlyTotal"} {
		if !got[want] {
			t.Fatalf("export spec must omit %q; got %v", want, spec.OmittedQueryParams)
		}
	}
}
