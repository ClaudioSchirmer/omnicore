package bootstrap

import (
	"context"

	"github.com/ClaudioSchirmer/omnicore/application/translation"
	"github.com/ClaudioSchirmer/omnicore/infra/cache"
	"github.com/ClaudioSchirmer/omnicore/web/openapi"
	"github.com/gofiber/fiber/v3"
)

// Wiring is what the service returns via the Wire callback.
//
// Features is the main declaration: each Feature mounts its own routes
// and, if it implements ReadableFeature, contributes Views to SyncEngine.
// A feature may also opt into the GraphQL and gRPC surfaces by implementing
// GraphQLFeature / GRPCFeature — the framework discovers those by type
// assertion (like ReadableFeature), builds the single shared registry, and
// serves it; the service never constructs a graphql/grpc registry itself.
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

	// The GraphQL and gRPC surfaces are NOT declared here: a feature opts in
	// by implementing GraphQLFeature / GRPCFeature, and the framework builds +
	// serves the single shared registry (surfaced on Deps.GraphQLRegistry /
	// Deps.GRPCRegistry). Both surfaces stay deliberately separate from
	// REST/OpenAPI — never in the Swagger document, not policed by the REST
	// route scans; the only shared surface is the application-layer handlers
	// the fields/wrappers dispatch to. The yaml `graphql:`/`grpc:` blocks carry
	// each surface's address + policy knobs.

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
