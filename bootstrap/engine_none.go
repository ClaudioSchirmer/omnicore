//go:build !postgres && !mysql

package bootstrap

import (
	"context"

	"github.com/ClaudioSchirmer/omnicore/infra/migration"
)

// This file compiles when neither engine build tag is set. No relational engine
// registers, so bootstrap.Build aborts at core.NewEngine ("no relational engine
// registered ... build with the engine's build tag") before these helpers are
// ever reached at runtime. They exist only so the neutral bootstrap code still
// compiles tagless — building with no engine is a configuration error caught at
// boot, not a compile error.

func ensureFuturePartitions(_ context.Context, _ Deps, _ int) error { return nil }

func newMigrator(_ Deps, _ *Config) *migration.Manager { return nil }
