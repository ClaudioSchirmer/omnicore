package bootstrap

import (
	"context"
	"fmt"

	"github.com/ClaudioSchirmer/omnicore/infra/migration"
)

// engineBoot bundles the dialect-bound boot steps an engine binding registers:
// the migration-runner factory and (when the backend needs one) the audit
// partition maintainer. Each engine's binding file (engine_<dialect>.go, gated
// on that engine's build tag alone) registers its bundle in init() — the same
// self-registration pattern as core.RegisterEngine, and for the same reason:
// with three engines, mutually-exclusive build-tag twins stop composing
// (`postgres && !mysql` also matches a postgres+sqlserver build, so twin files
// collide on symbol names), while a registry keyed on the runtime
// relational.dialect scales to any tag combination — a build links exactly the
// bindings its tags select, and boot dispatches on the configured dialect.
type engineBoot struct {
	// newMigrator builds the dialect's migration runner. Never nil in a
	// registered bundle.
	newMigrator func(deps Deps, cfg *Config) *migration.Manager
	// ensureFuturePartitions provisions upcoming audit_events partitions. nil
	// when the backend's audit table is not partitioned (MySQL, SQL Server,
	// Oracle) — partition maintenance is a Postgres-only concern.
	ensureFuturePartitions func(ctx context.Context, deps Deps, n int) error
}

// engineBoots is the dialect → boot-steps registry. Populated by the tag-gated
// engine_<dialect>.go init()s; read by the neutral resolvers below. A build
// with no engine tag leaves it empty — boot then aborts earlier, at
// core.NewEngine ("no relational engine registered"), so the resolvers are
// never reached with an unregistered dialect at runtime.
var engineBoots = map[string]engineBoot{}

// registerEngineBoot records a dialect's boot steps. Called from an engine
// binding's init(); a duplicate registration is a build-time bug, surfaced
// loudly.
func registerEngineBoot(dialect string, b engineBoot) {
	if _, dup := engineBoots[dialect]; dup {
		panic(fmt.Sprintf("bootstrap: engine boot steps for dialect %q already registered", dialect))
	}
	engineBoots[dialect] = b
}

// newMigrator resolves the configured dialect's migration runner. A nil return
// means no binding is registered for the dialect — reachable only in a
// misconfigured build (core.NewEngine would already have aborted boot for the
// data path); applyMigrations guards it with a clear error.
func newMigrator(deps Deps, cfg *Config) *migration.Manager {
	b, ok := engineBoots[cfg.Relational.Dialect]
	if !ok {
		return nil
	}
	return b.newMigrator(deps, cfg)
}

// ensureFuturePartitions runs the configured dialect's audit-partition
// maintenance; dialects whose audit table is not range-partitioned register no
// maintainer and no-op here.
func ensureFuturePartitions(ctx context.Context, deps Deps, cfg *Config, n int) error {
	b, ok := engineBoots[cfg.Relational.Dialect]
	if !ok || b.ensureFuturePartitions == nil {
		return nil
	}
	return b.ensureFuturePartitions(ctx, deps, n)
}
