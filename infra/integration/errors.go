package integration

import "errors"

// ErrIntegrationEventNotConfigured is returned by Dispatch when the
// supplied eventKey does not resolve to an entry under
// `integration.publishes.events.<eventKey>` in the loaded YAML. The
// framework intentionally surfaces this lazily — at the first Dispatch
// call site — rather than aborting boot, matching the posture httpclient
// adopts for unknown service/endpoint references: an empty publishes
// block is a valid steady state for services that only consume
// integration events, and a typo on the producer side should fail at the
// call site with a clear diagnostic rather than break the boot of an
// otherwise-functional service.
//
// Use errors.Is(err, ErrIntegrationEventNotConfigured) to branch in
// tests. The sentinel travels verbatim through the application's hook
// closure so a non-NotificationCarrier error becomes Result.Exception →
// 500 on the wire — surfaces in slog as a programming bug, not a domain
// validation rejection.
var ErrIntegrationEventNotConfigured = errors.New("integration: eventKey not configured under integration.publishes.events")

// ErrIntegrationConfigNotInitialized is returned by Dispatch when the
// process has not called Configure yet. Typically only seen in tests
// that exercise the singleton path without bootstrap; production
// services pay this once at the first Dispatch after Configure ran.
var ErrIntegrationConfigNotInitialized = errors.New("integration: package not configured — call integration.Configure before Dispatch")

// ErrIntegrationAggregateIDRequired is returned by Dispatch when the
// loaded YAML declares an `aggregate:` field for the eventKey but the
// caller did not supply WithAggregateID(id). Aggregates without an id
// would land as NULL on aggregate_id while aggregate_type is populated —
// breaks the forensic timeline index. The framework refuses to write
// the inconsistent row.
var ErrIntegrationAggregateIDRequired = errors.New("integration: eventKey declares aggregate type — WithAggregateID(id) required")
