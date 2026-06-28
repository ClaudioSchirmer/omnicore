package integration

import (
	"log/slog"
	"sync"

	"github.com/ClaudioSchirmer/omnicore/infra/db"
)

// state is the package-level singleton populated by Configure at boot
// and consumed by Dispatch (the producer entry point) at request time.
// Keeping it package-level mirrors the framework's existing posture for
// set-once-at-boot primitives (fwweb.SetTranslator, openapi.SetGate,
// configuration.SetPermissionsClaim): production services Configure once
// during bootstrap.Run and never mutate again; tests are responsible for
// resetting via Reset() between subtests when they exercise the
// singleton path directly.
//
// Synchronization is a write-once-read-many shape: Configure takes the
// mutex; Dispatch takes the read-lock. The path is hot enough that an
// atomic.Value would be cheaper, but the singleton is read once per
// Dispatch (which itself does PG IO) so the lock cost is irrelevant
// against the network round-trip.
var (
	stateMu sync.RWMutex
	state   *client
)

// client holds the runtime state Dispatch needs: the resolved Config
// (lookups against the publishes block), the relational engine for the
// standalone-INSERT path (when WithTx is omitted), and the framework's
// shared logger.
type client struct {
	cfg    *Config
	eng    db.RelationalEngine
	logger *slog.Logger
}

// Configure wires the integration package's singleton state. Called by
// bootstrap.Run after LoadConfig + buildDeps, and before any Feature
// mounts its routes or receivers. A subsequent Configure call
// overwrites the previous state — useful in tests that swap the YAML
// config between subtests; production never does this.
//
// cfg may be a freshly-parsed Config (canonical) or a synthesized one
// (tests). eng may be nil when the service emits no integration events
// AND does not need the standalone-INSERT path — a standalone Dispatch
// then fails loudly, which is the desired behavior. logger falls back to
// slog.Default() when nil. The engine is the neutral RelationalEngine, so
// the producer works on any backend (Postgres or MySQL).
func Configure(cfg *Config, eng db.RelationalEngine, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	stateMu.Lock()
	defer stateMu.Unlock()
	state = &client{cfg: cfg, eng: eng, logger: logger}
}

// Reset clears the singleton — only used in tests. Production has no
// reason to reset; a second Configure call already overwrites.
func Reset() {
	stateMu.Lock()
	defer stateMu.Unlock()
	state = nil
}

// snapshot returns the current state under the read-lock. Callers MUST
// nil-check the result: Configure may not have run, especially in test
// scaffolds.
func snapshot() *client {
	stateMu.RLock()
	defer stateMu.RUnlock()
	return state
}
