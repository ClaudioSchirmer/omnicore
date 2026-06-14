package openapi

import (
	"github.com/gofiber/fiber/v2"
)

// SpecPath is the framework-controlled route where the OpenAPI JSON
// document is served. It is intentionally NOT operator-configurable —
// the YAML knob only moves the UI HTML route (Swagger UI) because the
// JSON path is internal plumbing the UI fetches from. Tooling that
// scrapes the spec relies on it living at a canonical, predictable
// path.
const SpecPath = "/openapi.json"

// registerOptions accumulate the functional options passed to Register.
type registerOptions struct {
	uiPath string
	auth   *AuthContext
}

// RegisterOption customizes Register's behavior. Functional options keep
// the surface stable as the framework grows: future phases add options
// without changing the call signature consumers (bootstrap, custom Wire
// flows) already rely on.
type RegisterOption func(*registerOptions)

// WithAuth attaches an AuthContext to the Spec — bootstrap passes this
// when the service runs under `auth.mode: jwt` so the rendered
// document gains the bearerAuth security scheme and per-operation
// security entries. Without WithAuth, the spec carries no auth
// declarations regardless of runtime config.
func WithAuth(auth AuthContext) RegisterOption {
	return func(o *registerOptions) { o.auth = &auth }
}

// WithUIPath overrides the default "/docs" route where the Swagger UI
// HTML is served. The path must start with "/"; bootstrap validates it
// before reaching here, so Register treats an empty value as "use the
// default" rather than failing.
func WithUIPath(path string) RegisterOption {
	return func(o *registerOptions) { o.uiPath = path }
}

// Register mounts the two documentation-facing routes on app:
//
//   - GET SpecPath — serves the JSON-encoded OpenAPI 3.1 spec.
//     Built lazily on the first request and cached on the Spec value
//     for the lifetime of the process.
//   - GET uiPath — serves the Swagger UI HTML that loads SpecPath
//     and renders the interactive explorer. Default "/docs"; override
//     via WithUIPath.
//
// Both routes are intentionally public; when the consumer service runs
// with `auth.mode: jwt`, the bootstrap layer lists them in
// `auth.publicRoutes` automatically so the AuthMiddleware lets
// unauthenticated traffic through.
//
// Register intentionally avoids registering with the Registry — the
// SpecPath and UI routes are scaffolding for the spec itself, not part
// of the documented surface. Consumers who want them in the spec can
// register them through MountRaw with a manual RawSpec.
func Register(app *fiber.App, cfg Config, registry *Registry, opts ...RegisterOption) {
	resolved := registerOptions{uiPath: "/docs"}
	for _, opt := range opts {
		opt(&resolved)
	}
	if resolved.uiPath == "" {
		resolved.uiPath = "/docs"
	}

	spec := NewSpec(cfg, registry)
	if resolved.auth != nil {
		spec.auth = resolved.auth
	}

	app.Get(SpecPath, func(c *fiber.Ctx) error {
		bytes, err := spec.Build()
		if err != nil {
			c.Set(fiber.HeaderContentType, "application/json; charset=utf-8")
			return c.Status(fiber.StatusInternalServerError).SendString(`{"error":"openapi build failed"}`)
		}
		c.Set(fiber.HeaderContentType, "application/json; charset=utf-8")
		return c.Send(bytes)
	})

	app.Get(resolved.uiPath, func(c *fiber.Ctx) error {
		c.Set(fiber.HeaderContentType, "text/html; charset=utf-8")
		var languages []LanguageOption
		if cfg.LanguageSelector {
			languages = cfg.Languages
		}
		return c.SendString(SwaggerUIHTML(cfg.Title, SpecPath, languages))
	})
}
