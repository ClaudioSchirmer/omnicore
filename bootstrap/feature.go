package bootstrap

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/ClaudioSchirmer/omnicore/infra/db/query"
	"github.com/ClaudioSchirmer/omnicore/infra/integration"
	"github.com/ClaudioSchirmer/omnicore/web/graphql"
	fwgrpc "github.com/ClaudioSchirmer/omnicore/web/grpc"
	"github.com/gofiber/fiber/v3"
)

// Feature is a declared capability of the service. Each feature knows how to
// mount its own Fiber routes. Bootstrap calls Mount in the declaration order
// of Wiring.Features, after registering the default middlewares and the
// /livez + /readyz probe routes (provided by the framework).
//
// Features with a read side (that contribute ViewDefinitions to SyncEngine)
// also implement ReadableFeature.
type Feature interface {
	Mount(app *fiber.App, deps Deps)
}

// ReadableFeature is the opt-in for the read side. Bootstrap collects all the
// Views() declared by the ReadableFeatures and passes them to SyncEngine.
// Returning a slice (not a single view) covers the case of one feature
// exposing multiple projections (e.g. users full + users_summary denormalized
// for listing).
type ReadableFeature interface {
	Feature
	Views() []*query.ViewDefinition
}

// IntegrationFeature is the opt-in for the consumer side of the
// cross-service async-messaging surface. Bootstrap calls
// MountReceivers(reg, deps) on every feature that satisfies the
// interface after Phase HTTP (Mount) finishes, BEFORE the
// IntegrationConsumerPool starts. The receiver registry passed in is
// the same one Deps.IntegrationRegistry exposes, so the consumer
// service can register receivers via reg.From("...").On("...", ...)
// from inside the feature.
//
// Mirrors the role ReadableFeature plays for the read side: opt-in
// via type assertion. Features that emit integration events but do
// NOT consume any pay zero cost — fwintegration.Dispatch reads the
// publishes block straight from the package singleton; no per-Feature
// hook is required on the producer side.
type IntegrationFeature interface {
	Feature
	MountReceivers(reg *integration.Registry, deps Deps)
}

// GraphQLFeature is the opt-in for the GraphQL surface — the query/mutation
// twin of Mount. Bootstrap creates the single graphql.Registry (one POST
// /graphql surface) and calls MountGraphQL on every feature that satisfies
// this interface, so each feature contributes its fields cumulatively into the
// same graph, reusing the same repo/service/view it already holds. The registry
// is framework-owned (surfaced on Deps.GraphQLRegistry), exactly like the
// OpenAPI and Integration registries — the feature never constructs it.
//
// Opt-in by type assertion, mirroring ReadableFeature/IntegrationFeature: a
// feature declaring this method makes the surface exist; no feature declaring
// it leaves POST /graphql unmounted. There is no yaml/Wiring enable-flag — the
// declaration IS the switch. The yaml `graphql:` block carries only the
// surface's address/knobs (path, introspection).
type GraphQLFeature interface {
	Feature
	MountGraphQL(reg *graphql.Registry, deps Deps)
}

// GRPCFeature is the opt-in for the gRPC surface — the same pattern as
// GraphQLFeature, one dedicated-listener grpc.Registry the framework owns
// (Deps.GRPCRegistry). Each feature registers its generated Connect service
// handlers cumulatively via MountGRPC; a feature declaring the method lights up
// the surface, none declaring it leaves it unmounted. The yaml `grpc:` block
// carries the listener + policy knobs; the declaration is the on/off switch.
type GRPCFeature interface {
	Feature
	MountGRPC(reg *fwgrpc.Registry, deps Deps)
}

// collectViews iterates features, aggregates views from those that implement
// ReadableFeature, and rejects view name collisions between features
// (same Mongo collection declared by more than one aggregate).
// buildViewMaxLimitResolver returns the closure MongoViewReader consults at
// every ReadPage to honor the framework's per-view max-limit cascade. The
// closure captures a snapshot of the per-view overrides + the yaml-supplied
// default; the framework constant fallback (100) lives in the reader itself
// so a returned 0 here signals "delegate to the framework default".
func buildViewMaxLimitResolver(views []*query.ViewDefinition, yamlDefault int64) func(view string) int64 {
	overrides := make(map[string]int64, len(views))
	for _, v := range views {
		if n := v.MaxLimitValue(); n > 0 {
			overrides[v.Name()] = n
		}
	}
	return func(view string) int64 {
		if n, ok := overrides[view]; ok {
			return n
		}
		return yamlDefault
	}
}

func collectViews(features []Feature) ([]*query.ViewDefinition, error) {
	var views []*query.ViewDefinition
	seen := make(map[string]string) // viewName -> first owner ("%T")
	for _, f := range features {
		rf, ok := f.(ReadableFeature)
		if !ok {
			continue
		}
		for _, v := range rf.Views() {
			if prev, dup := seen[v.Name()]; dup {
				return nil, fmt.Errorf(
					"bootstrap: view name collision: %q declared by %s and %T",
					v.Name(), prev, f,
				)
			}
			seen[v.Name()] = fmt.Sprintf("%T", f)
			views = append(views, v)
		}
	}
	return views, nil
}

// mountSurfaceFeatures builds the single framework-owned GraphQL / gRPC
// registry (on Deps) from the features that opt into GraphQLFeature /
// GRPCFeature, then lets each such feature contribute its fields/services
// cumulatively — the discovery twin of collectViews for the two web surfaces.
//
// Idempotent per surface: a non-nil registry means it was already built and
// populated (serve builds both BEFORE buildApp so the dedicated gRPC listener
// serves the SAME object buildApp configured), so the second call skips. When
// no feature opts into a surface its registry stays nil and bootstrap serves
// nothing for it — the interface declaration IS the on/off switch, exactly like
// ReadableFeature/IntegrationFeature. No yaml/Wiring enable-flag.
func mountSurfaceFeatures(deps *Deps, wiring Wiring) error {
	if deps.GraphQLRegistry == nil {
		for _, f := range wiring.Features {
			gf, ok := f.(GraphQLFeature)
			if !ok {
				continue
			}
			if deps.GraphQLRegistry == nil {
				deps.GraphQLRegistry = graphql.New(deps.Pipeline)
			}
			gf.MountGraphQL(deps.GraphQLRegistry, *deps)
		}
		// Build the schema once, at boot: a cross-feature field-name collision
		// (two features registering the same root Query/Mutation field) fails the
		// schema load, and building here turns that into an ACTIONABLE BOOT ABORT
		// instead of a lazy "schema build failed" on every GraphQL request. This
		// mirrors collectViews' boot-time duplicate-view check and the gRPC
		// listener's duplicate-procedure panic. The schema is cached, so Execute
		// does no extra work.
		if deps.GraphQLRegistry != nil {
			if _, err := deps.GraphQLRegistry.SDL(); err != nil {
				return fmt.Errorf("bootstrap: GraphQL schema failed to build — likely a field-name collision across features: %w", err)
			}
		}
	}
	if deps.GRPCRegistry == nil {
		for _, f := range wiring.Features {
			rf, ok := f.(GRPCFeature)
			if !ok {
				continue
			}
			if deps.GRPCRegistry == nil {
				deps.GRPCRegistry = fwgrpc.New(deps.Pipeline)
			}
			rf.MountGRPC(deps.GRPCRegistry, *deps)
		}
	}
	return nil
}

// validateWiring rejects wiring that is structurally incomplete.
//
// Two guards:
//
//  1. At least one Feature OR a BeforeServe hook — the /livez + /readyz
//     probes coming from the framework do not count; the service must do
//     something useful.
//
//  2. At least one translation.Module on Wiring.Translations. The whole
//     stack consumes the Translator: domain notification messages,
//     wire error envelopes (ErrorMessage.Message), audit log strings,
//     and the OpenAPI language dropdown all flow through it. Booting
//     with an empty Translator leaves every translated message blank
//     in production — a silent class of bug that the framework
//     prefers to reject loud at boot.
//
// Dev-only exception — the EMPTY SHELL: under APP_PROFILE=dev a wiring with
// no Features and no BeforeServe is accepted and boots serving only the
// framework surfaces (probes; OpenAPI/GraphQL when wired). This is the
// legitimate state of a freshly scaffolded service — it lets the environment
// (config, connections, migrations, CDC relay) be proven live before the
// first entity exists. The translations guard is waived only inside that
// shell (no feature exists to produce a translatable message); a wiring WITH
// features still requires at least one translation.Module, dev included.
// Every other profile rejects the empty wiring exactly as before.
func validateWiring(w Wiring, dev bool) error {
	if len(w.Features) == 0 && w.BeforeServe == nil {
		if dev {
			slog.Warn("bootstrap: wiring declared no Features and no BeforeServe — dev-only empty-shell boot; serving framework surfaces only (any other profile aborts on this wiring)")
			return nil
		}
		return errors.New("bootstrap: wiring declared no Features and no BeforeServe; nothing to serve (only a dev-profile boot accepts an empty shell)")
	}
	if len(w.Translations) == 0 {
		return errors.New("bootstrap: wiring declared no Translations; at least one translation.Module is required (notifications, error envelopes, audit fields all flow through the Translator)")
	}
	return nil
}
