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
	"github.com/ClaudioSchirmer/omnicore/web/queryschema"
	"github.com/gofiber/fiber/v3"
)

// QueryExport creates a fiber.Handler that streams a view's filtered read
// as a flat tabular file (CSV/XLSX), reusing the SAME Request DTO + query
// handler the JSON list endpoint uses. The format is the pluggable enc
// (export.Encoder); everything else — plan walk, hierarchy column offset,
// labelKey headers, `?fields=` narrowing — is format-neutral.
//
// Wire behavior:
//   - Filters come from the Request DTO's `query:"X" filter:"ops"` tags (same
//     allowlist as the JSON endpoint). `?search=` and `?includeArchived=` are
//     honored; `?fields=` and `?orderBy=` are validated/translated against the
//     plan (the view schema), not a Response DTO.
//   - User pagination (`?first` / `?last` / `?after` / `?before` / `?onlyTotal`) is
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
// ExportView is the read-side projection surface a tabular export needs from a
// ViewDefinition, expressed as an interface so omnicore/web stays free of any
// infra import — the concrete *infra.ViewDefinition satisfies it structurally
// ("accept interfaces"). All three methods return / take only types web already
// names (the plan lives in application/queries; the ceiling and the filename are
// primitives), so the "web must not import infra" rule holds without a bridge.
type ExportView interface {
	// ExportPlan is the format-neutral column tree (root + embeds) the encoder walks.
	ExportPlan() *queries.ExportPlan
	// ResolveMaxExportRows resolves the row ceiling: the per-view override when
	// declared, else the supplied yaml default, else the framework fallback.
	ResolveMaxExportRows(yamlDefault int64) int64
	// Name is the Mongo collection name, used as the default download filename base.
	Name() string
}

// ExportDeps bundles the service-ambient inputs every tabular export shares —
// the Translator singleton (labelKey header rendering) and the yaml default
// row ceiling (query.maxExportRows). bootstrap.Run pre-packages it on
// Deps.Export so the consumer threads one value instead of spelling out
// d.Translator + d.Config.Query.MaxExportRows at every export route.
type ExportDeps struct {
	// Translator renders each column's labelKey header in the request's language.
	Translator *translation.Translator
	// MaxExportRows is the yaml-supplied default ceiling (cfg.Query.MaxExportRows),
	// fed to ExportView.ResolveMaxExportRows; a per-view override still wins.
	MaxExportRows int64
}

// exportIgnoredQueryParams are the reserved pagination/control keys an export
// route accepts on the wire but deliberately ignores — an export streams the
// full filtered set, not a page. Declared once so the SAME set is dropped in
// buildExportCriteria (runtime) AND listed on RouteSpec.OmittedQueryParams
// (OpenAPI), keeping the spec honest: Swagger never renders a knob the export
// does not honor. Every other reserved key the export DOES honor (filters,
// fields, orderBy, search, includeArchived) stays in the spec.
var exportIgnoredQueryParams = []string{"first", "last", "after", "before", "onlyTotal"}

func isExportIgnoredParam(key string) bool {
	for _, k := range exportIgnoredQueryParams {
		if k == key {
			return true
		}
	}
	return false
}

func QueryExport[TReq HasToParamsQuery[TQ], TQ queries.QueryWithParams](
	pipe *pipeline.Pipeline,
	sample TReq,
	view ExportView,
	deps ExportDeps,
	enc export.Encoder,
	h pipeline.Handler[TQ, queries.Page],
) fiber.Handler {
	_ = sample
	// Resolve the view-derived + ambient inputs once: the plan tree, the
	// labelKey Translator, the effective row ceiling (per-view override > yaml
	// default > framework fallback), and the download filename base (the view's
	// collection name). The rest of the body is format-neutral.
	plan := view.ExportPlan()
	translator := deps.Translator
	maxRows := view.ResolveMaxExportRows(deps.MaxExportRows)
	filenameBase := view.Name()
	reqType := reflect.TypeOf(sample)
	if reqType.Kind() == reflect.Pointer {
		reqType = reqType.Elem()
	}
	schema := queryschema.ExtractRequestSchema(reqType)
	pathSchema := inspectPathTags(reqType)

	return func(c fiber.Ctx) error {
		crit, badField, ok := buildExportCriteria(c, schema, plan, maxRows)
		if !ok {
			return respondSchemaViolation[queries.Page](c, pipe, badField)
		}
		var req TReq
		if bad, bok := applyPathBinding(c, pathSchema, reflect.ValueOf(&req)); !bok {
			return respondSchemaViolation[queries.Page](c, pipe, bad)
		}
		appCtx := AppContext(c)
		appCtx.SetParentIfAbsent(c)
		q := req.ToQuery(crit)
		result := pipeline.Dispatch(pipe, appCtx, q, h)
		if !result.IsSuccess() {
			return RespondFromResult(c, result, fiber.StatusOK)
		}
		page := result.Value()

		// Prune the column plan to the projection the read actually used
		// (post-ToCriteria, echoed on page.Projection) — not the pre-dispatch
		// wire ?fields — so a field a Query hid via the criteria drops its
		// CSV/XLSX column (header included), keeping ToCriteria the single
		// source of truth across JSON + export. ?fields still feeds the read
		// projection (and its validation) in buildExportCriteria.
		pruned := plan.PruneToProjection(page.Projection)
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

// QueryAsCSV is the CSV-format convenience over QueryExport. The
// optional csvOpts (e.g. export.WithDelimiter(';')) are the per-route CSV
// serialization choices, fixed at mount time.
func QueryAsCSV[TReq HasToParamsQuery[TQ], TQ queries.QueryWithParams](
	pipe *pipeline.Pipeline,
	sample TReq,
	view ExportView,
	deps ExportDeps,
	h pipeline.Handler[TQ, queries.Page],
	csvOpts ...export.CSVOption,
) fiber.Handler {
	return QueryExport(pipe, sample, view, deps, export.CSV(csvOpts...), h)
}

// QueryAsXLSX is the Excel-format convenience over QueryExport —
// a drop-in sibling of QueryAsCSV sharing the same plan, generator, and
// criteria handling; only the encoder differs. Optional xlsxOpts (e.g.
// export.WithSheetName("Users")) are the per-route serialization choices.
func QueryAsXLSX[TReq HasToParamsQuery[TQ], TQ queries.QueryWithParams](
	pipe *pipeline.Pipeline,
	sample TReq,
	view ExportView,
	deps ExportDeps,
	h pipeline.Handler[TQ, queries.Page],
	xlsxOpts ...export.XLSXOption,
) fiber.Handler {
	return QueryExport(pipe, sample, view, deps, export.XLSX(xlsxOpts...), h)
}

// QueryExportSpec is the OpenAPI-aware sibling of QueryExport — it
// returns the handler AND the openapi.RouteSpec describing it, so the export
// mounts on the canonical fwopenapi.Mount path (exactly like
// QueryWithParamsSpec). RequestType captures TReq, so the assembler
// reflects the same `query:"X" filter:"ops"` parameter set the JSON list
// renders; FileResponse marks the success response as a file/download of the
// encoder's content type instead of the JSON envelope. The consumer mounts with
// fwopenapi.Mount(reg, group, GET, path, handler, spec, Doc{…}, RequirePermission(…)).
func QueryExportSpec[TReq HasToParamsQuery[TQ], TQ queries.QueryWithParams](
	pipe *pipeline.Pipeline,
	sample TReq,
	view ExportView,
	deps ExportDeps,
	enc export.Encoder,
	h pipeline.Handler[TQ, queries.Page],
) (fiber.Handler, openapi.RouteSpec) {
	handler := QueryExport(pipe, sample, view, deps, enc, h)
	return handler, openapi.RouteSpec{
		RequestType:        reflect.TypeOf((*TReq)(nil)).Elem(),
		SuccessStatus:      fiber.StatusOK,
		FileResponse:       &openapi.FileResponseSpec{ContentType: enc.ContentType()},
		OmittedQueryParams: exportIgnoredQueryParams,
	}
}

// QueryAsCSVSpec is the OpenAPI-aware, self-sufficient CSV sibling: it
// returns (handler, RouteSpec) for fwopenapi.Mount. csvOpts (e.g.
// export.WithDelimiter(';')) are the per-route CSV options chosen at mount.
func QueryAsCSVSpec[TReq HasToParamsQuery[TQ], TQ queries.QueryWithParams](
	pipe *pipeline.Pipeline,
	sample TReq,
	view ExportView,
	deps ExportDeps,
	h pipeline.Handler[TQ, queries.Page],
	csvOpts ...export.CSVOption,
) (fiber.Handler, openapi.RouteSpec) {
	return QueryExportSpec(pipe, sample, view, deps, export.CSV(csvOpts...), h)
}

// QueryAsXLSXSpec is the OpenAPI-aware, self-sufficient Excel sibling: it
// returns (handler, RouteSpec) for fwopenapi.Mount. xlsxOpts (e.g.
// export.WithSheetName("Users")) are the per-route XLSX options chosen at mount.
func QueryAsXLSXSpec[TReq HasToParamsQuery[TQ], TQ queries.QueryWithParams](
	pipe *pipeline.Pipeline,
	sample TReq,
	view ExportView,
	deps ExportDeps,
	h pipeline.Handler[TQ, queries.Page],
	xlsxOpts ...export.XLSXOption,
) (fiber.Handler, openapi.RouteSpec) {
	return QueryExportSpec(pipe, sample, view, deps, export.XLSX(xlsxOpts...), h)
}

// buildExportCriteria parses the export route's query string. Filters come from
// the Request DTO schema (reusing the JSON allowlist); `?fields=` / `?orderBy=` are
// validated/translated against the plan; `?search=` / `?includeArchived=` flow
// through. Pagination keys are ignored — the export forces Limit=maxRows and
// walks the full filtered set. `?fields=` flows into crit.Projection (validated
// against the plan), which the reader echoes on Page.Projection for the export
// plan pruning. Returns the criteria and the first bad field on a 400.
func buildExportCriteria(c fiber.Ctx, schema *queryschema.RequestSchema, plan *queries.ExportPlan, maxRows int64) (queries.ReadCriteria, string, bool) {
	// BypassMaxLimit: the export enforces its own operator-set ceiling
	// (maxRows = resolved maxExportRows), which is deliberately larger than the
	// per-view `?first/?last` page-size ceiling — the reader must not reject it.
	crit := queries.ReadCriteria{Filter: map[string]any{}, Limit: maxRows, BypassMaxLimit: true}
	wireToGo := plan.WireToGoPaths()
	// The DTO governs what the export honors, exactly as on the JSON listing:
	// the honored controls (fields/orderBy/search/includeArchived) pass the
	// canonical gate; the pagination keys stay accepted-but-ignored — they are
	// part of the export contract as no-ops, documented via the omitted
	// OpenAPI parameters.
	var controls queryschema.Controls
	var bad string
	ok := true
	c.Request().URI().QueryArgs().VisitAll(func(k, v []byte) { //nolint:staticcheck // SA1019: fasthttp VisitAll deprecated; All() migration deferred.
		if !ok {
			return
		}
		key := string(k)
		val := string(v)
		// A reserved spelling the DTO declared as a FILTER leaf keeps its
		// filter meaning — the reserved vocabulary never shadows an explicit
		// declaration, the same carve-out the JSON listing's buildCriteria
		// applies. Only a non-shadowed key is read as a control (or ignored
		// as a pagination no-op) below.
		if _, isFilterLeaf := schema.Filters[key]; !isFilterLeaf {
			// Reserved pagination/control keys are accepted but ignored — an export
			// streams the full filtered set, not a page. The same set is omitted
			// from the generated OpenAPI parameters (RouteSpec.OmittedQueryParams),
			// so Swagger never advertises a knob the export does not honor.
			if isExportIgnoredParam(key) {
				return
			}
			switch key {
			case queryschema.KeySearch:
				controls.Search = true
				crit.Search = val
				return
			case queryschema.KeyIncludeArchived:
				controls.IncludeArchived = true
				crit.IncludeArchived = val == "true"
				return
			case queryschema.KeyFields:
				controls.Fields = true
				tokens := queries.SplitFields(val)
				if b, vok := plan.Validate(tokens); !vok {
					bad, ok = "fields["+b+"]", false
					return
				}
				crit.Projection = plan.Projection(tokens)
				return
			case queryschema.KeyOrderBy:
				controls.OrderBy = true
				sf, b, sok := parseExportOrderBy(val, wireToGo)
				if !sok {
					bad, ok = "orderBy["+b+"]", false
					return
				}
				crit.OrderBy = sf
				return
			}
		}
		wirePath, op := queryschema.ParseKeyAgainstSchema(key, schema)
		if wirePath == "" {
			bad, ok = key, false
			return
		}
		spec := schema.Filters[wirePath]
		eff := op
		if eff == "" {
			eff = OpEq
		}
		if !spec.Ops[eff] {
			bad, ok = key, false
			return
		}
		queryschema.ApplyFilterParam(crit.Filter, spec, op, val)
	})
	if !ok {
		return crit, bad, false
	}
	if violations := queryschema.ValidateControls(schema.Reserved, controls, nil); len(violations) > 0 {
		return crit, violations[0].Field(), false
	}
	return crit, "", true
}

// parseExportOrderBy resolves a comma-separated `?orderBy=` value against the
// plan's wire→Go path map. Each token may carry a leading `-` (descending). An
// unknown token returns it verbatim for the 400 envelope.
func parseExportOrderBy(val string, wireToGo map[string]string) ([]queries.OrderByField, string, bool) {
	tokens := strings.Split(val, ",")
	out := make([]queries.OrderByField, 0, len(tokens))
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
		out = append(out, queries.OrderByField{Field: gp, Desc: desc})
	}
	return out, "", true
}
