package openapi

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/gofiber/fiber/v3"
)

// covEmbedBody is an anonymous *struct embed carrying a body field — drives
// hasBodyFields' anonymous-pointer deref + recurse branch.
type covEmbedBody struct {
	Note string `json:"note"`
}

// covCanonReq exercises the parameter walkers:
//   - *struct embed with a body field  → hasBodyFields anonymous deref
//   - path field with a description tag → walkPathTags description branch
//   - unexported path/query fields      → IsExported guards
//   - filter:"eq,,in" with an empty op  → splitOps drops the empty token
type covCanonReq struct {
	*covEmbedBody
	ID     string  `path:"id" description:"the resource id"`
	secret string  `path:"secret"` //nolint:unused // reflection-only: skipped (unexported)
	hidden string  `query:"hidden"` //nolint:unused // reflection-only: skipped (unexported)
	Status *string `query:"status" filter:"eq,,in"`
	Limit  *int64  `query:"limit"`
}

type covResp struct {
	ID string `json:"id"`
}

// TestSpecBuild_CanonicalParameterAndOperationBranches mounts a canonical route
// whose pointer RequestType drives the parameter-walk and operation-build
// optional branches (OperationID, Deprecated, path description, unexported
// fields, empty filter op, anonymous *struct embed), then asserts the document
// assembles cleanly.
func TestSpecBuild_CanonicalParameterAndOperationBranches(t *testing.T) {
	reg := NewRegistry()
	app := fiber.New()
	Mount(reg, app, fiber.MethodPost, "/things/:id/:secret",
		func(c fiber.Ctx) error { return c.SendStatus(200) },
		RouteSpec{
			RequestType:   reflect.TypeOf((*covCanonReq)(nil)), // pointer → canonicalParameters deref
			ResponseType:  reflect.TypeOf(covResp{}),
			SuccessStatus: 200,
		},
		Doc{
			Summary:     "Create thing",
			OperationID: "createThing",
			Deprecated:  true,
			Tags:        []string{"Things"},
		})

	cfg := Config{
		Title:   "API",
		Version: "1.0.0",
		Contact: &Contact{Name: "Team", Email: "t@x.com", URL: "https://x.com"},
		License: &License{Name: "MIT", URL: "https://opensource.org/MIT"},
	}
	raw, err := NewSpec(cfg, reg).Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("spec is not valid JSON: %v", err)
	}
	info := doc["info"].(map[string]any)
	contact := info["contact"].(map[string]any)
	if contact["url"] != "https://x.com" {
		t.Errorf("expected contact.url rendered, got %v", contact["url"])
	}
	op := doc["paths"].(map[string]any)["/things/{id}/{secret}"].(map[string]any)["post"].(map[string]any)
	if op["operationId"] != "createThing" {
		t.Errorf("expected operationId rendered, got %v", op["operationId"])
	}
	if op["deprecated"] != true {
		t.Errorf("expected deprecated rendered, got %v", op["deprecated"])
	}
}

// TestSpecBuild_CanonicalResponseExamplesDefaultStatus drives mount.go's
// successStatus==0 fallback (SuccessStatus omitted) plus buildErrorExamplesMap's
// declared-example and injected-default Description branches.
func TestSpecBuild_CanonicalResponseExamplesDefaultStatus(t *testing.T) {
	reg := NewRegistry()
	app := fiber.New()
	Mount(reg, app, fiber.MethodGet, "/widgets",
		func(c fiber.Ctx) error { return c.SendStatus(200) },
		RouteSpec{
			ResponseType: reflect.TypeOf(covResp{}),
			// SuccessStatus omitted (0) → validateCanonicalResponseExamples
			// defaults it to 200.
		},
		Doc{
			Summary: "List widgets",
			ResponseExamples: map[int]map[string]Example{
				422: {
					"custom": {
						Summary:     "A custom 422",
						Description: "consumer-declared example",
						Value:       map[string]any{"errors": []any{}},
					},
				},
			},
		})

	raw, err := NewSpec(Config{Title: "API", Version: "1.0.0"}, reg).Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("spec is not valid JSON: %v", err)
	}
	resps := doc["paths"].(map[string]any)["/widgets"].(map[string]any)["get"].(map[string]any)["responses"].(map[string]any)
	if _, ok := resps["422"]; !ok {
		t.Errorf("expected a 422 response entry, got %v", resps)
	}
}

// isPublic returns true when no AuthContext is configured (auth nil), so every
// operation is public by default.
func TestIsPublic_NilAuthContext(t *testing.T) {
	s := NewSpec(Config{Title: "API"}, NewRegistry()) // auth stays nil
	if !s.isPublic(Operation{Method: "GET", Path: "/x", Doc: Doc{}}) {
		t.Error("isPublic must return true when no AuthContext is configured")
	}
}

// buildErrorExamplesMap returns nil when there is neither a framework default
// for the status nor any declared example (out stays empty). Status 200 has no
// DefaultErrorExample.
func TestBuildErrorExamplesMap_EmptyReturnsNil(t *testing.T) {
	if got := buildErrorExamplesMap(nil, 200); got != nil {
		t.Errorf("expected nil for an empty examples map, got %v", got)
	}
}

// walkQueryTags derefs a pointer type before walking (queryschema.WalkRequest
// handles the top-level pointer transparently).
func TestWalkQueryTags_PointerTypeDeref(t *testing.T) {
	gen := NewGenerator(nil)
	out := walkQueryTags(reflect.PointerTo(reflect.TypeOf(covCanonReq{})), gen)
	var sawStatus bool
	for _, p := range out {
		if p["name"] == "status" {
			sawStatus = true
		}
	}
	if !sawStatus {
		t.Errorf("expected the status query param after deref, got %v", out)
	}
}

// hasBodyFields must skip an anonymous embed that contributes no body field
// (all its fields are path-tagged) and keep scanning the outer fields.
type covEmbedNoBody struct {
	ID string `path:"id"`
}

type covReqEmptyEmbed struct {
	covEmbedNoBody
	Name string `json:"name"`
}

func TestHasBodyFields_AnonymousEmbedWithoutBodyFields(t *testing.T) {
	if !hasBodyFields(reflect.TypeOf(covReqEmptyEmbed{})) {
		t.Error("outer body field must be detected past a body-less anonymous embed")
	}
}

type covRawReqBody struct {
	Field string `json:"field"`
}

// TestSpecBuild_RawOperationBranches drives the raw-operation optional branches:
// OperationID, Deprecated, Parameters, RequestBody.Description, a declared
// standard-error status (rawResponses skip), and a body-less response entry.
func TestSpecBuild_RawOperationBranches(t *testing.T) {
	reg := NewRegistry()
	app := fiber.New()
	MountRaw(reg, app, fiber.MethodPost, "/raw",
		func(c fiber.Ctx) error { return c.SendStatus(200) },
		RawSpec{
			Summary:     "Raw route",
			OperationID: "rawRoute",
			Deprecated:  true,
			Tags:        []string{"Raw"},
			Parameters: []Parameter{
				PathParam("tenant", "tenant id"),
				QueryParam("q", "free text", reflect.TypeOf("")),
			},
			RequestBody: &RequestBody{
				Description: "the raw body",
				Required:    true,
				Type:        reflect.TypeOf(covRawReqBody{}),
			},
			Responses: map[int]ResponseSpec{
				200: {Description: "ok", Type: reflect.TypeOf(covResp{})},
				204: {Description: "no content"},     // Type nil → rawResponseEntry early return
				422: {Description: "custom validation"}, // declared standard error → rawResponses skip
			},
		})

	raw, err := NewSpec(Config{Title: "API", Version: "1.0.0"}, reg).Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("spec is not valid JSON: %v", err)
	}
	op := doc["paths"].(map[string]any)["/raw"].(map[string]any)["post"].(map[string]any)
	if op["operationId"] != "rawRoute" {
		t.Errorf("expected operationId rendered, got %v", op["operationId"])
	}
	if op["deprecated"] != true {
		t.Errorf("expected deprecated rendered, got %v", op["deprecated"])
	}
	rb := op["requestBody"].(map[string]any)
	if rb["description"] != "the raw body" {
		t.Errorf("expected requestBody.description rendered, got %v", rb["description"])
	}
	resps := op["responses"].(map[string]any)
	if got := resps["422"].(map[string]any)["description"]; got != "custom validation" {
		t.Errorf("declared 422 must win over the standard error, got %v", got)
	}
	if _, ok := resps["204"]; !ok {
		t.Errorf("expected the body-less 204 response entry, got %v", resps)
	}
}
