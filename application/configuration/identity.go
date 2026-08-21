package configuration

import (
	"strconv"
	"strings"
	"time"
)

// Identity carries the authenticated principal of a request — produced by the
// framework's auth middleware after a successful JWT validation and attached
// to the AppContext via SetIdentity. Handlers read it via AppContext.Identity().
//
// A nil Identity means the request is unauthenticated: the route is public
// (declared in auth.publicRoutes), the service runs with auth.mode=disabled,
// or the middleware has not yet populated it (pre-middleware code paths,
// background jobs, tests). Handlers that mandate authentication must guard
// for this explicitly.
//
// Subject/Issuer/ExpiresAt mirror the standard JWT claims (`sub`, `iss`,
// `exp`) and are always populated on a successfully authenticated request.
// Claims carries the raw claim map for services to extract custom claims
// (roles, email, tenant, …) — the framework intentionally does not opine on
// which custom claims exist or how they are named.
//
// Identity is treated as immutable after the auth middleware populates it.
// HasPermission / IsSuperAdmin cache the parsed permissions set on first
// call without locking — the same *Identity is never shared across
// goroutines (Fiber dispatches one goroutine per request).
type Identity struct {
	Subject   string
	Issuer    string
	ExpiresAt time.Time
	Claims    map[string]any

	// parsedPermissions caches the result of parsing the configured
	// permissions claim on first HasPermission call. nil = not parsed yet.
	parsedPermissions map[string]struct{}
}

// superAdminGrant is the claim entry that satisfies every permission check.
// Asked for directly via IsSuperAdmin; never accepted as a HasPermission
// argument.
const superAdminGrant = "*:*"

// permissions returns the parsed permissions claim, populating the cache on
// first use. See the Identity doc comment on why no locking is needed.
func (i *Identity) permissions() map[string]struct{} {
	if i.parsedPermissions == nil {
		i.parsedPermissions = parsePermissionsClaim(i.Claims[permissionsClaimName()])
	}
	return i.parsedPermissions
}

// HasPermission reports whether the Identity's permissions claim grants the
// given action. Matches exact ("users:read"), resource wildcard ("users:*"),
// and super-admin wildcard ("*:*"). Returns false on a nil Identity.
//
// The permission string is taken verbatim — wildcards on the caller side
// panic at runtime (the claim wildcards; the request does not). An empty
// string or a string without ':' also panics — symmetric with the boot panic
// on fwopenapi.RequirePermission. Compose "any of A or B" with an explicit
// OR over concrete actions; do not pass "users:*" to ask. To ask the
// super-admin question itself, call IsSuperAdmin.
//
// The parsed claim set is cached on the Identity after the first call;
// subsequent calls are direct map lookups + at most one extra comparison.
func (i *Identity) HasPermission(p string) bool {
	if i == nil {
		return false
	}
	if p == "" || !strings.Contains(p, ":") {
		panic("configuration.Identity.HasPermission: permission must be \"resource:action\"; got " + strconv.Quote(p))
	}
	if strings.Contains(p, "*") {
		panic("configuration.Identity.HasPermission: wildcards are not allowed on the caller side; got " +
			strconv.Quote(p) + ". Compose explicit OR over concrete actions, or call IsSuperAdmin.")
	}
	perms := i.permissions()
	if _, ok := perms[superAdminGrant]; ok {
		return true
	}
	if _, ok := perms[p]; ok {
		return true
	}
	colon := strings.Index(p, ":")
	if _, ok := perms[p[:colon+1]+"*"]; ok {
		return true
	}
	return false
}

// IsSuperAdmin reports whether the permissions claim carries the "*:*"
// grant — the entry HasPermission honors as satisfying every action.
// Returns false on a nil Identity and on a principal whose claim is absent.
//
// This is the only sanctioned way to ask the super-admin question:
// HasPermission("*:*") panics by design, because a caller-side wildcard
// blurs which action a route actually requires. IsSuperAdmin asks a
// different, well-defined question, so it does not weaken that invariant.
//
// Prefer naming a concrete permission ("users:admin") and letting "*:*"
// satisfy it — that is the framework's intended shape and keeps the grant
// auditable per resource. Reach for IsSuperAdmin only where there is no
// concrete permission to name: cross-tenant bypass inside
// Query.ToCriteria(ctx), a /whoami flag for the UI, decisions that belong
// to no single resource.
//
// Like every other Identity helper, unaffected by the authorization master
// switch (auth.authorization.enabled) — it reads the token, not the gate.
// Shares the parsed-claim cache with HasPermission and honors the claim
// name configured via authorization.permissionsClaim.
func (i *Identity) IsSuperAdmin() bool {
	if i == nil {
		return false
	}
	_, ok := i.permissions()[superAdminGrant]
	return ok
}

// TenantID returns the tenant claim configured via
// authorization.tenant.claim (default "tenant_id"). Returns "" when the
// claim is absent or empty, or on a nil Identity. Multiple shapes are
// tolerated: string, []string with one element, []any with one string
// element. Anything else returns "".
//
// When authorization.tenant.required: true is set in the yaml, the
// AuthMiddleware rejects any non-public request whose Identity has an
// empty TenantID — so consumers reaching for this helper inside
// Query.ToCriteria(ctx) / Command.ApplyTo(ctx, t) can safely treat the
// value as non-empty.
func (i *Identity) TenantID() string {
	if i == nil {
		return ""
	}
	raw, ok := i.Claims[tenantClaimName()]
	if !ok {
		return ""
	}
	switch v := raw.(type) {
	case string:
		return v
	case []string:
		if len(v) == 1 {
			return v[0]
		}
	case []any:
		if len(v) == 1 {
			if s, ok := v[0].(string); ok {
				return s
			}
		}
	}
	return ""
}

// parsePermissionsClaim normalizes the heterogeneous shapes IdPs emit for
// the permissions claim into a set. Tolerated inputs:
//   - []string{"users:read", ...}   → set of each
//   - []any{"users:read", ...}      → set of each string element
//   - "users:read users:write"      → split on whitespace
//   - "users:read,users:write"      → split on comma
//   - nil / absent / other type     → empty set
//
// Whitespace around each element is trimmed; empty elements are dropped.
func parsePermissionsClaim(raw any) map[string]struct{} {
	out := map[string]struct{}{}
	switch v := raw.(type) {
	case []string:
		for _, s := range v {
			if s = strings.TrimSpace(s); s != "" {
				out[s] = struct{}{}
			}
		}
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok {
				if s = strings.TrimSpace(s); s != "" {
					out[s] = struct{}{}
				}
			}
		}
	case string:
		sep := " "
		if strings.Contains(v, ",") {
			sep = ","
		}
		for _, s := range strings.Split(v, sep) {
			if s = strings.TrimSpace(s); s != "" {
				out[s] = struct{}{}
			}
		}
	}
	return out
}
