package openapi

import (
	"reflect"
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v3"
)

// Mount registers handler on the Fiber group AND records the operation
// in registry. When registry is nil, Mount is a thin passthrough — only
// group.Add runs. Consumers that opt out of /openapi.json keep their
// existing wiring intact: pass d.OpenAPIRegistry (nil under
// `Wiring.OpenAPI == nil`) at the call site uniformly, the no-op
// branch ensures zero cost.
//
// Canonical pairing — the wrapper sibling returns (handler, spec):
//
//	h, spec := fwweb.HandleCommandWithBodySpec(d.Pipeline,
//	    requests.InsertUserRequest{},
//	    requests.InsertUserResponse{}.FromResult,
//	    &handlers.InsertCommandHandler[...]{...},
//	    fiber.StatusCreated)
//	openapi.Mount(d.OpenAPIRegistry, users, fiber.MethodPost, "/",
//	    h, spec,
//	    openapi.Doc{Summary: "Create a user", Tags: []string{"Users"}})
//
// Manual-with-pipeline pairing — the consumer hand-rolls handler AND
// supplies the same RouteSpec the wrapper would have produced (pointing
// to the Request / Response DTOs they already declared for
// BodyParser / ParseCriteria / BindPath):
//
//	openapi.Mount(d.OpenAPIRegistry, group, fiber.MethodPost, "/",
//	    customInsertUser(d.Pipeline, repo, d.Auditor, svc),
//	    openapi.RouteSpec{
//	        RequestType:   reflect.TypeOf(requests.InsertUserCustomRequest{}),
//	        ResponseType:  reflect.TypeOf(responses.UserCustomResponse{}),
//	        SuccessStatus: fiber.StatusCreated,
//	    },
//	    openapi.Doc{Summary: "...", Tags: []string{"Users — manual"}})
//
// The full registered path is joined from the group's Prefix and path.
// For *fiber.App (no prefix) the path is used verbatim.
//
// Variadic opts attach declarative behaviors to the route. The only option
// in this round is RequirePermission, which (a) populates spec.RequiredPermission
// for the OpenAPI generator and (b) wraps handler with the framework's
// permission gate via the Gate registered by SetGate. Future options
// (alternative gates, rate-limit hints, custom envelopes) plug into the
// same variadic surface.
func Mount(
	registry *Registry,
	group fiber.Router,
	method, path string,
	handler fiber.Handler,
	spec RouteSpec,
	doc Doc,
	opts ...MountOption,
) {
	routePath := JoinPath(group, path)
	routeID := strings.ToUpper(method) + " " + routePath

	if spec.Paged && (spec.ResponseType == nil || isResponseNone(spec.ResponseType)) {
		panic("openapi.Mount: route " + routeID + " declares Paged:true with " +
			"nil ResponseType or fwresponses.None — paging requires a per-item " +
			"wire shape on ResponseType; pick one")
	}

	if spec.FileResponse != nil {
		if spec.Paged {
			panic("openapi.Mount: route " + routeID + " declares FileResponse with Paged:true — " +
				"a file/download success is not a paged envelope; pick one")
		}
		if spec.ResponseType != nil && !isResponseNone(spec.ResponseType) {
			panic("openapi.Mount: route " + routeID + " declares FileResponse with a non-nil " +
				"ResponseType — the success body is the file, not a typed JSON envelope; pick one")
		}
		if spec.FileResponse.ContentType == "" {
			panic("openapi.Mount: route " + routeID + " declares FileResponse with an empty " +
				"ContentType — set the download media type (e.g. \"text/csv\")")
		}
	}

	cfg := processOptions(opts, routeID)
	if cfg.requiredPermission != "" {
		if doc.Public {
			panic("openapi.Mount: route " + routeID + " declares Public:true and RequirePermission(" +
				strconv.Quote(cfg.requiredPermission) + ") — semantic contradiction; pick one")
		}
		spec.RequiredPermission = cfg.requiredPermission
		gate := resolveGate()
		if gate == nil {
			panic("openapi.Mount: route " + routeID + " uses RequirePermission(" +
				strconv.Quote(cfg.requiredPermission) + ") but no Gate is registered. " +
				"bootstrap.Run normally calls openapi.SetGate(web.PermissionGate(deps.Translator)) — " +
				"services using bootstrap.Build/Serve manually must wire it explicitly")
		}
		handler = gate(handler, cfg.requiredPermission)
	}

	group.Add([]string{method}, path, handler)
	if registry == nil {
		return
	}

	resolvedReq := validateExampleMap(doc.RequestExamples, spec.RequestType, true, routeID, "requestBody")
	resolvedResp := validateCanonicalResponseExamples(doc.ResponseExamples, spec, routeID)

	registry.add(Operation{
		Method:                   method,
		Path:                     routePath,
		Spec:                     spec,
		Doc:                      doc,
		resolvedRequestExamples:  resolvedReq,
		resolvedResponseExamples: resolvedResp,
	})
}

// validateCanonicalResponseExamples processes Doc.ResponseExamples per
// status: examples on the SuccessStatus are validated strictly against
// RouteSpec.ResponseType (the inner data shape — the consumer fills the
// payload that the framework wraps in the canonical Response envelope);
// examples on every other status are checked for JSON validity only
// (those carry the error envelope shape, which is shared and accepts
// arbitrary errors[] content).
func validateCanonicalResponseExamples(
	declared map[int]map[string]Example,
	spec RouteSpec,
	routeID string,
) map[int]map[string]rawExample {
	if len(declared) == 0 {
		return nil
	}
	successStatus := spec.SuccessStatus
	if successStatus == 0 {
		successStatus = 200
	}
	out := make(map[int]map[string]rawExample, len(declared))
	for status, exs := range declared {
		strict := status == successStatus
		var wantType reflect.Type
		if strict && spec.ResponseType != nil && !isResponseNone(spec.ResponseType) {
			wantType = spec.ResponseType
		}
		slot := "response " + strconv.Itoa(status)
		out[status] = validateExampleMap(exs, wantType, strict && wantType != nil, routeID, slot)
	}
	return out
}

// JoinPath resolves the registered route to its fully-qualified path,
// prepending the group's Prefix when group is a *fiber.Group. *fiber.App
// has no prefix concept, so its routes are returned verbatim. Exported
// because manual-pure consumers (Phase 3 MountRaw) call it too — the
// rule "registered path = group prefix + relative path" must stay one
// implementation across the package.
func JoinPath(group fiber.Router, path string) string {
	if g, ok := group.(*fiber.Group); ok && g.Prefix != "" {
		return strings.TrimRight(g.Prefix, "/") + path
	}
	return path
}
