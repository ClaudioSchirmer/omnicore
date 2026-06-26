package audit

import (
	"context"
	"log/slog"

	"github.com/ClaudioSchirmer/omnicore/application/persistence"
)

// EchoSlog emits ev as a structured slog line at LevelInfo with msg="audit".
// Every field of AuditEvent lands as a top-level slog.Attr so log
// aggregators index without parsing a nested envelope; omitempty fields are
// skipped when zero-valued.
//
// Best-effort: any slog handler error is swallowed (slog itself is the
// observability layer; making it part of the write-path failure surface
// would defeat its purpose). When the database destination is also active,
// the audit_events row remains the source of truth.
//
// logger may be nil — falls back to slog.Default().
func EchoSlog(ctx persistence.RequestContext, logger *slog.Logger, ev AuditEvent) {
	if logger == nil {
		logger = slog.Default()
	}
	attrs := []slog.Attr{
		slog.String("threadId", ev.ThreadID),
		slog.String("entityType", ev.EntityType),
		slog.String("entityId", ev.EntityID),
		slog.String("verb", ev.Verb),
		slog.String("actionName", ev.ActionName),
		slog.String("kind", ev.Kind),
		slog.String("actor", ev.Actor),
		slog.Time("dateTime", ev.DateTime),
	}
	if ev.TraceID != "" {
		attrs = append(attrs, slog.String("traceId", ev.TraceID))
	}
	if ev.ActorIssuer != "" {
		attrs = append(attrs, slog.String("actorIssuer", ev.ActorIssuer))
	}
	if len(ev.ActorClaims) > 0 {
		attrs = append(attrs, slog.Any("actorClaims", ev.ActorClaims))
	}
	if ev.TenantID != "" {
		attrs = append(attrs, slog.String("tenantId", ev.TenantID))
	}
	if ev.Snapshot != nil {
		attrs = append(attrs, slog.Any("snapshot", ev.Snapshot))
	}
	if len(ev.Changes) > 0 {
		attrs = append(attrs, slog.Any("changes", ev.Changes))
	}
	if len(ev.Children) > 0 {
		attrs = append(attrs, slog.Any("children", ev.Children))
	}
	// Use context.Background() — slog's ctx parameter is for distributed-trace
	// propagation through Handler, but the audit line is request-scoped (we
	// carry threadId on the attr); detaching ctx avoids surprising trace
	// merges when audit fires post-commit and the request ctx may already be
	// canceled.
	logger.LogAttrs(context.Background(), slog.LevelInfo, "audit", attrs...)
}
