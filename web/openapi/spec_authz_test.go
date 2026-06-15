package openapi

import (
	"reflect"
	"testing"

	"github.com/gofiber/fiber/v3"
)

type authzSampleResponse struct {
	ID string `json:"id"`
}

// authzRegistry mounts three routes that together cover the rendering
// matrix of Phase 3:
//
//   - /protected/gated   — RequirePermission("users:write") declared
//   - /protected/loose   — protected (not public) but no RequirePermission
//   - /public            — Doc.Public = true; no auth, no 403
func authzRegistry(t *testing.T) *Registry {
	t.Helper()
	resetGate(t)
	SetGate(noopGate)
	reg := NewRegistry()
	app := fiber.New()

	Mount(reg, app, fiber.MethodPost, "/protected/gated",
		func(c fiber.Ctx) error { return nil },
		RouteSpec{ResponseType: reflect.TypeOf(authzSampleResponse{}), SuccessStatus: 201},
		Doc{Summary: "Gated route", Description: "Already-existing prose."},
		RequirePermission("users:write"))

	Mount(reg, app, fiber.MethodGet, "/protected/loose",
		func(c fiber.Ctx) error { return nil },
		RouteSpec{ResponseType: reflect.TypeOf(authzSampleResponse{}), SuccessStatus: 200},
		Doc{Summary: "Layer-2-only route"})

	Mount(reg, app, fiber.MethodGet, "/public",
		func(c fiber.Ctx) error { return nil },
		RouteSpec{ResponseType: reflect.TypeOf(authzSampleResponse{}), SuccessStatus: 200},
		Doc{Summary: "Public", Public: true})

	return reg
}

func TestSpec_DescriptionSuffix_WhenRequiredPermissionSet(t *testing.T) {
	reg := authzRegistry(t)
	spec := NewSpec(Config{Title: "T", Version: "1"}, reg)
	spec.auth = &AuthContext{AuthorizationEnabled: true}
	out := marshalSpec(t, spec)

	op := out["paths"].(map[string]any)["/protected/gated"].(map[string]any)["post"].(map[string]any)
	desc, ok := op["description"].(string)
	if !ok {
		t.Fatal("expected description on gated route, got none")
	}
	wantSuffix := "**Required permission:** `users:write`"
	if !contains(desc, wantSuffix) {
		t.Errorf("description missing suffix: got %q, want suffix %q", desc, wantSuffix)
	}
	if !contains(desc, "Already-existing prose.") {
		t.Errorf("base description lost: got %q", desc)
	}
}

func TestSpec_DescriptionSuffix_OmittedWhenNoRequiredPermission(t *testing.T) {
	reg := authzRegistry(t)
	spec := NewSpec(Config{Title: "T", Version: "1"}, reg)
	spec.auth = &AuthContext{AuthorizationEnabled: true}
	out := marshalSpec(t, spec)

	op := out["paths"].(map[string]any)["/protected/loose"].(map[string]any)["get"].(map[string]any)
	if d, has := op["description"]; has {
		if s, _ := d.(string); contains(s, "Required permission") {
			t.Errorf("loose route should NOT carry permission suffix, got %q", s)
		}
	}
}

// Gate-disabled cases — the suffix must NOT appear when the runtime gate
// is not enforcing. Otherwise the spec advertises a constraint the server
// does not honor, which is the bug the gating was introduced to fix.

func TestSpec_DescriptionSuffix_OmittedWhenAuthDisabled(t *testing.T) {
	reg := authzRegistry(t)
	// No WithAuth → spec.auth == nil → auth.mode=disabled equivalent.
	spec := NewSpec(Config{Title: "T", Version: "1"}, reg)
	out := marshalSpec(t, spec)

	op := out["paths"].(map[string]any)["/protected/gated"].(map[string]any)["post"].(map[string]any)
	if d, has := op["description"]; has {
		if s, _ := d.(string); contains(s, "Required permission") {
			t.Errorf("auth.mode=disabled: gated route must NOT carry permission suffix, got %q", s)
		}
	}
	// Base prose must survive — the gating suppresses the suffix only.
	desc, _ := op["description"].(string)
	if !contains(desc, "Already-existing prose.") {
		t.Errorf("base description lost when suffix suppressed: got %q", desc)
	}
}

func TestSpec_DescriptionSuffix_OmittedWhenAuthzDisabled(t *testing.T) {
	reg := authzRegistry(t)
	// JWT mode but authorization.enabled=false → runtime gate no-ops.
	spec := NewSpec(Config{Title: "T", Version: "1"}, reg)
	spec.auth = &AuthContext{AuthorizationEnabled: false}
	out := marshalSpec(t, spec)

	op := out["paths"].(map[string]any)["/protected/gated"].(map[string]any)["post"].(map[string]any)
	if d, has := op["description"]; has {
		if s, _ := d.(string); contains(s, "Required permission") {
			t.Errorf("jwt+authz.disabled: gated route must NOT carry permission suffix, got %q", s)
		}
	}
	desc, _ := op["description"].(string)
	if !contains(desc, "Already-existing prose.") {
		t.Errorf("base description lost when suffix suppressed: got %q", desc)
	}
}

// Introspection ruler — Spec.RequiredPermission stays populated on the
// route value even when the suffix is suppressed. Tooling that walks
// the registry (consumer codegen, contract diffs) can still see the
// declared permission; only the user-facing description suppresses.
func TestSpec_RequiredPermissionStaysOnRouteValue_WhenSuffixSuppressed(t *testing.T) {
	reg := authzRegistry(t)
	ops := reg.Operations()
	var gated *Operation
	for i := range ops {
		if ops[i].Path == "/protected/gated" {
			gated = &ops[i]
			break
		}
	}
	if gated == nil {
		t.Fatal("/protected/gated not registered")
	}
	if gated.Spec.RequiredPermission != "users:write" {
		t.Errorf("Spec.RequiredPermission = %q, want %q (introspection must survive suffix gating)",
			gated.Spec.RequiredPermission, "users:write")
	}
}

// Raw side — MountRaw routes go through the same gate. Cover canonical
// and raw symmetrically so a future divergence between rawOperation and
// canonicalOperation is caught by tests.
func TestSpec_DescriptionSuffix_RawRoute_Gated(t *testing.T) {
	resetGate(t)
	SetGate(noopGate)
	reg := NewRegistry()
	app := fiber.New()
	MountRaw(reg, app, fiber.MethodGet, "/raw/gated",
		func(c fiber.Ctx) error { return nil },
		RawSpec{Summary: "Raw gated", Description: "Raw prose."},
		RequirePermission("users:read"))

	// authz.enabled=true → suffix present
	specOn := NewSpec(Config{Title: "T", Version: "1"}, reg)
	specOn.auth = &AuthContext{AuthorizationEnabled: true}
	out := marshalSpec(t, specOn)
	op := out["paths"].(map[string]any)["/raw/gated"].(map[string]any)["get"].(map[string]any)
	desc, _ := op["description"].(string)
	if !contains(desc, "**Required permission:** `users:read`") {
		t.Errorf("raw + authz.enabled: description must carry suffix, got %q", desc)
	}

	// authz.enabled=false → suffix suppressed
	specOff := NewSpec(Config{Title: "T", Version: "1"}, reg)
	specOff.auth = &AuthContext{AuthorizationEnabled: false}
	out = marshalSpec(t, specOff)
	op = out["paths"].(map[string]any)["/raw/gated"].(map[string]any)["get"].(map[string]any)
	if d, has := op["description"]; has {
		if s, _ := d.(string); contains(s, "Required permission") {
			t.Errorf("raw + authz.disabled: description must NOT carry suffix, got %q", s)
		}
	}
}

func TestSpec_403_AutoEmitted_OnEveryAuthenticatedRoute(t *testing.T) {
	reg := authzRegistry(t)
	spec := NewSpec(Config{Title: "T", Version: "1"}, reg)
	spec.auth = &AuthContext{}
	out := marshalSpec(t, spec)

	// Gated route — must have 403 entry referencing ErrorEnvelope
	gated := out["paths"].(map[string]any)["/protected/gated"].(map[string]any)["post"].(map[string]any)
	gatedResp := gated["responses"].(map[string]any)
	if _, has := gatedResp["403"]; !has {
		t.Error("gated route must carry 403 in responses")
	}

	// Loose (Layer-2-only) route — must ALSO have 403 entry
	loose := out["paths"].(map[string]any)["/protected/loose"].(map[string]any)["get"].(map[string]any)
	looseResp := loose["responses"].(map[string]any)
	if _, has := looseResp["403"]; !has {
		t.Error("Layer-2-only route must still carry 403 (any authenticated route can produce it)")
	}
}

func TestSpec_403_NotEmitted_OnPublicRoute(t *testing.T) {
	reg := authzRegistry(t)
	spec := NewSpec(Config{Title: "T", Version: "1"}, reg)
	spec.auth = &AuthContext{}
	out := marshalSpec(t, spec)

	pub := out["paths"].(map[string]any)["/public"].(map[string]any)["get"].(map[string]any)
	pubResp := pub["responses"].(map[string]any)
	if _, has := pubResp["403"]; has {
		t.Error("public route must NOT carry 403 (no auth → no Forbidden outcome)")
	}
	if _, has := pubResp["401"]; has {
		t.Error("sanity: public route must not carry 401 either")
	}
}

func TestSpec_403_NotEmitted_WhenAuthDisabled(t *testing.T) {
	reg := authzRegistry(t)
	// NewSpec without WithAuth ⇒ s.auth==nil, every route counts as public
	spec := NewSpec(Config{Title: "T", Version: "1"}, reg)
	out := marshalSpec(t, spec)

	for path, methods := range out["paths"].(map[string]any) {
		for verb, op := range methods.(map[string]any) {
			resp := op.(map[string]any)["responses"].(map[string]any)
			if _, has := resp["403"]; has {
				t.Errorf("auth disabled: %s %s should not carry 403", verb, path)
			}
		}
	}
}

func TestSpec_403_ExampleCarriesMissingPermissionNotification(t *testing.T) {
	reg := authzRegistry(t)
	spec := NewSpec(Config{Title: "T", Version: "1"}, reg)
	spec.auth = &AuthContext{}
	out := marshalSpec(t, spec)

	gated := out["paths"].(map[string]any)["/protected/gated"].(map[string]any)["post"].(map[string]any)
	resp403 := gated["responses"].(map[string]any)["403"].(map[string]any)
	media := resp403["content"].(map[string]any)["application/json"].(map[string]any)
	example := media["example"].(map[string]any)

	// JSON-decoded numbers land as float64; assert by formatted value.
	if status, _ := example["status"].(float64); int(status) != 403 {
		t.Errorf("example status = %v, want 403", example["status"])
	}
	errors := example["errors"].([]any)
	msg := errors[0].(map[string]any)["messages"].([]any)[0].(map[string]any)
	if msg["notificationKey"] != "MissingPermissionNotification" {
		t.Errorf("notificationKey = %v, want MissingPermissionNotification", msg["notificationKey"])
	}
	if msg["field"] != "permission" {
		t.Errorf("field = %v, want permission", msg["field"])
	}
	if msg["semantic"] != "Forbidden" {
		t.Errorf("semantic = %v, want Forbidden", msg["semantic"])
	}
	if msg["value"] != "users:write" {
		t.Errorf("value = %v, want users:write (illustrative)", msg["value"])
	}
}

func TestSpec_AppendPermissionSuffix_StandaloneSemantic(t *testing.T) {
	cases := []struct {
		name, base, perm, want string
	}{
		{"no permission keeps base", "Existing.", "", "Existing."},
		{"no permission no base returns empty", "", "", ""},
		{"permission without base returns suffix alone", "", "users:read",
			"**Required permission:** `users:read`"},
		{"permission with base joins with blank line", "Lead.", "users:read",
			"Lead.\n\n**Required permission:** `users:read`"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := appendPermissionSuffix(tc.base, tc.perm)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// contains is the test helper; avoids importing strings everywhere.
func contains(haystack, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
