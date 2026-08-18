package openapi

import (
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v3"
)

// MountRaw is the manual-pure counterpart of Mount: it registers handler
// on the Fiber group AND records the operation in registry, but the
// schema/parameter shape comes from a hand-declared RawSpec rather than
// from the reflection-driven RouteSpec the canonical wrappers produce.
//
// Use when there is no typed Request DTO to point a Mount call at —
// Whoami (replies with a fiber.Map), Echo (writes raw bytes / parses
// multipart / streams SSE), Showcase Keycloak/HTTPClient (assembles
// vendor-shaped DTOs the consumer owns). Anything that does NOT chain
// BodyParser/QueryParser.Parse/BindPath at the request boundary belongs
// here; otherwise Mount + RouteSpec is the right call.
//
// When registry is nil, MountRaw is a thin passthrough — only
// group.Add runs, matching Mount's nil-safe contract so consumers can
// pass d.OpenAPIRegistry uniformly without nil-checking.
//
//	openapi.MountRaw(d.OpenAPIRegistry, app, fiber.MethodGet, "/whoami",
//	    whoami,
//	    openapi.RawSpec{
//	        Summary: "Returns the authenticated identity",
//	        Tags:    []string{"Auth"},
//	        Responses: map[int]openapi.ResponseSpec{
//	            200: {Type: reflect.TypeOf(WhoamiResponse{})},
//	        },
//	    })
//
// MountRaw normalizes RawSpec on the way in: every path-in parameter
// has Required forced to true (OpenAPI 3.1 §4.4.4 requirement), and the
// Required flag flowed in is otherwise preserved.
//
// Variadic opts behave identically to openapi.Mount — the only option in
// this round is RequirePermission, which patches raw.RequiredPermission for
// the spec generator and wraps handler with the framework's permission gate.
func MountRaw(
	registry *Registry,
	group fiber.Router,
	method, path string,
	handler fiber.Handler,
	raw RawSpec,
	opts ...MountOption,
) {
	routePath := JoinPath(group, path)
	routeID := strings.ToUpper(method) + " " + routePath

	cfg := processOptions(opts, routeID)
	if cfg.requiredPermission != "" {
		if raw.Public {
			panic("openapi.MountRaw: route " + routeID + " declares Public:true and RequirePermission(" +
				strconv.Quote(cfg.requiredPermission) + ") — semantic contradiction; pick one")
		}
		raw.RequiredPermission = cfg.requiredPermission
		gate := resolveGate()
		if gate == nil {
			panic("openapi.MountRaw: route " + routeID + " uses RequirePermission(" +
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
	normalizeRawSpec(&raw)

	var resolvedReq map[string]rawExample
	if raw.RequestBody != nil && len(raw.RequestBody.Examples) > 0 {
		resolvedReq = validateExampleMap(
			raw.RequestBody.Examples,
			raw.RequestBody.Type,
			raw.RequestBody.Type != nil,
			routeID, "requestBody",
		)
	}

	var resolvedResp map[int]map[string]rawExample
	for status, rs := range raw.Responses {
		if len(rs.Examples) == 0 {
			continue
		}
		if resolvedResp == nil {
			resolvedResp = map[int]map[string]rawExample{}
		}
		slot := "response " + strconv.Itoa(status)
		resolvedResp[status] = validateExampleMap(
			rs.Examples,
			rs.Type,
			rs.Type != nil,
			routeID, slot,
		)
	}

	registry.add(Operation{
		Method:                   method,
		Path:                     routePath,
		Raw:                      &raw,
		resolvedRequestExamples:  resolvedReq,
		resolvedResponseExamples: resolvedResp,
	})
}

// normalizeRawSpec applies the structural invariants RawSpec carries
// implicitly so consumers do not have to remember them at every call
// site:
//   - path parameters are always Required per OpenAPI 3.1 §4.4.4
//
// Other fields pass through unchanged; the spec-assembly phase applies
// remaining normalization (default content types, error-envelope
// expansion).
func normalizeRawSpec(raw *RawSpec) {
	for i := range raw.Parameters {
		if raw.Parameters[i].In == InPath {
			raw.Parameters[i].Required = true
		}
	}
}
