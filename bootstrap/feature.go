package bootstrap

import (
	"errors"
	"fmt"

	"github.com/ClaudioSchirmer/omnicore/infra/db/query"
	"github.com/ClaudioSchirmer/omnicore/infra/integration"
	"github.com/gofiber/fiber/v3"
)

// Feature is a declared capability of the service. Each feature knows how to
// mount its own Fiber routes. Bootstrap calls Mount in the declaration order
// of Wiring.Features, after registering the default middlewares and the
// /health route (provided by the framework).
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

// validateWiring rejects wiring that is structurally incomplete.
//
// Two guards:
//
//  1. At least one Feature OR a BeforeServe hook — /health coming from
//     the framework does not count; the service must do something
//     useful.
//
//  2. At least one translation.Module on Wiring.Translations. The whole
//     stack consumes the Translator: domain notification messages,
//     wire error envelopes (ErrorMessage.Message), audit log strings,
//     and the OpenAPI language dropdown all flow through it. Booting
//     with an empty Translator leaves every translated message blank
//     in production — a silent class of bug that the framework
//     prefers to reject loud at boot.
func validateWiring(w Wiring) error {
	if len(w.Features) == 0 && w.BeforeServe == nil {
		return errors.New("bootstrap: wiring declared no Features and no BeforeServe; nothing to serve")
	}
	if len(w.Translations) == 0 {
		return errors.New("bootstrap: wiring declared no Translations; at least one translation.Module is required (notifications, error envelopes, audit fields all flow through the Translator)")
	}
	return nil
}
