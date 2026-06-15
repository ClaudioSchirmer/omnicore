package bootstrap

import (
	"context"

	"github.com/ClaudioSchirmer/omnicore/application/translation"
	"github.com/ClaudioSchirmer/omnicore/web/openapi"
	"github.com/gofiber/fiber/v3"
)

// Wiring is what the service returns via the Wire callback.
//
// Features is the main declaration: each Feature mounts its own routes
// and, if it implements ReadableFeature, contributes Views to SyncEngine.
//
// Translations is the list of translation modules of the service, imported
// into the framework's default Translator at boot.
//
// BeforeServe is an optional hook to register global routes/middlewares
// outside the Feature model (e.g. one-off administrative routes).
// OnShutdown is an optional hook for custom cleanup on drain.
//
// OpenAPI is an opt-in config: when non-nil, the framework constructs an
// openapi.Registry, exposes it on Deps.OpenAPIRegistry for features to
// pass to Mount / MountRaw, and registers GET /openapi.json + GET /docs
// after every feature has mounted. nil disables OpenAPI entirely —
// Deps.OpenAPIRegistry stays nil and Mount / MountRaw become
// passthroughs on the Fiber side (no spec routes, no in-memory
// registry, no schema generation overhead).
type Wiring struct {
	Translations []translation.Module
	Features     []Feature
	BeforeServe  func(app *fiber.App, deps Deps) error
	OnShutdown   func(ctx context.Context) error
	OpenAPI      *openapi.Config

	// UpstreamSubscriptions is the manual-lifecycle counterpart of
	// Config.UpstreamSubscriptions — populated when callers want to
	// declare subscriptions in Go (bootstrap.Build + Serve, integration
	// tests, ad-hoc replicas). Under bootstrap.Run the canonical source
	// is YAML: cfg.UpstreamSubscriptions is read first; if Wiring also
	// declares entries, they're appended and any Topic collision aborts
	// the boot — there is no per-field merging or implicit override.
	// Empty/nil means the service does no cross-service composition.
	// See tasks/mongo_cross_service_composition_final.md.
	UpstreamSubscriptions []UpstreamSubscription
}
