package web

import (
	"bytes"
	"fmt"
	"reflect"
	"strings"

	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/application/translation"
	"github.com/ClaudioSchirmer/omnicore/web/export"
	"github.com/ClaudioSchirmer/omnicore/web/openapi"
	"github.com/gofiber/fiber/v3"
)

// HandleQueryExport creates a fiber.Handler that streams a view's filtered read
// as a flat tabular file (CSV/XLSX), reusing the SAME Request DTO + query
// handler the JSON list endpoint uses. The format is the pluggable enc
// (export.Encoder); everything else — plan walk, hierarchy column offset,
// labelKey headers, `?fields=` narrowing — is format-neutral.
//
// Wire behavior:
//   - Filters come from the Request DTO's `query:"X" filter:"ops"` tags (same
//     allowlist as the JSON endpoint). `?search=` and `?includeArchived=` are
//     honored; `?fields=` and `?sort=` are validated/translated against the
//     plan (the view schema), not a Response DTO.
//   - User pagination (`?limit` / `?after` / `?before` / `?onlyTotal`) is
//     IGNORED — an export returns the full filtered set, capped at maxRows
//     (the resolved per-view / yaml ceiling) sent as the read limit.
//   - On a query-string violation → 400 SchemaViolationNotification BEFORE any
//     byte is written. On a domain failure/exception → the canonical error
//     envelope. On success → Content-Type + Content-Disposition headers and the
//     encoded rows.
//
// Headers are rendered from each column's labelKey via the Translator in the
// request's Accept-Language, falling back to the Go field name when a column
// carries no label.
func HandleQueryExport[TReq HasToParamsQuery[TQ], TQ queries.FindByParamsQuery](
	pipe *pipeline.Pipeline,
	sample TReq,
	plan *queries.ExportPlan,
	translator *translation.Translator,
	maxRows int64,
	filenameBase string,
	enc export.Encoder,
	h pipeline.Handler[TQ, queries.Page],
) fiber.Handler {
	_ = sample
	reqType := reflect.TypeOf(sample)
	if reqType.Kind() == reflect.Pointer {
		reqType = reqType.Elem()
	}
	schema := extractAllowedKeys(reqType)
	pathSchema := inspectPathTags(reqType)

	return func(c fiber.Ctx) error {
		crit, fields, badField, ok := buildExportCriteria(c, schema, plan, maxRows)
		if !ok {
			return respondSchemaViolation[queries.Page](c, pipe, badField)
		}
		var req TReq
		if bad, bok := applyPathBinding(c, pathSchema, reflect.ValueOf(&req)); !bok {
			return respondSchemaViolation[queries.Page](c, pipe, bad)
		}
		appCtx := AppContext(c)
		appCtx.SetParent(c)
		q := req.ToQuery(crit)
		result := pipeline.Dispatch(pipe, appCtx, q, h)
		if !result.IsSuccess() {
			return RespondFromResult(c, result, fiber.StatusOK)
		}
		page := result.Value()

		pruned := plan.Prune(fields)
		lang := appCtx.Language()
		label := func(labelKey, goField string) string {
			if labelKey == "" {
				return goField
			}
			return translator.Render(lang, labelKey, nil)
		}

		// Buffer up to maxRows worth of rows (the ceiling already bounds memory)
		// then send. page.Items is materialized by the reader, so there is no
		// store-side streaming to preserve.
		var buf bytes.Buffer
		sink, err := enc.Open(&buf)
		if err != nil {
			return err
		}
		if err := export.Generate(pruned, page.Items, label, sink); err != nil {
			return err
		}
		if err := sink.Close(); err != nil {
			return err
		}
		c.Set(fiber.HeaderContentType, enc.ContentType())
		c.Set(fiber.HeaderContentDisposition,
			fmt.Sprintf("attachment; filename=%q", filenameBase+"."+enc.Extension()))
		return c.Send(buf.Bytes())
	}
}

// HandleQueryAsCSV is the CSV-format convenience over HandleQueryExport. The
// optional csvOpts (e.g. export.WithDelimiter(';')) are the per-route CSV
// serialization choices, fixed at mount time.
func HandleQueryAsCSV[TReq HasToParamsQuery[TQ], TQ queries.FindByParamsQuery](
	pipe *pipeline.Pipeline,
	sample TReq,
	plan *queries.ExportPlan,
	translator *translation.Translator,
	maxRows int64,
	filenameBase string,
	h pipeline.Handler[TQ, queries.Page],
	csvOpts ...export.CSVOption,
) fiber.Handler {
	return HandleQueryExport(pipe, sample, plan, translator, maxRows, filenameBase, export.CSV(csvOpts...), h)
}

// HandleQueryAsXLSX is the Excel-format convenience over HandleQueryExport —
// a drop-in sibling of HandleQueryAsCSV sharing the same plan, generator, and
// criteria handling; only the encoder differs. Optional xlsxOpts (e.g.
// export.WithSheetName("Users")) are the per-route serialization choices.
func HandleQueryAsXLSX[TReq HasToParamsQuery[TQ], TQ queries.FindByParamsQuery](
	pipe *pipeline.Pipeline,
	sample TReq,
	plan *queries.ExportPlan,
	translator *translation.Translator,
	maxRows int64,
	filenameBase string,
	h pipeline.Handler[TQ, queries.Page],
	xlsxOpts ...export.XLSXOption,
) fiber.Handler {
	return HandleQueryExport(pipe, sample, plan, translator, maxRows, filenameBase, export.XLSX(xlsxOpts...), h)
}

// HandleQueryExportSpec is the OpenAPI-aware sibling of HandleQueryExport — it
// returns the handler AND the openapi.RouteSpec describing it, so the export
// mounts on the canonical fwopenapi.Mount path (exactly like
// HandleQueryWithParamsSpec). RequestType captures TReq, so the assembler
// reflects the same `query:"X" filter:"ops"` parameter set the JSON list
// renders; FileResponse marks the success response as a file/download of the
// encoder's content type instead of the JSON envelope. The consumer mounts with
// fwopenapi.Mount(reg, group, GET, path, handler, spec, Doc{…}, RequirePermission(…)).
func HandleQueryExportSpec[TReq HasToParamsQuery[TQ], TQ queries.FindByParamsQuery](
	pipe *pipeline.Pipeline,
	sample TReq,
	plan *queries.ExportPlan,
	translator *translation.Translator,
	maxRows int64,
	filenameBase string,
	enc export.Encoder,
	h pipeline.Handler[TQ, queries.Page],
) (fiber.Handler, openapi.RouteSpec) {
	handler := HandleQueryExport(pipe, sample, plan, translator, maxRows, filenameBase, enc, h)
	return handler, openapi.RouteSpec{
		RequestType:   reflect.TypeOf((*TReq)(nil)).Elem(),
		SuccessStatus: fiber.StatusOK,
		FileResponse:  &openapi.FileResponseSpec{ContentType: enc.ContentType()},
	}
}

// HandleQueryAsCSVSpec is the OpenAPI-aware, self-sufficient CSV sibling: it
// returns (handler, RouteSpec) for fwopenapi.Mount. csvOpts (e.g.
// export.WithDelimiter(';')) are the per-route CSV options chosen at mount.
func HandleQueryAsCSVSpec[TReq HasToParamsQuery[TQ], TQ queries.FindByParamsQuery](
	pipe *pipeline.Pipeline,
	sample TReq,
	plan *queries.ExportPlan,
	translator *translation.Translator,
	maxRows int64,
	filenameBase string,
	h pipeline.Handler[TQ, queries.Page],
	csvOpts ...export.CSVOption,
) (fiber.Handler, openapi.RouteSpec) {
	return HandleQueryExportSpec(pipe, sample, plan, translator, maxRows, filenameBase, export.CSV(csvOpts...), h)
}

// HandleQueryAsXLSXSpec is the OpenAPI-aware, self-sufficient Excel sibling: it
// returns (handler, RouteSpec) for fwopenapi.Mount. xlsxOpts (e.g.
// export.WithSheetName("Users")) are the per-route XLSX options chosen at mount.
func HandleQueryAsXLSXSpec[TReq HasToParamsQuery[TQ], TQ queries.FindByParamsQuery](
	pipe *pipeline.Pipeline,
	sample TReq,
	plan *queries.ExportPlan,
	translator *translation.Translator,
	maxRows int64,
	filenameBase string,
	h pipeline.Handler[TQ, queries.Page],
	xlsxOpts ...export.XLSXOption,
) (fiber.Handler, openapi.RouteSpec) {
	return HandleQueryExportSpec(pipe, sample, plan, translator, maxRows, filenameBase, export.XLSX(xlsxOpts...), h)
}

// buildExportCriteria parses the export route's query string. Filters come from
// the Request DTO schema (reusing the JSON allowlist); `?fields=` / `?sort=` are
// validated/translated against the plan; `?search=` / `?includeArchived=` flow
// through. Pagination keys are ignored — the export forces Limit=maxRows and
// walks the full filtered set. Returns the criteria, the requested `?fields=`
// tokens (for plan pruning), and the first bad field on a 400.
func buildExportCriteria(c fiber.Ctx, schema *requestSchema, plan *queries.ExportPlan, maxRows int64) (queries.ReadCriteria, []string, string, bool) {
	// BypassMaxLimit: the export enforces its own operator-set ceiling
	// (maxRows = resolved maxExportRows), which is deliberately larger than the
	// per-view page `?limit` ceiling — the reader must not reject it.
	crit := queries.ReadCriteria{Filter: map[string]any{}, Limit: maxRows, BypassMaxLimit: true}
	wireToGo := plan.WireToGoPaths()
	var fields []string
	var bad string
	ok := true
	c.Request().URI().QueryArgs().VisitAll(func(k, v []byte) {
		if !ok {
			return
		}
		key := string(k)
		val := string(v)
		switch key {
		case "limit", "after", "before", "onlyTotal":
			// Export ignores user pagination; the row count is bounded by maxRows.
			return
		case "search":
			crit.Search = val
			return
		case "includeArchived":
			crit.IncludeArchived = val == "true"
			return
		case "fields":
			tokens := queries.SplitFields(val)
			if b, vok := plan.Validate(tokens); !vok {
				bad, ok = "fields["+b+"]", false
				return
			}
			crit.Projection = plan.Projection(tokens)
			fields = tokens
			return
		case "sort":
			sf, b, sok := parseExportSort(val, wireToGo)
			if !sok {
				bad, ok = "sort["+b+"]", false
				return
			}
			crit.Sort = sf
			return
		}
		wirePath, op := parseKeyAgainstSchema(key, schema)
		if wirePath == "" {
			bad, ok = key, false
			return
		}
		spec := schema.filters[wirePath]
		eff := op
		if eff == "" {
			eff = OpEq
		}
		if !spec.ops[eff] {
			bad, ok = key, false
			return
		}
		applyFilterParam(crit.Filter, spec, op, val)
	})
	if !ok {
		return crit, nil, bad, false
	}
	return crit, fields, "", true
}

// parseExportSort resolves a comma-separated `?sort=` value against the plan's
// wire→Go path map. Each token may carry a leading `-` (descending). An unknown
// token returns it verbatim for the 400 envelope.
func parseExportSort(val string, wireToGo map[string]string) ([]queries.SortField, string, bool) {
	tokens := strings.Split(val, ",")
	out := make([]queries.SortField, 0, len(tokens))
	for _, t := range tokens {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		desc := false
		name := t
		if strings.HasPrefix(t, "-") {
			desc, name = true, t[1:]
		}
		gp, found := wireToGo[name]
		if !found {
			return nil, t, false
		}
		out = append(out, queries.SortField{Field: gp, Desc: desc})
	}
	return out, "", true
}
