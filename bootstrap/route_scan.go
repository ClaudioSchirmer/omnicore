package bootstrap

import (
	"sort"
	"strings"

	"github.com/ClaudioSchirmer/omnicore/web/openapi"
	"github.com/gofiber/fiber/v3"
)

// scanAuthorization enforces "every non-public route declares
// fwopenapi.RequirePermission(...)" when auth.authorization.enabled is set.
// Lists every offending route in a single panic so the maintainer fixes them
// in one pass rather than discovering them one by one. Skipped silently when
// the flag is off so services not yet on authz boot normally.
//
// Public detection mirrors openapi.Spec.isPublic: a route is public when
//
//   - Doc.Public = true on a canonical operation, OR
//   - RawSpec.Public = true on a raw operation, OR
//   - "METHOD /path" appears in publicRoutes (the augmented list bootstrap
//     uses for the auth middleware, including the OpenAPI documentation
//     routes when applicable)
//
// The augmented publicRoutes is the caller's responsibility — buildApp
// already builds it once for the AuthMiddleware and passes the same value
// here, so spec.isPublic and this scan agree on the bypass list.
func scanAuthorization(reg *openapi.Registry, publicRoutes []string) {
	if reg == nil {
		return
	}
	publicSet := buildPublicRouteSet(publicRoutes)
	var offenders []string
	for _, op := range reg.Operations() {
		if isOperationPublic(op, publicSet) {
			continue
		}
		if requiredPermissionOf(op) == "" {
			offenders = append(offenders, strings.ToUpper(op.Method)+" "+op.Path)
		}
	}
	if len(offenders) == 0 {
		return
	}
	sort.Strings(offenders)
	panic("bootstrap: auth.authorization.enabled=true requires every non-public route to declare " +
		"fwopenapi.RequirePermission(...). Offending route(s):\n  " + strings.Join(offenders, "\n  "))
}

// scanRouteRegistration enforces the framework-wide convention that every
// Fiber route registered on the app passes through openapi.Mount /
// openapi.MountRaw. Active independently of auth.authorization.enabled —
// the convention is structural to the framework (Mount/MountRaw is the
// single canonical channel for documentation, gating, and observability),
// not specific to authz.
//
// Skipped when Wiring.OpenAPI is nil (services that opt out of the spec
// surface accept that they own their routing visibility). Otherwise: any
// Fiber-registered route whose "METHOD /path" is not in the Registry is
// listed in a single panic so the maintainer migrates them all at once.
//
// Reserves the Method+Path tuple shape "METHOD /path" — the exact same
// string Registry operations are keyed by, so the comparison stays
// canonical.
func scanRouteRegistration(app *fiber.App, reg *openapi.Registry) {
	if reg == nil || app == nil {
		return
	}
	registered := make(map[string]struct{})
	for _, op := range reg.Operations() {
		registered[strings.ToUpper(op.Method)+" "+op.Path] = struct{}{}
	}
	var offenders []string
	for _, route := range app.GetRoutes(true) {
		key := strings.ToUpper(route.Method) + " " + route.Path
		if _, ok := registered[key]; ok {
			continue
		}
		offenders = append(offenders, key)
	}
	if len(offenders) == 0 {
		return
	}
	sort.Strings(offenders)
	panic("bootstrap: every Fiber route must be registered via openapi.Mount or openapi.MountRaw " +
		"so the spec, the runtime gate, and the boot validation share one canonical channel. " +
		"Routes registered outside that channel:\n  " + strings.Join(offenders, "\n  "))
}

// scanPublicRoutes validates the operator-declared auth.publicRoutes against
// the routes actually registered on the app. The AuthMiddleware matches a
// public route by EXACT "METHOD /path" string (web.matchPublic), so a typo
// (GET /helth), a wrong method, or a trailing slash silently leaves the
// intended route behind the bearer wall — and a path carrying a Fiber
// parameter or wildcard (GET /users/:id) can NEVER match a concrete request
// path, so it stays private with no diagnostic. Both sides of the reference
// are in hand at boot: the parsed publicRoutes and app.GetRoutes(true). Every
// offender is listed in a single panic so the operator fixes them in one pass.
//
// Runs only on the operator's own list — the framework-added documentation
// routes (the OpenAPI spec + UI, the optional root redirect) are correct by
// construction and not re-validated here. Skipped when app is nil or no
// publicRoutes are declared.
func scanPublicRoutes(app *fiber.App, publicRoutes []string) {
	if app == nil || len(publicRoutes) == 0 {
		return
	}
	registered := make(map[string]struct{})
	for _, route := range app.GetRoutes(true) {
		registered[strings.ToUpper(route.Method)+" "+route.Path] = struct{}{}
	}
	var offenders []string
	for _, raw := range publicRoutes {
		parts := strings.Fields(raw)
		if len(parts) != 2 {
			offenders = append(offenders, raw+` (must be "METHOD /path")`)
			continue
		}
		method, path := strings.ToUpper(parts[0]), parts[1]
		if strings.ContainsAny(path, ":*") {
			offenders = append(offenders, method+" "+path+
				" (carries a path parameter or wildcard — auth.publicRoutes is matched by exact path and can never match a concrete request; mark the route Doc.Public=true / RawSpec.Public=true instead)")
			continue
		}
		if _, ok := registered[method+" "+path]; !ok {
			offenders = append(offenders, method+" "+path+" (no route is registered under this method+path)")
		}
	}
	if len(offenders) == 0 {
		return
	}
	sort.Strings(offenders)
	panic("bootstrap: auth.publicRoutes must reference routes that exist and are matchable by exact method+path. Offending entr(ies):\n  " +
		strings.Join(offenders, "\n  "))
}

func buildPublicRouteSet(publicRoutes []string) map[string]struct{} {
	set := make(map[string]struct{}, len(publicRoutes))
	for _, r := range publicRoutes {
		parts := strings.Fields(r)
		if len(parts) != 2 {
			continue
		}
		set[strings.ToUpper(parts[0])+" "+parts[1]] = struct{}{}
	}
	return set
}

func isOperationPublic(op openapi.Operation, publicSet map[string]struct{}) bool {
	if op.Raw != nil && op.Raw.Public {
		return true
	}
	if op.Raw == nil && op.Doc.Public {
		return true
	}
	_, ok := publicSet[strings.ToUpper(op.Method)+" "+op.Path]
	return ok
}

func requiredPermissionOf(op openapi.Operation) string {
	if op.Raw != nil {
		return op.Raw.RequiredPermission
	}
	return op.Spec.RequiredPermission
}
