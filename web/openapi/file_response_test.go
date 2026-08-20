package openapi

import (
	"reflect"
	"testing"

	"github.com/gofiber/fiber/v3"
)

// A RouteSpec carrying FileResponse documents the success status as a raw
// file/stream of the declared content type — NOT the JSON envelope — while the
// query/filter parameters (reflected from RequestType) and the standard error
// envelopes render exactly as on a normal canonical query route.
func TestSpec_FileResponse_SuccessIsBinaryFileWithReflectedParams(t *testing.T) {
	reg := NewRegistry()
	Mount(reg, fiber.New(), fiber.MethodGet, "/users.csv",
		func(c fiber.Ctx) error { return nil },
		RouteSpec{
			RequestType:   reflect.TypeOf(specListRequest{}),
			SuccessStatus: fiber.StatusOK,
			FileResponse:  &FileResponseSpec{ContentType: "text/csv"},
		},
		Doc{Summary: "Export users as CSV", Tags: []string{"Users"}})

	out := marshalSpec(t, NewSpec(Config{Title: "T", Version: "1"}, reg))
	get := out["paths"].(map[string]any)["/users.csv"].(map[string]any)["get"].(map[string]any)

	resp := get["responses"].(map[string]any)
	success := resp["200"].(map[string]any)
	content := success["content"].(map[string]any)
	if _, isJSON := content["application/json"]; isJSON {
		t.Fatalf("file 200 must not carry a JSON envelope; got %+v", content)
	}
	csv, ok := content["text/csv"].(map[string]any)
	if !ok {
		t.Fatalf("file 200 must carry text/csv content; got %+v", content)
	}
	schema := csv["schema"].(map[string]any)
	if schema["type"] != "string" || schema["format"] != "binary" {
		t.Fatalf("file 200 schema must be {type:string,format:binary}; got %+v", schema)
	}

	if _, ok := resp["422"]; !ok {
		t.Fatal("422 error response still expected on a file route")
	}
	if _, ok := resp["500"]; !ok {
		t.Fatal("500 error response still expected on a file route")
	}

	params, _ := get["parameters"].([]any)
	names := map[string]bool{}
	for _, p := range params {
		names[p.(map[string]any)["name"].(string)] = true
	}
	// `orderBy` has no `query:"…"` scalar to reflect — the sortable
	// declarations put it there, which is what makes the spec honest about a
	// control the route accepts.
	for _, want := range []string{"name", "name.in", "first", "orderBy"} {
		if !names[want] {
			t.Fatalf("expected query param %q reflected from RequestType; got %v", want, names)
		}
	}
}

// OmittedQueryParams removes the listed query keys from the rendered
// parameters even though RequestType declares them — the export wrappers use it
// to hide the pagination knobs they accept-but-ignore, while honored
// filters/controls stay.
func TestSpec_OmittedQueryParams_HidesListedKeys(t *testing.T) {
	type exportReq struct {
		Name      *string `query:"name" filter:"eq,in"`
		Search    *string `query:"search"`
		Fields    *string `query:"fields"`
		First     *int64  `query:"first"`
		After     *string `query:"after"`
		Before    *string `query:"before"`
		OnlyTotal *bool   `query:"onlyTotal"`
	}
	reg := NewRegistry()
	Mount(reg, fiber.New(), fiber.MethodGet, "/users.csv",
		func(c fiber.Ctx) error { return nil },
		RouteSpec{
			RequestType:        reflect.TypeOf(exportReq{}),
			SuccessStatus:      fiber.StatusOK,
			FileResponse:       &FileResponseSpec{ContentType: "text/csv"},
			OmittedQueryParams: []string{"limit", "after", "before", "onlyTotal"},
		},
		Doc{Summary: "Export", Tags: []string{"Users"}})

	out := marshalSpec(t, NewSpec(Config{Title: "T", Version: "1"}, reg))
	get := out["paths"].(map[string]any)["/users.csv"].(map[string]any)["get"].(map[string]any)
	params, _ := get["parameters"].([]any)
	names := map[string]bool{}
	for _, p := range params {
		names[p.(map[string]any)["name"].(string)] = true
	}
	// Honored knobs survive (filters + the export-honored control keys).
	for _, want := range []string{"name", "name.in", "search", "fields"} {
		if !names[want] {
			t.Fatalf("honored param %q must stay in the spec; got %v", want, names)
		}
	}
	// Ignored pagination knobs are stripped — Swagger never advertises them.
	for _, gone := range []string{"limit", "after", "before", "onlyTotal"} {
		if names[gone] {
			t.Fatalf("omitted param %q must NOT appear in the spec; got %v", gone, names)
		}
	}
}

func TestMount_FileResponseWithPaged_Panics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("FileResponse + Paged must panic at Mount")
		}
	}()
	// ResponseType non-nil so the Paged-needs-ResponseType guard does not fire first.
	Mount(NewRegistry(), fiber.New(), fiber.MethodGet, "/x",
		func(c fiber.Ctx) error { return nil },
		RouteSpec{
			SuccessStatus: 200,
			Paged:         true,
			ResponseType:  reflect.TypeOf(specListItem{}),
			FileResponse:  &FileResponseSpec{ContentType: "text/csv"},
		},
		Doc{})
}

func TestMount_FileResponseWithResponseType_Panics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("FileResponse + non-nil ResponseType must panic at Mount")
		}
	}()
	Mount(NewRegistry(), fiber.New(), fiber.MethodGet, "/x",
		func(c fiber.Ctx) error { return nil },
		RouteSpec{
			SuccessStatus: 200,
			ResponseType:  reflect.TypeOf(specListItem{}),
			FileResponse:  &FileResponseSpec{ContentType: "text/csv"},
		},
		Doc{})
}

func TestMount_FileResponseEmptyContentType_Panics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("FileResponse with empty ContentType must panic at Mount")
		}
	}()
	Mount(NewRegistry(), fiber.New(), fiber.MethodGet, "/x",
		func(c fiber.Ctx) error { return nil },
		RouteSpec{SuccessStatus: 200, FileResponse: &FileResponseSpec{}},
		Doc{})
}
