package graphql

import (
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/application/translation"
)

// guardedRegistry registers the `users` read field behind
// RequirePermission("users:read"), with the authorization master switch set to
// `authz`.
func guardedRegistry(authz bool) *Registry {
	h := &fakeReadHandler{page: queries.Page{}}
	return New(pipeline.New(translation.Default())).
		Register(Query[execRequest, execResponse]("users", "User", h, RequirePermission("users:read"))).
		EnableAuthorization(authz)
}

// TestPermissionGate_OffIsPassThrough — with the master switch off (the
// default / dev posture), a RequirePermission annotation is inert: the field
// resolves even for an unauthenticated request. Mirrors the REST gate's
// incremental-rollout no-op.
func TestPermissionGate_OffIsPassThrough(t *testing.T) {
	reg := guardedRegistry(false)
	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG) // no identity
	resp := reg.Execute(ctx, `query { users(first: 1) { totalCount } }`, nil, "")
	if len(resp.Errors) != 0 {
		t.Fatalf("authz off must pass through; got errors %+v", resp.Errors)
	}
}

// TestPermissionGate_OnDeniesWithoutPermission — switch on + an Identity that
// lacks the permission (here nil = unauthenticated) is rejected with the
// canonical MissingPermissionNotification triple, and the handler never runs.
func TestPermissionGate_OnDeniesWithoutPermission(t *testing.T) {
	reg := guardedRegistry(true)
	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG) // nil identity
	resp := reg.Execute(ctx, `query { users(first: 1) { totalCount } }`, nil, "")
	if len(resp.Errors) == 0 {
		t.Fatal("authz on + no permission must be denied")
	}
	e := resp.Errors[0]
	if got := e.Extensions["semantic"]; got != "Forbidden" {
		t.Errorf("extensions.semantic = %v, want Forbidden", got)
	}
	if got := e.Extensions["notificationKey"]; got != "MissingPermissionNotification" {
		t.Errorf("extensions.notificationKey = %v, want MissingPermissionNotification", got)
	}
	if got := e.Extensions["field"]; got != "permission" {
		t.Errorf("extensions.field = %v, want permission", got)
	}
	if resp.Data["users"] != nil {
		t.Errorf("denied request must not resolve the handler; data.users = %v", resp.Data["users"])
	}
}

// TestPermissionGate_OnAllowsWithPermission — switch on + an Identity carrying
// the declared permission resolves normally.
func TestPermissionGate_OnAllowsWithPermission(t *testing.T) {
	reg := guardedRegistry(true)
	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG)
	ctx.SetIdentity(&configuration.Identity{
		Claims: map[string]any{"permissions": []string{"users:read"}},
	})
	resp := reg.Execute(ctx, `query { users(first: 1) { totalCount } }`, nil, "")
	if len(resp.Errors) != 0 {
		t.Fatalf("identity with users:read must pass; got %+v", resp.Errors)
	}
}
