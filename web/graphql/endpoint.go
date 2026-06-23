package graphql

import (
	"github.com/ClaudioSchirmer/omnicore/web"
	"github.com/gofiber/fiber/v3"
)

// gqlRequest is the standard GraphQL POST request body.
type gqlRequest struct {
	Query         string         `json:"query"`
	Variables     map[string]any `json:"variables"`
	OperationName string         `json:"operationName"`
}

// Handler returns the fiber.Handler serving the GraphQL endpoint for this
// registry. GraphQL is its own web surface — it does NOT go through the REST
// openapi.Mount/MountRaw machinery and never appears in the Swagger/OpenAPI
// document; the only thing shared with REST is the application-layer handlers
// it dispatches to. Per the GraphQL convention the response is always HTTP 200
// with a `{ data, errors }` body — request faults (parse/validation) and field
// faults (domain notifications) travel in `errors`, never the status line. The
// per-request *AppContext (request id + language + identity) is taken from the
// same AppContextMiddleware the REST routes use, so tenant/auth overlays and
// cancellation flow into the dispatched handlers unchanged.
func (r *Registry) Handler() fiber.Handler {
	return func(c fiber.Ctx) error {
		var req gqlRequest
		if err := c.Bind().Body(&req); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(Response{
				Errors: []GraphQLError{{Message: "invalid GraphQL request body"}},
			})
		}
		if req.Query == "" {
			return c.Status(fiber.StatusBadRequest).JSON(Response{
				Errors: []GraphQLError{{Message: "missing GraphQL query"}},
			})
		}
		appCtx := web.AppContext(c)
		appCtx.SetParent(c)
		resp := r.Execute(appCtx, req.Query, req.Variables, req.OperationName)
		return c.JSON(resp)
	}
}
