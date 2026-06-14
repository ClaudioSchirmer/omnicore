package web

import "sync/atomic"

// SetAuthorizationEnabled toggles the master switch the permission gate
// consults per request. When disabled (default), the gate becomes a
// no-op — handlers run as if no RequirePermission option had been declared.
// When enabled, the gate enforces Identity.HasPermission as documented.
//
// Called by bootstrap.Run with cfg.Auth.Authorization.Enabled before the
// first request reaches the gate. The default-false zero-value matches the
// design's incremental-rollout pattern: services can annotate routes with
// RequirePermission before flipping the yaml flag — the annotations sit
// inert in the spec until the operator opts in, leaving the spec ahead of
// the runtime so the consumer documentation never drifts.
//
// Identity helpers (HasPermission, TenantID) are unaffected by this flag;
// they consult their own configured claim names regardless. Only the
// per-request short-circuit in the gate is gated by this switch.
func SetAuthorizationEnabled(enabled bool) {
	if enabled {
		authorizationEnabledFlag.Store(1)
	} else {
		authorizationEnabledFlag.Store(0)
	}
}

func authorizationEnabled() bool {
	return authorizationEnabledFlag.Load() != 0
}

// atomic.Int32 keeps the per-request read cheap (no mutex contention) while
// keeping SetAuthorizationEnabled itself concurrent-safe. The flag is set
// once at boot in practice, so contention is theoretical.
var authorizationEnabledFlag atomic.Int32
