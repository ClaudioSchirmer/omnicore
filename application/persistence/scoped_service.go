package persistence

import (
	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/domain"
)

// ScopedServiceProvider is the service-side mirror of ScopedReaderProvider. A
// domain Service whose implementation runs IO (a uniqueness pre-check, a
// cardinality probe) can implement it so the framework hands it the request ctx
// BEFORE BuildRules runs — closing the gap where a ctx-less service probe would
// otherwise run on context.Background() and ignore http.requestTimeoutSeconds,
// cancellation, and tracing (the same gap ScopedReaderProvider closes for loads).
//
// ScopedService MUST return a per-request VIEW that closes over ctx — a shallow
// copy of the receiver — never mutate the receiver: the wired Service is a
// singleton shared across concurrent requests, so mutating it would be a data
// race. The returned value is still a domain.Service (the pure marker); the
// domain port never pronounces ctx — the binding lives entirely in application +
// infra, exactly as ScopedReaderProvider does for the read port.
//
// Optional capability, probed (never embedded) via ScopeService. A Service that
// does not implement it is passed through unchanged and keeps today's behavior.
type ScopedServiceProvider interface {
	ScopedService(ctx *configuration.AppContext) domain.Service
}

// ScopeService binds the request ctx to svc when it implements
// ScopedServiceProvider; otherwise it returns svc unchanged. nil stays nil, so
// the entity's RequiresService()==nil boot check (domain raises
// ServiceIsRequiredNotification) is preserved verbatim. Mirror of LoadForWrite's
// probe-or-fallback shape — the Auto command handlers call it on their optional
// Service field before passing it into domain.Get*, and a custom (manual)
// command handler that drives domain.Get* itself calls it the same way with the
// request ctx in hand.
func ScopeService(svc domain.Service, ctx *configuration.AppContext) domain.Service {
	if svc == nil {
		return nil
	}
	if sp, ok := svc.(ScopedServiceProvider); ok {
		return sp.ScopedService(ctx)
	}
	return svc
}
