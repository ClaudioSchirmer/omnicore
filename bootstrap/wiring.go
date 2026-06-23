package bootstrap

import (
	"context"

	"github.com/ClaudioSchirmer/omnicore/application/translation"
	"github.com/ClaudioSchirmer/omnicore/infra/cache"
	"github.com/ClaudioSchirmer/omnicore/web/graphql"
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

	// GraphQL is an opt-in, self-contained web surface: a graphql.Registry the
	// service builds with graphql.New(deps.Pipeline) and attaches read/write
	// handlers to. When non-nil, bootstrap mounts a single POST endpoint
	// (Config.GraphQL.Path) serving it. GraphQL is deliberately separate from
	// REST/OpenAPI — it never goes through openapi.Mount/MountRaw, never
	// appears in the Swagger document, and is not policed by the REST route
	// scans; the only shared surface is the application-layer handlers it
	// dispatches to. nil disables GraphQL entirely.
	GraphQL *graphql.Registry

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

	// Cache is the escape hatch for cache.store: custom in YAML. When
	// set, the framework uses this implementation as Deps.Cache; the
	// conflict matrix (declared in cache_config.go::resolveCache)
	// rejects every YAML store value other than "custom" so the
	// configuration always describes the intent.
	Cache cache.Cache

	// SharedCache is the escape hatch for cache.shared.store: custom.
	// Same conflict matrix as Cache, with the additional rule that
	// cache.shared.store: memory is rejected at boot regardless of
	// Wiring — an in-process LRU cannot satisfy the cross-service
	// read contract.
	SharedCache cache.Cache
}
