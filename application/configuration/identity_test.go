package configuration

import (
	"reflect"
	"testing"
	"time"
)

func TestAppContext_Identity_NilByDefault(t *testing.T) {
	ctx := NewAppContextWithRandomID(LangPTBR)
	if ctx.Identity() != nil {
		t.Fatalf("expected nil Identity by default, got %#v", ctx.Identity())
	}
}

func TestAppContext_SetIdentity_RoundTrips(t *testing.T) {
	ctx := NewAppContextWithRandomID(LangPTBR)
	exp := time.Now().Add(1 * time.Hour)
	id := &Identity{
		Subject:   "user-42",
		Issuer:    "https://idp.example.com",
		ExpiresAt: exp,
		Claims:    map[string]any{"role": "admin"},
	}
	ctx.SetIdentity(id)

	got := ctx.Identity()
	if got == nil {
		t.Fatal("expected Identity to be set, got nil")
	}
	if got.Subject != "user-42" {
		t.Errorf("Subject = %q, want %q", got.Subject, "user-42")
	}
	if got.Issuer != "https://idp.example.com" {
		t.Errorf("Issuer = %q", got.Issuer)
	}
	if !got.ExpiresAt.Equal(exp) {
		t.Errorf("ExpiresAt = %v, want %v", got.ExpiresAt, exp)
	}
	if v, _ := got.Claims["role"].(string); v != "admin" {
		t.Errorf("Claims[role] = %v, want %q", got.Claims["role"], "admin")
	}
}

func TestAppContext_ActorSubject_AnonymousWhenNil(t *testing.T) {
	ctx := NewAppContextWithRandomID(LangPTBR)
	if got := ctx.ActorSubject(); got != "anonymous" {
		t.Errorf("ActorSubject() default = %q, want %q", got, "anonymous")
	}
	if got := ctx.ActorIssuer(); got != "" {
		t.Errorf("ActorIssuer() default = %q, want \"\"", got)
	}
	if got := ctx.ActorClaims(); got != nil {
		t.Errorf("ActorClaims() default = %v, want nil", got)
	}
}

func TestAppContext_ActorMethods_ReflectIdentity(t *testing.T) {
	ctx := NewAppContextWithRandomID(LangPTBR)
	ctx.SetIdentity(&Identity{
		Subject: "user-42",
		Issuer:  "https://idp.test",
		Claims:  map[string]any{"tenant_id": "acme", "roles": []any{"admin"}},
	})
	if got := ctx.ActorSubject(); got != "user-42" {
		t.Errorf("ActorSubject() = %q", got)
	}
	if got := ctx.ActorIssuer(); got != "https://idp.test" {
		t.Errorf("ActorIssuer() = %q", got)
	}
	claims := ctx.ActorClaims()
	if claims["tenant_id"] != "acme" {
		t.Errorf("ActorClaims[tenant_id] = %v", claims["tenant_id"])
	}
	// Mutating the returned map must not affect the stored Identity.
	claims["tenant_id"] = "mutated"
	again := ctx.ActorClaims()
	if again["tenant_id"] != "acme" {
		t.Errorf("ActorClaims should return a fresh copy each call; got mutated %v", again["tenant_id"])
	}
}

func TestAppContext_SetIdentity_NilClears(t *testing.T) {
	ctx := NewAppContextWithRandomID(LangPTBR)
	ctx.SetIdentity(&Identity{Subject: "x"})
	if ctx.Identity() == nil {
		t.Fatal("expected Identity to be set before clear")
	}
	ctx.SetIdentity(nil)
	if ctx.Identity() != nil {
		t.Errorf("expected nil Identity after SetIdentity(nil), got %#v", ctx.Identity())
	}
}

func TestIdentity_HasPermission_NilSafe(t *testing.T) {
	var id *Identity
	if id.HasPermission("users:read") {
		t.Error("nil Identity must report no permission")
	}
}

func TestIdentity_HasPermission_Exact(t *testing.T) {
	id := &Identity{Claims: map[string]any{"permissions": []string{"users:read", "users:write"}}}
	if !id.HasPermission("users:read") {
		t.Error("exact match must succeed")
	}
	if id.HasPermission("users:archive") {
		t.Error("unlisted permission must be denied")
	}
}

func TestIdentity_HasPermission_ResourceWildcard(t *testing.T) {
	id := &Identity{Claims: map[string]any{"permissions": []string{"users:*"}}}
	if !id.HasPermission("users:read") {
		t.Error("users:* must grant users:read")
	}
	if !id.HasPermission("users:archive") {
		t.Error("users:* must grant users:archive")
	}
	if id.HasPermission("orders:read") {
		t.Error("users:* must NOT grant orders:read")
	}
}

func TestIdentity_HasPermission_SuperAdmin(t *testing.T) {
	id := &Identity{Claims: map[string]any{"permissions": []string{"*:*"}}}
	if !id.HasPermission("users:read") {
		t.Error("*:* must grant users:read")
	}
	if !id.HasPermission("anything:any-action") {
		t.Error("*:* must grant any action")
	}
}

func TestIdentity_IsSuperAdmin(t *testing.T) {
	cases := []struct {
		name  string
		claim any
		want  bool
	}{
		{"grant present", []string{"*:*"}, true},
		{"grant among others", []any{"users:read", "*:*"}, true},
		{"string claim", "orders:read *:*", true},
		{"resource wildcard is not super-admin", []string{"users:*"}, false},
		{"concrete permissions only", []string{"users:read", "users:write"}, false},
		{"claim absent", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := &Identity{Claims: map[string]any{"permissions": tc.claim}}
			if got := id.IsSuperAdmin(); got != tc.want {
				t.Errorf("IsSuperAdmin() = %v, want %v (claim %#v)", got, tc.want, tc.claim)
			}
		})
	}
}

func TestIdentity_IsSuperAdmin_NilSafe(t *testing.T) {
	var id *Identity
	if id.IsSuperAdmin() {
		t.Error("nil Identity must not report super-admin")
	}
}

func TestIdentity_IsSuperAdmin_CustomClaimName(t *testing.T) {
	defer SetPermissionsClaim("") // restore default
	SetPermissionsClaim("scope")
	id := &Identity{Claims: map[string]any{"scope": "*:*"}}
	if !id.IsSuperAdmin() {
		t.Error("custom claim name must be honored")
	}
	// The default name must NOT be consulted once reconfigured.
	other := &Identity{Claims: map[string]any{"permissions": "*:*"}}
	if other.IsSuperAdmin() {
		t.Error("stale default claim name must not be read")
	}
}

// IsSuperAdmin and HasPermission share one parsed-claim cache — entering
// through either method must leave the other seeing the same set.
func TestIdentity_IsSuperAdmin_SharesCacheWithHasPermission(t *testing.T) {
	id := &Identity{Claims: map[string]any{"permissions": []string{"*:*"}}}
	if !id.IsSuperAdmin() {
		t.Fatal("*:* must report super-admin")
	}
	if id.parsedPermissions == nil {
		t.Error("cache must populate after first IsSuperAdmin")
	}
	id.Claims["permissions"] = []string{"users:read"}
	if !id.HasPermission("anything:goes") {
		t.Error("HasPermission must hit the cache IsSuperAdmin populated, not re-parse")
	}
}

func TestIdentity_HasPermission_CallerWildcardPanics(t *testing.T) {
	id := &Identity{Claims: map[string]any{"permissions": []string{"users:read"}}}
	defer func() {
		if r := recover(); r == nil {
			t.Error("HasPermission(\"users:*\") must panic")
		}
	}()
	_ = id.HasPermission("users:*")
}

func TestIdentity_HasPermission_EmptyPanics(t *testing.T) {
	id := &Identity{Claims: map[string]any{}}
	defer func() {
		if r := recover(); r == nil {
			t.Error("HasPermission(\"\") must panic")
		}
	}()
	_ = id.HasPermission("")
}

func TestIdentity_HasPermission_NoColonPanics(t *testing.T) {
	id := &Identity{Claims: map[string]any{}}
	defer func() {
		if r := recover(); r == nil {
			t.Error("HasPermission(\"usersread\") must panic")
		}
	}()
	_ = id.HasPermission("usersread")
}

func TestIdentity_HasPermission_NoClaim(t *testing.T) {
	id := &Identity{Claims: map[string]any{}}
	if id.HasPermission("users:read") {
		t.Error("absent permissions claim must deny")
	}
}

func TestIdentity_HasPermission_CustomClaimName(t *testing.T) {
	defer SetPermissionsClaim("") // restore default
	SetPermissionsClaim("scope")
	id := &Identity{Claims: map[string]any{"scope": "users:read users:write"}}
	if !id.HasPermission("users:read") {
		t.Error("custom claim name must be honored")
	}
}

func TestIdentity_TenantID_DefaultClaim(t *testing.T) {
	id := &Identity{Claims: map[string]any{"tenant_id": "acme"}}
	if got := id.TenantID(); got != "acme" {
		t.Errorf("TenantID() = %q, want %q", got, "acme")
	}
}

func TestIdentity_TenantID_Absent(t *testing.T) {
	id := &Identity{Claims: map[string]any{}}
	if got := id.TenantID(); got != "" {
		t.Errorf("absent claim must return \"\", got %q", got)
	}
}

func TestIdentity_TenantID_NilSafe(t *testing.T) {
	var id *Identity
	if got := id.TenantID(); got != "" {
		t.Errorf("nil Identity must return \"\", got %q", got)
	}
}

func TestIdentity_TenantID_StringSlice(t *testing.T) {
	id := &Identity{Claims: map[string]any{"tenant_id": []string{"acme"}}}
	if got := id.TenantID(); got != "acme" {
		t.Errorf("[]string{\"acme\"} → %q, want \"acme\"", got)
	}
}

func TestIdentity_TenantID_AnySlice(t *testing.T) {
	id := &Identity{Claims: map[string]any{"tenant_id": []any{"acme"}}}
	if got := id.TenantID(); got != "acme" {
		t.Errorf("[]any{\"acme\"} → %q, want \"acme\"", got)
	}
}

func TestIdentity_TenantID_CustomClaimName(t *testing.T) {
	defer SetTenantClaim("") // restore default
	SetTenantClaim("org")
	id := &Identity{Claims: map[string]any{"org": "globex"}}
	if got := id.TenantID(); got != "globex" {
		t.Errorf("TenantID() with custom claim = %q, want %q", got, "globex")
	}
}

func TestIdentity_TenantID_UnexpectedShape(t *testing.T) {
	cases := []any{
		42,
		[]string{"a", "b"}, // multi-element rejected — ambiguous
		map[string]any{"x": 1},
	}
	for _, raw := range cases {
		id := &Identity{Claims: map[string]any{"tenant_id": raw}}
		if got := id.TenantID(); got != "" {
			t.Errorf("unexpected shape %T → %q, want \"\"", raw, got)
		}
	}
}

func TestParsePermissionsClaim_AllShapes(t *testing.T) {
	cases := []struct {
		name string
		raw  any
		want map[string]struct{}
	}{
		{"nil", nil, map[string]struct{}{}},
		{"stringSlice", []string{"users:read", "users:write"},
			map[string]struct{}{"users:read": {}, "users:write": {}}},
		{"anySlice", []any{"users:read", "users:write"},
			map[string]struct{}{"users:read": {}, "users:write": {}}},
		{"spaceSep", "users:read users:write",
			map[string]struct{}{"users:read": {}, "users:write": {}}},
		{"commaSep", "users:read,users:write",
			map[string]struct{}{"users:read": {}, "users:write": {}}},
		{"trimmed", " users:read , users:write ",
			map[string]struct{}{"users:read": {}, "users:write": {}}},
		{"singleString", "users:read", map[string]struct{}{"users:read": {}}},
		{"unsupportedType", 42, map[string]struct{}{}},
		{"emptyString", "", map[string]struct{}{}},
		{"anySliceMixedJunk", []any{"users:read", 123, "users:write"},
			map[string]struct{}{"users:read": {}, "users:write": {}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parsePermissionsClaim(tc.raw)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parsePermissionsClaim(%#v) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

func TestIdentity_HasPermission_CachesParsed(t *testing.T) {
	id := &Identity{Claims: map[string]any{"permissions": []string{"users:read"}}}
	_ = id.HasPermission("users:read")
	if id.parsedPermissions == nil {
		t.Error("cache must populate after first HasPermission")
	}
	// Mutating the source claim post-cache must NOT change subsequent lookups —
	// Identity is documented as immutable after middleware populates it.
	id.Claims["permissions"] = []string{"users:write"}
	if id.HasPermission("users:write") {
		t.Error("subsequent call must hit cache, not re-parse")
	}
}
