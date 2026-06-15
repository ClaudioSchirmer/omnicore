package openapi

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
)

// ─── Public API ───────────────────────────────────────────────────────────

func TestDefaultErrorExample_KnownStatuses(t *testing.T) {
	for _, status := range []int{
		http.StatusBadRequest, http.StatusUnauthorized,
		http.StatusNotFound, http.StatusUnprocessableEntity,
		http.StatusInternalServerError,
	} {
		ex, ok := DefaultErrorExample(status)
		if !ok {
			t.Fatalf("status %d should have a framework default", status)
		}
		v, ok := ex.Value.(map[string]any)
		if !ok {
			t.Fatalf("status %d Value should be map[string]any, got %T", status, ex.Value)
		}
		if v["status"].(int) != status {
			t.Fatalf("status %d envelope carries wrong status field: %v", status, v["status"])
		}
		if v["success"] != false {
			t.Fatalf("status %d success should be false; got %v", status, v["success"])
		}
	}
}

func TestDefaultErrorExample_UnknownStatusReturnsFalse(t *testing.T) {
	_, ok := DefaultErrorExample(451)
	if ok {
		t.Fatal("non-standard status should return ok=false")
	}
}

func TestDefaultErrorExamples_ReturnsFreshCopy(t *testing.T) {
	a := DefaultErrorExamples()
	b := DefaultErrorExamples()
	// Mutating one snapshot must not affect a subsequent snapshot.
	delete(a, http.StatusUnprocessableEntity)
	if _, ok := b[http.StatusUnprocessableEntity]; !ok {
		t.Fatal("DefaultErrorExamples should return a fresh copy on every call")
	}
}

// ─── Fixtures ────────────────────────────────────────────────────────────

type insertReq struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

type insertResp struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

// ─── Request examples ────────────────────────────────────────────────────

func TestSpec_RequestExamples_RenderAsPluralMap(t *testing.T) {
	reg := NewRegistry()
	Mount(reg, fiber.New(), fiber.MethodPost, "/users",
		func(c fiber.Ctx) error { return nil },
		RouteSpec{
			RequestType:   reflect.TypeOf(insertReq{}),
			ResponseType:  reflect.TypeOf(insertResp{}),
			SuccessStatus: fiber.StatusCreated,
		},
		Doc{
			Summary: "Create",
			RequestExamples: map[string]Example{
				"minimal": {
					Summary: "Minimal valid payload",
					Value:   insertReq{Name: "Alice", Email: "alice@example.com"},
				},
				"withMiddle": {
					Summary: "With another payload",
					Value:   insertReq{Name: "Bob", Email: "bob@example.com"},
				},
			},
		})

	out := marshalSpec(t, NewSpec(Config{Title: "T", Version: "1"}, reg))
	post := out["paths"].(map[string]any)["/users"].(map[string]any)["post"].(map[string]any)
	body := post["requestBody"].(map[string]any)
	content := body["content"].(map[string]any)["application/json"].(map[string]any)
	examples, ok := content["examples"].(map[string]any)
	if !ok {
		t.Fatalf("examples missing on requestBody; got %+v", content)
	}
	if _, exists := examples["minimal"]; !exists {
		t.Fatalf("minimal example missing; got %v", keysOfAny(examples))
	}
	if _, exists := examples["withMiddle"]; !exists {
		t.Fatalf("withMiddle example missing; got %v", keysOfAny(examples))
	}
	minimal := examples["minimal"].(map[string]any)
	if minimal["summary"] != "Minimal valid payload" {
		t.Fatalf("summary not preserved; got %v", minimal["summary"])
	}
	value := minimal["value"].(map[string]any)
	if value["name"] != "Alice" || value["email"] != "alice@example.com" {
		t.Fatalf("value not preserved verbatim; got %+v", value)
	}
}

func TestSpec_RequestExamples_RawMessagePassesThrough(t *testing.T) {
	reg := NewRegistry()
	Mount(reg, fiber.New(), fiber.MethodPost, "/users",
		func(c fiber.Ctx) error { return nil },
		RouteSpec{
			RequestType:   reflect.TypeOf(insertReq{}),
			ResponseType:  reflect.TypeOf(insertResp{}),
			SuccessStatus: fiber.StatusCreated,
		},
		Doc{
			RequestExamples: map[string]Example{
				"hand": {
					Value: json.RawMessage(`{"name":"Alice","email":"a@x"}`),
				},
			},
		})

	out := marshalSpec(t, NewSpec(Config{Title: "T", Version: "1"}, reg))
	post := out["paths"].(map[string]any)["/users"].(map[string]any)["post"].(map[string]any)
	body := post["requestBody"].(map[string]any)
	content := body["content"].(map[string]any)["application/json"].(map[string]any)
	examples := content["examples"].(map[string]any)
	hand := examples["hand"].(map[string]any)
	value := hand["value"].(map[string]any)
	if value["name"] != "Alice" {
		t.Fatalf("json.RawMessage should round-trip verbatim; got %v", value)
	}
}

func TestSpec_RequestExamples_RejectsTypoAtBoot(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic when example carries an unknown field")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, "requestBody") {
			t.Fatalf("panic should mention the slot, got: %s", msg)
		}
	}()
	reg := NewRegistry()
	type bad struct {
		Naem string `json:"naem"` // typo — does not match insertReq.Name
	}
	Mount(reg, fiber.New(), fiber.MethodPost, "/users",
		func(c fiber.Ctx) error { return nil },
		RouteSpec{
			RequestType:   reflect.TypeOf(insertReq{}),
			ResponseType:  reflect.TypeOf(insertResp{}),
			SuccessStatus: fiber.StatusCreated,
		},
		Doc{
			RequestExamples: map[string]Example{
				"broken": {Value: bad{Naem: "x"}},
			},
		})
}

// ─── Success response examples — wrap automático ─────────────────────────

func TestSpec_SuccessExamples_WrapsValueInEnvelope(t *testing.T) {
	reg := NewRegistry()
	Mount(reg, fiber.New(), fiber.MethodPost, "/users",
		func(c fiber.Ctx) error { return nil },
		RouteSpec{
			RequestType:   reflect.TypeOf(insertReq{}),
			ResponseType:  reflect.TypeOf(insertResp{}),
			SuccessStatus: fiber.StatusCreated,
		},
		Doc{
			ResponseExamples: map[int]map[string]Example{
				fiber.StatusCreated: {
					"happyPath": {
						Summary: "User successfully created",
						Value:   insertResp{ID: "u_abc", Name: "Alice", Email: "alice@x"},
					},
				},
			},
		})

	out := marshalSpec(t, NewSpec(Config{Title: "T", Version: "1"}, reg))
	post := out["paths"].(map[string]any)["/users"].(map[string]any)["post"].(map[string]any)
	resp := post["responses"].(map[string]any)["201"].(map[string]any)
	content := resp["content"].(map[string]any)["application/json"].(map[string]any)
	examples, ok := content["examples"].(map[string]any)
	if !ok {
		t.Fatalf("success status should expose examples; got %+v", content)
	}
	happy := examples["happyPath"].(map[string]any)
	value := happy["value"].(map[string]any)
	if value["success"] != true {
		t.Fatalf("success-wrap should set success:true; got %v", value["success"])
	}
	// JSON round-trip turns numeric examples into float64.
	if value["status"].(float64) != float64(fiber.StatusCreated) {
		t.Fatalf("success-wrap should set status:201; got %v", value["status"])
	}
	if value["description"] != "Created" {
		t.Fatalf("success-wrap should set description:Created; got %v", value["description"])
	}
	data := value["data"].(map[string]any)
	if data["id"] != "u_abc" || data["name"] != "Alice" {
		t.Fatalf("data should carry the consumer's inner shape; got %+v", data)
	}
}

func TestSpec_SuccessExamples_RejectsBadShapeAtBoot(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic when success example carries unknown field")
		}
	}()
	reg := NewRegistry()
	type bad struct {
		Whatever string `json:"whatever"` // not on insertResp
	}
	Mount(reg, fiber.New(), fiber.MethodPost, "/users",
		func(c fiber.Ctx) error { return nil },
		RouteSpec{
			RequestType:   reflect.TypeOf(insertReq{}),
			ResponseType:  reflect.TypeOf(insertResp{}),
			SuccessStatus: fiber.StatusCreated,
		},
		Doc{
			ResponseExamples: map[int]map[string]Example{
				fiber.StatusCreated: {
					"bad": {Value: bad{Whatever: "x"}},
				},
			},
		})
}

// ─── Error response examples — auto-merge default ────────────────────────

func TestSpec_ErrorExamples_AutoMergesDefault(t *testing.T) {
	reg := NewRegistry()
	Mount(reg, fiber.New(), fiber.MethodPost, "/users",
		func(c fiber.Ctx) error { return nil },
		RouteSpec{
			RequestType:   reflect.TypeOf(insertReq{}),
			ResponseType:  reflect.TypeOf(insertResp{}),
			SuccessStatus: fiber.StatusCreated,
		},
		Doc{
			ResponseExamples: map[int]map[string]Example{
				fiber.StatusUnprocessableEntity: {
					"duplicateEmail": {
						Summary: "Email already in use",
						Value:   errorEnvelopeValue(422, "User", "EmailAlreadyExistsNotification", "email", "", "Conflict"),
					},
				},
			},
		})

	out := marshalSpec(t, NewSpec(Config{Title: "T", Version: "1"}, reg))
	post := out["paths"].(map[string]any)["/users"].(map[string]any)["post"].(map[string]any)
	resp := post["responses"].(map[string]any)["422"].(map[string]any)
	content := resp["content"].(map[string]any)["application/json"].(map[string]any)
	examples, ok := content["examples"].(map[string]any)
	if !ok {
		t.Fatalf("error status should expose examples plural; got %+v", content)
	}
	if _, exists := examples["default"]; !exists {
		t.Fatalf("framework default should auto-merge under \"default\"; got %v", keysOfAny(examples))
	}
	if _, exists := examples["duplicateEmail"]; !exists {
		t.Fatalf("consumer example should appear alongside default; got %v", keysOfAny(examples))
	}
	// Singular `example` must not coexist with plural `examples` (OpenAPI 3.x rule).
	if _, exists := content["example"]; exists {
		t.Fatal("singular example must not coexist with plural examples")
	}
}

func TestSpec_ErrorExamples_ExplicitDefaultOverridesFramework(t *testing.T) {
	reg := NewRegistry()
	customDefault := errorEnvelopeValue(422, "User", "OverriddenNotification", "x", "", "Validation")
	Mount(reg, fiber.New(), fiber.MethodPost, "/users",
		func(c fiber.Ctx) error { return nil },
		RouteSpec{
			RequestType:   reflect.TypeOf(insertReq{}),
			ResponseType:  reflect.TypeOf(insertResp{}),
			SuccessStatus: fiber.StatusCreated,
		},
		Doc{
			ResponseExamples: map[int]map[string]Example{
				fiber.StatusUnprocessableEntity: {
					"default": {Summary: "Custom default", Value: customDefault},
				},
			},
		})

	out := marshalSpec(t, NewSpec(Config{Title: "T", Version: "1"}, reg))
	post := out["paths"].(map[string]any)["/users"].(map[string]any)["post"].(map[string]any)
	resp := post["responses"].(map[string]any)["422"].(map[string]any)
	content := resp["content"].(map[string]any)["application/json"].(map[string]any)
	examples := content["examples"].(map[string]any)
	def := examples["default"].(map[string]any)
	if def["summary"] != "Custom default" {
		t.Fatalf("explicit default should win over framework's; got %v", def["summary"])
	}
	val := def["value"].(map[string]any)
	errs := val["errors"].([]any)[0].(map[string]any)
	msgs := errs["messages"].([]any)[0].(map[string]any)
	if msgs["notificationKey"] != "OverriddenNotification" {
		t.Fatalf("explicit override should carry consumer's notification; got %v", msgs["notificationKey"])
	}
}

func TestSpec_ErrorExamples_EmptyDefaultRemovesEntry(t *testing.T) {
	reg := NewRegistry()
	Mount(reg, fiber.New(), fiber.MethodPost, "/users",
		func(c fiber.Ctx) error { return nil },
		RouteSpec{
			RequestType:   reflect.TypeOf(insertReq{}),
			ResponseType:  reflect.TypeOf(insertResp{}),
			SuccessStatus: fiber.StatusCreated,
		},
		Doc{
			ResponseExamples: map[int]map[string]Example{
				fiber.StatusUnprocessableEntity: {
					"default":  {}, // empty Value = remove the canonical default
					"variant": {Value: errorEnvelopeValue(422, "X", "Y", "f", "", "Validation")},
				},
			},
		})

	out := marshalSpec(t, NewSpec(Config{Title: "T", Version: "1"}, reg))
	post := out["paths"].(map[string]any)["/users"].(map[string]any)["post"].(map[string]any)
	resp := post["responses"].(map[string]any)["422"].(map[string]any)
	content := resp["content"].(map[string]any)["application/json"].(map[string]any)
	examples := content["examples"].(map[string]any)
	if _, exists := examples["default"]; exists {
		t.Fatalf("empty default should remove the canonical entry; got %v", keysOfAny(examples))
	}
	if _, exists := examples["variant"]; !exists {
		t.Fatalf("consumer variant should still render; got %v", keysOfAny(examples))
	}
}

func TestSpec_ErrorExamples_CustomStatusRendersConsumerExamples(t *testing.T) {
	// A canonical route may emit a status outside the framework's
	// default error set (400/401/403/404/422/500) — typically when a
	// domain notification overrides Semantic() to Conflict/Unavailable/
	// etc. Declaring Doc.ResponseExamples[N] for that status must surface
	// a responses["N"] entry carrying the consumer's examples, not be
	// silently dropped.
	reg := NewRegistry()
	conflictEnvelope := errorEnvelopeValue(409, "User", "EmailAlreadyExistsNotification", "email", "alice@x", "Conflict")
	Mount(reg, fiber.New(), fiber.MethodPost, "/users",
		func(c fiber.Ctx) error { return nil },
		RouteSpec{
			RequestType:   reflect.TypeOf(insertReq{}),
			ResponseType:  reflect.TypeOf(insertResp{}),
			SuccessStatus: fiber.StatusCreated,
		},
		Doc{
			ResponseExamples: map[int]map[string]Example{
				fiber.StatusConflict: {
					"duplicateEmail": {
						Summary: "Email already registered",
						Value:   conflictEnvelope,
					},
				},
			},
		})

	out := marshalSpec(t, NewSpec(Config{Title: "T", Version: "1"}, reg))
	post := out["paths"].(map[string]any)["/users"].(map[string]any)["post"].(map[string]any)
	responses := post["responses"].(map[string]any)
	resp, exists := responses["409"].(map[string]any)
	if !exists {
		t.Fatalf("custom status declared via ResponseExamples should render; got statuses %v", keysOfAny(responses))
	}
	if resp["description"] != "Conflict" {
		t.Fatalf("custom status description should come from http.StatusText; got %v", resp["description"])
	}
	content := resp["content"].(map[string]any)["application/json"].(map[string]any)
	schema, ok := content["schema"].(map[string]any)
	if !ok {
		t.Fatalf("custom status should reuse the ErrorEnvelope schema; got %+v", content)
	}
	if ref, _ := schema["$ref"].(string); ref != errorEnvelopeRef {
		t.Fatalf("custom status schema should $ref ErrorEnvelope; got %v", schema)
	}
	examples, ok := content["examples"].(map[string]any)
	if !ok {
		t.Fatalf("consumer examples should render under plural examples; got %+v", content)
	}
	if _, exists := examples["duplicateEmail"]; !exists {
		t.Fatalf("consumer-declared example should render; got %v", keysOfAny(examples))
	}
	// 409 has no DefaultErrorExample entry → no auto-merged `default`.
	if _, exists := examples["default"]; exists {
		t.Fatalf("non-default status should NOT auto-merge a default entry; got %v", keysOfAny(examples))
	}
	// Singular `example` must not coexist with plural `examples`.
	if _, exists := content["example"]; exists {
		t.Fatal("singular example must not coexist with plural examples")
	}
}

func TestSpec_ErrorExamples_NoDeclarationKeepsSingular(t *testing.T) {
	// Backwards-compat: when consumer declares NOTHING for a status, the
	// renderer keeps the pre-Phase-2 singular `example` shape.
	reg := NewRegistry()
	Mount(reg, fiber.New(), fiber.MethodPost, "/users",
		func(c fiber.Ctx) error { return nil },
		RouteSpec{
			RequestType:   reflect.TypeOf(insertReq{}),
			ResponseType:  reflect.TypeOf(insertResp{}),
			SuccessStatus: fiber.StatusCreated,
		},
		Doc{Summary: "Create"})

	out := marshalSpec(t, NewSpec(Config{Title: "T", Version: "1"}, reg))
	post := out["paths"].(map[string]any)["/users"].(map[string]any)["post"].(map[string]any)
	resp := post["responses"].(map[string]any)["422"].(map[string]any)
	content := resp["content"].(map[string]any)["application/json"].(map[string]any)
	if _, exists := content["examples"]; exists {
		t.Fatalf("examples plural should NOT appear when consumer declared nothing; got %v", content["examples"])
	}
	if _, exists := content["example"]; !exists {
		t.Fatalf("example singular should be preserved as the backwards-compat default; got %+v", content)
	}
}

// ─── RawSpec examples ────────────────────────────────────────────────────

type rawWhoami struct {
	Subject string `json:"subject"`
}

func TestSpec_RawSpec_RequestBodyExamples(t *testing.T) {
	reg := NewRegistry()
	type echoPayload struct {
		Body string `json:"body"`
	}
	MountRaw(reg, fiber.New(), fiber.MethodPost, "/echo/signed",
		func(c fiber.Ctx) error { return nil },
		RawSpec{
			Summary: "Signed echo",
			RequestBody: &RequestBody{
				Required: true,
				Type:     reflect.TypeOf(echoPayload{}),
				Examples: map[string]Example{
					"sample": {Summary: "Sample body", Value: echoPayload{Body: "hello"}},
				},
			},
		})

	out := marshalSpec(t, NewSpec(Config{Title: "T", Version: "1"}, reg))
	op := out["paths"].(map[string]any)["/echo/signed"].(map[string]any)["post"].(map[string]any)
	body := op["requestBody"].(map[string]any)
	content := body["content"].(map[string]any)["application/json"].(map[string]any)
	examples, ok := content["examples"].(map[string]any)
	if !ok {
		t.Fatalf("raw request body should carry examples; got %+v", content)
	}
	sample := examples["sample"].(map[string]any)
	if sample["summary"] != "Sample body" {
		t.Fatalf("raw request example summary lost; got %v", sample["summary"])
	}
}

func TestSpec_RawSpec_ResponseExamplesRenderRaw(t *testing.T) {
	// Raw responses do NOT wrap — consumer-declared payload renders
	// verbatim, no envelope injection. Symmetric to RequestBody behavior.
	reg := NewRegistry()
	MountRaw(reg, fiber.New(), fiber.MethodGet, "/whoami",
		func(c fiber.Ctx) error { return nil },
		RawSpec{
			Summary: "Whoami",
			Responses: map[int]ResponseSpec{
				200: {
					Type: reflect.TypeOf(rawWhoami{}),
					Examples: map[string]Example{
						"sample": {Value: rawWhoami{Subject: "user-42"}},
					},
				},
			},
		})

	out := marshalSpec(t, NewSpec(Config{Title: "T", Version: "1"}, reg))
	op := out["paths"].(map[string]any)["/whoami"].(map[string]any)["get"].(map[string]any)
	resp := op["responses"].(map[string]any)["200"].(map[string]any)
	content := resp["content"].(map[string]any)["application/json"].(map[string]any)
	examples := content["examples"].(map[string]any)
	sample := examples["sample"].(map[string]any)
	val := sample["value"].(map[string]any)
	if val["subject"] != "user-42" {
		t.Fatalf("raw response example should render verbatim (no wrap); got %+v", val)
	}
	// No `success` / `status` / `description` injected — raw stays raw.
	if _, exists := val["success"]; exists {
		t.Fatal("raw response example must NOT be wrapped in the canonical envelope")
	}
}
