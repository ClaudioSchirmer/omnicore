package openapi

import (
	"strconv"
	"strings"
)

// MountOption declaratively configures a Mount / MountRaw call. The only
// option in this round is RequirePermission; future options (alternative
// gates, custom 403 envelopes, rate-limit tags) extend the same variadic
// surface. Consumer code never implements MountOption — the framework owns
// the option set.
type MountOption func(*mountConfig)

// mountConfig collects the resolved options for a single Mount / MountRaw
// call. Lives inside this file so Mount / MountRaw share the processing
// logic via processOptions.
type mountConfig struct {
	requiredPermission string
}

// RequirePermission declares the permission the request's Identity must
// satisfy for the route to execute. Permission strings are "resource:action"
// (Stripe / AWS / Auth0 convention); the IdP grants the actual claim.
// Wildcards on the caller side are rejected — the token wildcards, the
// route declares the exact action.
//
// Mount / MountRaw, on receiving this option:
//
//  1. Set spec.RequiredPermission (RouteSpec or RawSpec) so the OpenAPI
//     generator can append "**Required permission:** `<p>`" to the
//     operation description — but only when the runtime gate is actually
//     enforcing (`auth.mode: jwt` AND `auth.authorization.enabled: true`).
//     Under disabled or jwt-without-authz the value is preserved for
//     introspection while the description suffix is omitted, so the spec
//     never advertises a constraint the server is not honoring.
//
//  2. Wrap the handler with the framework's permission gate (registered via
//     SetGate). The gate short-circuits with the canonical 403 envelope
//     when the Identity does not carry the required permission; otherwise
//     it delegates to the original handler.
//
// Step 2 runs even when Registry is nil — the runtime gate is independent
// of OpenAPI being enabled.
//
// Boot panics:
//
//   - permission == "" or has no colon → not a valid "resource:action"
//   - permission contains "*" → caller-side wildcards are rejected
//   - duplicate RequirePermission on the same Mount call → RequireAny /
//     RequireAll over multiple permissions is out of scope; the IdP grants
//     a compound permission instead, and a duplicate option is always a
//     programming bug
//   - Mount is called with both Public:true and RequirePermission → semantic
//     contradiction (public bypasses auth; permission requires JWT)
//
// All four panics fire at service boot — the per-string panics at the
// RequirePermission(...) call site itself (eager validation, best stack
// trace), the duplicate and Public+Required panics later when Mount
// processes the options.
func RequirePermission(permission string) MountOption {
	if permission == "" || !strings.Contains(permission, ":") {
		panic("openapi.RequirePermission: permission must be \"resource:action\"; got " + strconv.Quote(permission))
	}
	if strings.Contains(permission, "*") {
		panic("openapi.RequirePermission: wildcards on the caller side are not allowed; got " +
			strconv.Quote(permission) + ". The IdP grants wildcards; the route declares the exact action")
	}
	return func(c *mountConfig) {
		if c.requiredPermission != "" {
			panic("openapi.RequirePermission: duplicate option on the same Mount/MountRaw call (previous=" +
				strconv.Quote(c.requiredPermission) + ", new=" + strconv.Quote(permission) +
				"). Combine via a compound permission at the IdP, not via multiple RequirePermission options")
		}
		c.requiredPermission = permission
	}
}

// processOptions folds the variadic options into a mountConfig. routeID is
// purely diagnostic — RequirePermission's own panic messages do not need it,
// but future option-level validations (e.g. cross-option consistency) will,
// so the routeID is plumbed through from the start.
func processOptions(opts []MountOption, _ string) mountConfig {
	var cfg mountConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	return cfg
}
