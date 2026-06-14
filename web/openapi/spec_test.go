package openapi

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/web/responses"
	"github.com/gofiber/fiber/v2"
)

// ─── Fixtures ──────────────────────────────────────────────────────────────

type specInsertRequest struct {
	Name  string  `json:"name"`
	Email string  `json:"email"`
	Phone *string `json:"phone,omitempty"`
}

type specInsertResponse struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

type specByEmailRequest struct {
	Email           string `path:"email"`
	IncludeArchived *bool  `query:"includeArchived"`
}

type specListRequest struct {
	Name  *string `query:"name" filter:"eq,in"`
	Limit *int64  `query:"limit"`
}

type specListItem struct {
	ID string `json:"id"`
}

// ─── Top-level document ───────────────────────────────────────────────────

func TestSpec_TopLevelStructure(t *testing.T) {
	reg := NewRegistry()
	spec := NewSpec(Config{
		Title:       "Test",
		Version:     "1.0.0",
		Description: "demo",
	}, reg)

	out := marshalSpec(t, spec)
	if out["openapi"] != "3.1.0" {
		t.Fatalf("openapi: got %v, want 3.1.0", out["openapi"])
	}
	info, ok := out["info"].(map[string]any)
	if !ok {
		t.Fatal("info missing")
	}
	if info["title"] != "Test" || info["version"] != "1.0.0" || info["description"] != "demo" {
		t.Fatalf("info: got %+v", info)
	}
}

func TestSpec_InfoBlockOptionalsOmittedWhenZero(t *testing.T) {
	reg := NewRegistry()
	spec := NewSpec(Config{Title: "T", Version: "1"}, reg)
	out := marshalSpec(t, spec)
	info := out["info"].(map[string]any)
	if _, exists := info["description"]; exists {
		t.Fatal("empty description must be omitted from info")
	}
	if _, exists := info["contact"]; exists {
		t.Fatal("nil contact must be omitted from info")
	}
	if _, exists := info["license"]; exists {
		t.Fatal("nil license must be omitted from info")
	}
}

func TestSpec_ServersBlockEmittedWhenSet(t *testing.T) {
	reg := NewRegistry()
	spec := NewSpec(Config{
		Title: "T", Version: "1",
		Servers: []Server{
			{URL: "https://api.example.com", Description: "production"},
			{URL: "https://staging.example.com"},
		},
	}, reg)
	out := marshalSpec(t, spec)
	servers, ok := out["servers"].([]any)
	if !ok || len(servers) != 2 {
		t.Fatalf("servers: got %+v", out["servers"])
	}
	first := servers[0].(map[string]any)
	if first["url"] != "https://api.example.com" || first["description"] != "production" {
		t.Fatalf("server[0]: got %+v", first)
	}
	second := servers[1].(map[string]any)
	if _, exists := second["description"]; exists {
		t.Fatal("server[1] empty description must be omitted")
	}
}

func TestSpec_ContactAndLicensePropagateWhenSet(t *testing.T) {
	reg := NewRegistry()
	spec := NewSpec(Config{
		Title: "T", Version: "1",
		Contact: &Contact{Name: "Maint", Email: "m@x"},
		License: &License{Name: "Apache-2.0", URL: "https://apache.org"},
	}, reg)
	out := marshalSpec(t, spec)
	info := out["info"].(map[string]any)
	contact := info["contact"].(map[string]any)
	if contact["name"] != "Maint" || contact["email"] != "m@x" {
		t.Fatalf("contact: got %+v", contact)
	}
	if _, exists := contact["url"]; exists {
		t.Fatal("empty URL must be omitted from contact")
	}
	license := info["license"].(map[string]any)
	if license["name"] != "Apache-2.0" || license["url"] != "https://apache.org" {
		t.Fatalf("license: got %+v", license)
	}
}

// ─── Canonical operation ─────────────────────────────────────────────────

func TestSpec_CanonicalCommandWithBody_RendersRequestBodyAndResponse(t *testing.T) {
	reg := NewRegistry()
	Mount(reg, fiber.New(), fiber.MethodPost, "/users",
		func(c *fiber.Ctx) error { return nil },
		RouteSpec{
			RequestType:   reflect.TypeOf(specInsertRequest{}),
			ResponseType:  reflect.TypeOf(specInsertResponse{}),
			SuccessStatus: fiber.StatusCreated,
			Strict:        false,
		},
		Doc{Summary: "Create a user", Tags: []string{"Users"}})

	out := marshalSpec(t, NewSpec(Config{Title: "T", Version: "1"}, reg))
	paths := out["paths"].(map[string]any)
	users, ok := paths["/users"].(map[string]any)
	if !ok {
		t.Fatalf("path /users missing; got %+v", paths)
	}
	post, ok := users["post"].(map[string]any)
	if !ok {
		t.Fatal("POST /users missing")
	}
	if post["summary"] != "Create a user" {
		t.Fatalf("summary: got %v", post["summary"])
	}
	tags, _ := post["tags"].([]any)
	if len(tags) != 1 || tags[0] != "Users" {
		t.Fatalf("tags: got %v", tags)
	}
	body, ok := post["requestBody"].(map[string]any)
	if !ok {
		t.Fatal("requestBody missing")
	}
	// Lenient handler → required: false on the body.
	if body["required"] != false {
		t.Fatalf("lenient body should have required:false; got %+v", body["required"])
	}
	resp := post["responses"].(map[string]any)
	if _, ok := resp["201"]; !ok {
		t.Fatalf("201 missing; got %+v", resp)
	}
	if _, ok := resp["422"]; !ok {
		t.Fatal("422 should be auto-added")
	}
	if _, ok := resp["500"]; !ok {
		t.Fatal("500 should be auto-added")
	}
	if _, ok := resp["400"]; !ok {
		t.Fatal("400 should be auto-added for body-carrying routes")
	}
}

func TestSpec_CanonicalStrictBody_MarksBodyRequiredTrue(t *testing.T) {
	reg := NewRegistry()
	Mount(reg, fiber.New(), fiber.MethodPut, "/users/:id",
		func(c *fiber.Ctx) error { return nil },
		RouteSpec{
			RequestType:   reflect.TypeOf(specInsertRequest{}),
			ResponseType:  reflect.TypeOf(specInsertResponse{}),
			SuccessStatus: fiber.StatusOK,
			Strict:        true,
			HasPathID:     true,
		},
		Doc{Summary: "Replace a user"})

	out := marshalSpec(t, NewSpec(Config{Title: "T", Version: "1"}, reg))
	paths := out["paths"].(map[string]any)
	// Fiber `:id` → OpenAPI `{id}`.
	op, ok := paths["/users/{id}"].(map[string]any)
	if !ok {
		t.Fatalf("/users/{id} missing; got %+v", paths)
	}
	put := op["put"].(map[string]any)
	body := put["requestBody"].(map[string]any)
	if body["required"] != true {
		t.Fatalf("strict body must mark required:true; got %+v", body["required"])
	}
	// Auto-added :id path param + 404.
	params := put["parameters"].([]any)
	if len(params) != 1 {
		t.Fatalf("expected 1 path param; got %+v", params)
	}
	first := params[0].(map[string]any)
	if first["name"] != "id" || first["in"] != "path" || first["required"] != true {
		t.Fatalf("path param: got %+v", first)
	}
	resp := put["responses"].(map[string]any)
	if _, ok := resp["404"]; !ok {
		t.Fatal("HasPathID route should auto-add 404")
	}
}

func TestSpec_BodylessNoneResponse_EnvelopeOmitsData(t *testing.T) {
	reg := NewRegistry()
	Mount(reg, fiber.New(), fiber.MethodDelete, "/users/:id",
		func(c *fiber.Ctx) error { return nil },
		RouteSpec{
			ResponseType:  reflect.TypeOf(responses.None{}),
			SuccessStatus: fiber.StatusNoContent,
			HasPathID:     true,
		},
		Doc{Summary: "Delete a user"})

	out := marshalSpec(t, NewSpec(Config{Title: "T", Version: "1"}, reg))
	paths := out["paths"].(map[string]any)
	op := paths["/users/{id}"].(map[string]any)
	del := op["delete"].(map[string]any)
	resp := del["responses"].(map[string]any)
	success := resp["204"].(map[string]any)
	content := success["content"].(map[string]any)
	json := content["application/json"].(map[string]any)
	envelope := json["schema"].(map[string]any)
	props := envelope["properties"].(map[string]any)
	if _, exists := props["data"]; exists {
		t.Fatalf("responses.None should omit data from the envelope; got %+v", props)
	}
}

// ─── Paged envelope (RouteSpec.Paged=true) ───────────────────────────────

// Paged routes must surface the same shape the runtime emits via
// fwweb.RespondPaged: data is an array of ResponseType items AND a
// top-level pagination property pointing to PaginationInfo. By-id
// routes keep data: <R> singular and no pagination property.
func TestSpec_PagedEnvelope_DataIsArrayPlusPaginationRef(t *testing.T) {
	reg := NewRegistry()
	Mount(reg, fiber.New(), fiber.MethodGet, "/users",
		func(c *fiber.Ctx) error { return nil },
		RouteSpec{
			RequestType:   reflect.TypeOf(specListRequest{}),
			ResponseType:  reflect.TypeOf(specListItem{}),
			SuccessStatus: fiber.StatusOK,
			Paged:         true,
		},
		Doc{Summary: "List users"})

	out := marshalSpec(t, NewSpec(Config{Title: "T", Version: "1"}, reg))
	paths := out["paths"].(map[string]any)
	get := paths["/users"].(map[string]any)["get"].(map[string]any)
	resp := get["responses"].(map[string]any)
	success := resp["200"].(map[string]any)
	envelope := success["content"].(map[string]any)["application/json"].(map[string]any)["schema"].(map[string]any)
	props := envelope["properties"].(map[string]any)

	data, ok := props["data"].(map[string]any)
	if !ok {
		t.Fatalf("paged envelope must carry data; got %+v", props)
	}
	if data["type"] != "array" {
		t.Fatalf("paged data must be array, got %+v", data)
	}
	if _, ok := data["items"].(map[string]any); !ok {
		t.Fatalf("paged data must declare items, got %+v", data)
	}
	pagination, ok := props["pagination"].(map[string]any)
	if !ok {
		t.Fatalf("paged envelope must carry pagination; got %+v", props)
	}
	if ref, _ := pagination["$ref"].(string); ref != "#/components/schemas/PaginationInfo" {
		t.Fatalf("pagination must $ref PaginationInfo, got %+v", pagination)
	}

	components := out["components"].(map[string]any)
	schemas := components["schemas"].(map[string]any)
	if _, ok := schemas["PaginationInfo"].(map[string]any); !ok {
		t.Fatal("PaginationInfo schema must land in components/schemas when a paged route exists")
	}
}

func TestSpec_NonPagedEnvelope_KeepsDataSingularAndOmitsPagination(t *testing.T) {
	reg := NewRegistry()
	Mount(reg, fiber.New(), fiber.MethodGet, "/users/:id",
		func(c *fiber.Ctx) error { return nil },
		RouteSpec{
			RequestType:   reflect.TypeOf(specByEmailRequest{}),
			ResponseType:  reflect.TypeOf(specInsertResponse{}),
			SuccessStatus: fiber.StatusOK,
			HasPathID:     true,
		},
		Doc{Summary: "Get a user"})

	out := marshalSpec(t, NewSpec(Config{Title: "T", Version: "1"}, reg))
	paths := out["paths"].(map[string]any)
	get := paths["/users/{id}"].(map[string]any)["get"].(map[string]any)
	resp := get["responses"].(map[string]any)
	success := resp["200"].(map[string]any)
	envelope := success["content"].(map[string]any)["application/json"].(map[string]any)["schema"].(map[string]any)
	props := envelope["properties"].(map[string]any)

	data, ok := props["data"]
	if !ok {
		t.Fatal("non-paged envelope must keep data")
	}
	if asMap, isMap := data.(map[string]any); isMap {
		if asMap["type"] == "array" {
			t.Fatal("non-paged data must NOT be an array")
		}
	}
	if _, exists := props["pagination"]; exists {
		t.Fatal("non-paged envelope must omit pagination")
	}
}

func TestMount_PagedWithNoneResponse_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("Paged:true paired with responses.None must panic at Mount")
		}
	}()
	Mount(nil, fiber.New(), fiber.MethodGet, "/x",
		func(c *fiber.Ctx) error { return nil },
		RouteSpec{
			ResponseType:  reflect.TypeOf(responses.None{}),
			SuccessStatus: fiber.StatusOK,
			Paged:         true,
		},
		Doc{Summary: "broken"})
}

func TestMount_PagedWithNilResponse_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("Paged:true paired with nil ResponseType must panic at Mount")
		}
	}()
	Mount(nil, fiber.New(), fiber.MethodGet, "/x",
		func(c *fiber.Ctx) error { return nil },
		RouteSpec{
			SuccessStatus: fiber.StatusOK,
			Paged:         true,
		},
		Doc{Summary: "broken"})
}

func TestRouteSpecOfPaged_SetsFlag(t *testing.T) {
	spec := RouteSpecOfPaged[specListRequest, specListItem](fiber.StatusOK)
	if !spec.Paged {
		t.Fatal("RouteSpecOfPaged must set Paged:true")
	}
	if spec.SuccessStatus != fiber.StatusOK {
		t.Fatalf("SuccessStatus: got %d", spec.SuccessStatus)
	}
	if spec.RequestType == nil || spec.RequestType.Name() != "specListRequest" {
		t.Fatalf("RequestType: got %+v", spec.RequestType)
	}
	if spec.ResponseType == nil || spec.ResponseType.Name() != "specListItem" {
		t.Fatalf("ResponseType: got %+v", spec.ResponseType)
	}
}

// ─── Query parameter expansion ───────────────────────────────────────────

func TestSpec_QueryFilterOperatorsExpandToParameters(t *testing.T) {
	reg := NewRegistry()
	Mount(reg, fiber.New(), fiber.MethodGet, "/users",
		func(c *fiber.Ctx) error { return nil },
		RouteSpec{
			RequestType:   reflect.TypeOf(specListRequest{}),
			ResponseType:  reflect.TypeOf(specListItem{}),
			SuccessStatus: fiber.StatusOK,
		},
		Doc{Summary: "List users"})

	out := marshalSpec(t, NewSpec(Config{Title: "T", Version: "1"}, reg))
	paths := out["paths"].(map[string]any)
	get := paths["/users"].(map[string]any)["get"].(map[string]any)
	params := get["parameters"].([]any)
	names := map[string]bool{}
	for _, p := range params {
		entry := p.(map[string]any)
		names[entry["name"].(string)] = true
	}
	// filter:"eq,in" on `name` → "name" + "name.in" (eq is the default,
	// no suffix). Reserved `limit` carries no filter tag → single entry.
	for _, expected := range []string{"name", "name.in", "limit"} {
		if !names[expected] {
			t.Fatalf("query parameter %q missing; got %v", expected, names)
		}
	}
}

func TestSpec_PathTagRequestEmitsPathParameter(t *testing.T) {
	reg := NewRegistry()
	Mount(reg, fiber.New(), fiber.MethodGet, "/users/:email",
		func(c *fiber.Ctx) error { return nil },
		RouteSpec{
			RequestType:   reflect.TypeOf(specByEmailRequest{}),
			ResponseType:  reflect.TypeOf(specInsertResponse{}),
			SuccessStatus: fiber.StatusOK,
		},
		Doc{Summary: "Get by email"})

	out := marshalSpec(t, NewSpec(Config{Title: "T", Version: "1"}, reg))
	paths := out["paths"].(map[string]any)
	get := paths["/users/{email}"].(map[string]any)["get"].(map[string]any)
	params := get["parameters"].([]any)

	var pathEmail, queryIncludeArchived map[string]any
	for _, p := range params {
		entry := p.(map[string]any)
		switch {
		case entry["in"] == "path" && entry["name"] == "email":
			pathEmail = entry
		case entry["in"] == "query" && entry["name"] == "includeArchived":
			queryIncludeArchived = entry
		}
	}
	if pathEmail == nil {
		t.Fatal("path:email parameter missing")
	}
	if pathEmail["required"] != true {
		t.Fatal("path parameter must always be required")
	}
	if queryIncludeArchived == nil {
		t.Fatal("query:includeArchived parameter missing")
	}
	if _, exists := queryIncludeArchived["required"]; exists {
		t.Fatalf("*bool query field should be optional; got %+v", queryIncludeArchived)
	}
}

// ─── Raw operations ──────────────────────────────────────────────────────

type whoamiSpecResponse struct {
	Subject       string `json:"subject"`
	Authenticated bool   `json:"authenticated"`
}

func TestSpec_RawOperation_RendersDeclaredResponses(t *testing.T) {
	reg := NewRegistry()
	MountRaw(reg, fiber.New(), fiber.MethodGet, "/whoami",
		func(c *fiber.Ctx) error { return nil },
		RawSpec{
			Summary: "Whoami",
			Tags:    []string{"Auth"},
			Responses: map[int]ResponseSpec{
				200: {Type: reflect.TypeOf(whoamiSpecResponse{})},
			},
		})
	out := marshalSpec(t, NewSpec(Config{Title: "T", Version: "1"}, reg))
	paths := out["paths"].(map[string]any)
	get := paths["/whoami"].(map[string]any)["get"].(map[string]any)
	if get["summary"] != "Whoami" {
		t.Fatalf("summary: got %v", get["summary"])
	}
	resp := get["responses"].(map[string]any)
	if _, ok := resp["200"]; !ok {
		t.Fatalf("200 missing; got %+v", resp)
	}
	if _, ok := resp["422"]; !ok {
		t.Fatal("422 must be auto-added even on raw ops")
	}
}

func TestSpec_RawRequestBody_RendersInline(t *testing.T) {
	reg := NewRegistry()
	type payload struct {
		Body string `json:"body"`
	}
	MountRaw(reg, fiber.New(), fiber.MethodPost, "/echo/signed",
		func(c *fiber.Ctx) error { return nil },
		RawSpec{
			Summary: "Signed echo",
			RequestBody: &RequestBody{
				Required: true,
				Type:     reflect.TypeOf(payload{}),
			},
		})
	out := marshalSpec(t, NewSpec(Config{Title: "T", Version: "1"}, reg))
	op := out["paths"].(map[string]any)["/echo/signed"].(map[string]any)["post"].(map[string]any)
	body := op["requestBody"].(map[string]any)
	if body["required"] != true {
		t.Fatalf("body required not propagated: %+v", body)
	}
	if _, ok := body["content"].(map[string]any)["application/json"]; !ok {
		t.Fatal("default content type should be application/json")
	}
	resp := op["responses"].(map[string]any)
	if _, ok := resp["400"]; !ok {
		t.Fatal("400 must be auto-added for routes with a request body")
	}
}

// ─── Hidden + cache ──────────────────────────────────────────────────────

func TestSpec_HiddenOperationsExcludedFromPaths(t *testing.T) {
	reg := NewRegistry()
	app := fiber.New()
	MountRaw(reg, app, fiber.MethodGet, "/echo/sse",
		func(c *fiber.Ctx) error { return nil },
		RawSpec{Hidden: true})
	Mount(reg, app, fiber.MethodGet, "/whoami",
		func(c *fiber.Ctx) error { return nil },
		RouteSpec{SuccessStatus: 200}, Doc{Hidden: true})
	Mount(reg, app, fiber.MethodGet, "/visible",
		func(c *fiber.Ctx) error { return nil },
		RouteSpec{SuccessStatus: 200}, Doc{Summary: "Visible"})

	out := marshalSpec(t, NewSpec(Config{Title: "T", Version: "1"}, reg))
	paths := out["paths"].(map[string]any)
	if _, exists := paths["/echo/sse"]; exists {
		t.Fatal("hidden raw operation must not appear in paths")
	}
	if _, exists := paths["/whoami"]; exists {
		t.Fatal("hidden canonical operation must not appear in paths")
	}
	if _, exists := paths["/visible"]; !exists {
		t.Fatal("visible operation should be in paths")
	}
}

func TestSpec_BuildIsCached(t *testing.T) {
	reg := NewRegistry()
	Mount(reg, fiber.New(), fiber.MethodGet, "/x",
		func(c *fiber.Ctx) error { return nil },
		RouteSpec{SuccessStatus: 200}, Doc{Summary: "X"})

	spec := NewSpec(Config{Title: "T", Version: "1"}, reg)
	first, err := spec.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	second, err := spec.Build()
	if err != nil {
		t.Fatalf("Build (2): %v", err)
	}
	// Same backing byte slice → identity check via len/pointer equality.
	if len(first) != len(second) {
		t.Fatal("cached Build should return the same payload")
	}
	if &first[0] != &second[0] {
		t.Fatal("cached Build should return the SAME byte slice (cache miss)")
	}
}

// ─── Components include ErrorEnvelope ─────────────────────────────────────

func TestSpec_ComponentsIncludesErrorEnvelope(t *testing.T) {
	reg := NewRegistry()
	Mount(reg, fiber.New(), fiber.MethodGet, "/x",
		func(c *fiber.Ctx) error { return nil },
		RouteSpec{SuccessStatus: 200}, Doc{Summary: "X"})

	out := marshalSpec(t, NewSpec(Config{Title: "T", Version: "1"}, reg))
	components, ok := out["components"].(map[string]any)
	if !ok {
		t.Fatal("components missing")
	}
	schemas := components["schemas"].(map[string]any)
	if _, ok := schemas["ErrorEnvelope"]; !ok {
		t.Fatalf("ErrorEnvelope schema missing from components; got %v", keysOfAny(schemas))
	}
}

// ─── Envelope examples (success + error) ─────────────────────────────────

func TestSpec_SuccessEnvelopePropertiesCarryExamples(t *testing.T) {
	reg := NewRegistry()
	Mount(reg, fiber.New(), fiber.MethodPost, "/users",
		func(c *fiber.Ctx) error { return nil },
		RouteSpec{
			RequestType:   reflect.TypeOf(specInsertRequest{}),
			ResponseType:  reflect.TypeOf(specInsertResponse{}),
			SuccessStatus: fiber.StatusCreated,
		},
		Doc{Summary: "Create a user"})

	out := marshalSpec(t, NewSpec(Config{Title: "T", Version: "1"}, reg))
	post := out["paths"].(map[string]any)["/users"].(map[string]any)["post"].(map[string]any)
	envelope := post["responses"].(map[string]any)["201"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["schema"].(map[string]any)
	props := envelope["properties"].(map[string]any)

	success := props["success"].(map[string]any)
	if success["example"] != true {
		t.Fatalf("success.example: got %v, want true", success["example"])
	}
	status := props["status"].(map[string]any)
	// JSON round-trip turns numeric examples into float64.
	if status["example"].(float64) != float64(fiber.StatusCreated) {
		t.Fatalf("status.example: got %v, want %d", status["example"], fiber.StatusCreated)
	}
	desc := props["description"].(map[string]any)
	if desc["example"] != "Created" {
		t.Fatalf("description.example: got %v, want Created", desc["example"])
	}
}

func TestSpec_SuccessEnvelopeDefaultsToStatusOKWhenZero(t *testing.T) {
	reg := NewRegistry()
	Mount(reg, fiber.New(), fiber.MethodGet, "/x",
		func(c *fiber.Ctx) error { return nil },
		RouteSpec{SuccessStatus: 0}, // unset → default 200
		Doc{Summary: "X"})

	out := marshalSpec(t, NewSpec(Config{Title: "T", Version: "1"}, reg))
	get := out["paths"].(map[string]any)["/x"].(map[string]any)["get"].(map[string]any)
	envelope := get["responses"].(map[string]any)["200"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["schema"].(map[string]any)
	props := envelope["properties"].(map[string]any)

	if props["status"].(map[string]any)["example"].(float64) != 200 {
		t.Fatalf("status.example with SuccessStatus=0 should default to 200; got %v", props["status"].(map[string]any)["example"])
	}
	if props["description"].(map[string]any)["example"] != "OK" {
		t.Fatalf("description.example with SuccessStatus=0 should default to OK; got %v", props["description"].(map[string]any)["example"])
	}
}

func TestSpec_ErrorResponsesCarryPerStatusContentExample(t *testing.T) {
	reg := NewRegistry()
	Mount(reg, fiber.New(), fiber.MethodPost, "/users",
		func(c *fiber.Ctx) error { return nil },
		RouteSpec{
			RequestType:   reflect.TypeOf(specInsertRequest{}),
			ResponseType:  reflect.TypeOf(specInsertResponse{}),
			SuccessStatus: fiber.StatusCreated,
		},
		Doc{Summary: "Create a user"})

	out := marshalSpec(t, NewSpec(Config{Title: "T", Version: "1"}, reg))
	post := out["paths"].(map[string]any)["/users"].(map[string]any)["post"].(map[string]any)
	responses := post["responses"].(map[string]any)

	cases := []struct {
		status   string
		wantCode float64
	}{
		{"400", 400},
		{"422", 422},
		{"500", 500},
	}
	for _, c := range cases {
		entry, ok := responses[c.status].(map[string]any)
		if !ok {
			t.Fatalf("response %s missing", c.status)
		}
		content := entry["content"].(map[string]any)["application/json"].(map[string]any)
		// Schema stays a shared $ref so the components pool is deduplicated.
		schema := content["schema"].(map[string]any)
		if schema["$ref"] != errorEnvelopeRef {
			t.Fatalf("response %s schema must $ref ErrorEnvelope; got %+v", c.status, schema)
		}
		// Content-level example overrides the schema-derived placeholder
		// (which is what Swagger UI shows otherwise: success=true, status=0).
		example, ok := content["example"].(map[string]any)
		if !ok {
			t.Fatalf("response %s missing content.example; got %+v", c.status, content)
		}
		if example["success"] != false {
			t.Fatalf("response %s example.success must be false; got %v", c.status, example["success"])
		}
		if example["status"].(float64) != c.wantCode {
			t.Fatalf("response %s example.status: got %v, want %v", c.status, example["status"], c.wantCode)
		}
		if _, hasErrors := example["errors"]; !hasErrors {
			t.Fatalf("response %s example missing errors block; got %+v", c.status, example)
		}
	}
}

// ─── fiberToOpenAPIPath ──────────────────────────────────────────────────

func TestFiberToOpenAPIPath(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"/users", "/users"},
		{"/users/:id", "/users/{id}"},
		{"/tenants/:tenantId/users/:id", "/tenants/{tenantId}/users/{id}"},
		{"/users/:id/archive", "/users/{id}/archive"},
	}
	for _, c := range cases {
		if got := fiberToOpenAPIPath(c.in); got != c.want {
			t.Errorf("fiberToOpenAPIPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ─── helpers ─────────────────────────────────────────────────────────────

func marshalSpec(t *testing.T, s *Spec) map[string]any {
	t.Helper()
	bytes, err := s.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(bytes, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

func keysOfAny(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
