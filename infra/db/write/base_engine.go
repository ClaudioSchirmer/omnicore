package write

import (
	"context"
	"log/slog"

	"github.com/ClaudioSchirmer/omnicore/application/persistence"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/audit"
	"github.com/ClaudioSchirmer/omnicore/infra/events"
)

// BaseEngine holds the cross-engine write-path state (audit config + logger +
// audit-claims allowlist + domain-event publisher) and the backend-neutral
// orchestration that surrounds every framework write: the in-TX audit row, the
// post-commit slog echo + domain-event publish, and the lifecycle-hook dispatch
// at TX positions A and D. Both relational engines (pg.Postgres, mysql.Engine)
// embed it and supply only the dialect-bound parts (Begin, the data writes, the
// Dialect, the Querier, the rebuild lock, Close).
//
// The audit/hook logic is written ONCE here against the neutral db.WriteTx /
// db.Tx surfaces + the engine's Dialect; an engine wraps its live driver tx
// (pgx.Tx / *sql.Tx) into the neutral surface at the call site. Zero-value
// BaseEngine = audit fully disabled + no publisher, which is correct for tests
// and fixtures that construct an engine directly without WithAudit.
type BaseEngine struct {
	beginner    WriteBeginner
	auditCfg    *audit.Config
	logger      *slog.Logger
	auditClaims []string
	publisher   events.Publisher
}

// SetBeginner wires the back-reference the embedded base uses to open a
// framework-owned write TX (the concrete engine is its own WriteBeginner). Each
// engine calls it once at construction (e.g. p.SetBeginner(p)); the write
// orchestration then opens transactions through it without naming a driver.
func (b *BaseEngine) SetBeginner(wb WriteBeginner) { b.beginner = wb }

// HookContext describes the slot the persister is about to fire — consumed only
// by the observability slog.Warn emitted on a hook error (verb, hookSlot,
// entityType, threadId, error). Engines build it per write verb.
type HookContext struct {
	Verb       string
	EntityType string
}

// AuditBundle carries the pre-built audit event + the entity's domain events
// from a write verb through to the post-commit side effects, so a verb builds
// the event once (in-TX) and fires echo/publish after COMMIT from the same
// value. A zero bundle (nil Ev, nil Evs) is inert.
type AuditBundle struct {
	Ev  *audit.AuditEvent
	Evs []domain.DomainEvent
}

// ConfigureAudit sets the audit surface — the shared body of each engine's
// WithAudit. nil cfg disables audit entirely (no in-TX row, no echo); a Config
// with destinations: [] yields the same posture (empty list = off). nil logger
// falls back to slog.Default() in the echo + hook-error paths.
func (b *BaseEngine) ConfigureAudit(cfg *audit.Config, logger *slog.Logger, auditClaims []string) {
	b.auditCfg = cfg
	b.logger = logger
	b.auditClaims = auditClaims
}

// SetPublisher wires the domain-event transport — the shared body of each
// engine's WithEventPublisher. A nil publisher disables publishing (events are
// still carried on the ValidEntity, simply not forwarded).
func (b *BaseEngine) SetPublisher(pub events.Publisher) { b.publisher = pub }

// AuditClaims is the JWT-claims allowlist surfaced on the audit actorClaims
// block; the engine threads it into the Build*Event helpers.
func (b *BaseEngine) AuditClaims() []string { return b.auditClaims }

// AuditEnabled reports whether any audit destination is active — the gate a
// write verb checks before building the event.
func (b *BaseEngine) AuditEnabled() bool {
	return b.auditCfg != nil &&
		(b.auditCfg.Includes(audit.DestinationSlog) || b.auditCfg.Includes(audit.DestinationDatabase))
}

// BuildAudit returns a bundle for a write verb: the event is built (via the
// supplied dialect-neutral builder) only when audit is enabled; the entity's
// domain events ride along for post-commit publishing regardless.
func (b *BaseEngine) BuildAudit(build func() audit.AuditEvent, evs []domain.DomainEvent) AuditBundle {
	var ev *audit.AuditEvent
	if b.AuditEnabled() {
		built := build()
		ev = &built
	}
	return AuditBundle{Ev: ev, Evs: evs}
}

// WriteAuditRow performs the in-TX INSERT into audit_events when the database
// destination is active. The placeholders are rendered through the tx's Dialect
// ($n on Postgres, ? on MySQL); the row id is Go-generated inside the audit
// package. No-op when audit is off or the event was never built.
func (b *BaseEngine) WriteAuditRow(ctx context.Context, tx Tx, ev *audit.AuditEvent) error {
	if ev == nil || b.auditCfg == nil || !b.auditCfg.Includes(audit.DestinationDatabase) {
		return nil
	}
	return audit.InsertAuditEvent(ctx, tx, tx.Dialect().Placeholder, *ev)
}

// EchoAuditSlog emits the post-commit slog audit line when the slog destination
// is active. No-op when audit is off or the event was never built.
func (b *BaseEngine) EchoAuditSlog(ctx persistence.RequestContext, ev *audit.AuditEvent) {
	if ev == nil || b.auditCfg == nil || !b.auditCfg.Includes(audit.DestinationSlog) {
		return
	}
	audit.EchoSlog(ctx, b.logger, *ev)
}

// PublishEvents forwards the entity's accumulated domain events post-commit,
// best-effort: a transport error is logged at Warn and swallowed so a failing
// publisher never affects the already-committed write. No-op when no publisher
// is wired or no events were registered.
func (b *BaseEngine) PublishEvents(ctx persistence.RequestContext, evs []domain.DomainEvent) {
	if b.publisher == nil || len(evs) == 0 {
		return
	}
	if err := b.publisher.PublishAll(ctx, evs); err != nil {
		b.log().Warn("event.publish.error", "err", err)
	}
}

// AfterCommit fires the post-commit, best-effort side effects of a bundle: the
// slog audit echo + domain-event publish. The single post-commit position both
// engines call once per write.
func (b *BaseEngine) AfterCommit(ctx persistence.RequestContext, ab AuditBundle) {
	b.EchoAuditSlog(ctx, ab.Ev)
	b.PublishEvents(ctx, ab.Evs)
}

// FireAfterBegin runs the AfterBegin hook (when configured) inside the open TX,
// before any framework write — position A. The hook receives the sealed handle
// the tx carries (tx.Handle()), so the dispatch names no driver type. On hook
// error it emits the best-effort persistence.hook.error line and returns the
// error verbatim so the caller rolls back and propagates the NotificationCarrier.
func (b *BaseEngine) FireAfterBegin(ctx persistence.RequestContext, tx WriteTx, src domain.Entity, hook WriteHook, hctx HookContext) error {
	if hook.AfterBegin == nil {
		return nil
	}
	if err := hook.AfterBegin(ctx, src, tx.Handle()); err != nil {
		b.logHookError(ctx, hctx, "afterBegin", err)
		return err
	}
	return nil
}

// FireBeforeCommit runs the BeforeCommit hook (when configured) inside the open
// TX, after all framework writes and before COMMIT — position D. Mirrors
// FireAfterBegin's error + observability shape.
func (b *BaseEngine) FireBeforeCommit(ctx persistence.RequestContext, tx WriteTx, src domain.Entity, id domain.ID, hook WriteHook, hctx HookContext) error {
	if hook.BeforeCommit == nil {
		return nil
	}
	if err := hook.BeforeCommit(ctx, src, id, tx.Handle()); err != nil {
		b.logHookError(ctx, hctx, "beforeCommit", err)
		return err
	}
	return nil
}

// logHookError emits the observability line. Best-effort: a nil logger falls
// back to slog.Default so the line still lands even when boot skipped the logger.
func (b *BaseEngine) logHookError(ctx persistence.RequestContext, hctx HookContext, slot string, err error) {
	b.log().Warn("persistence.hook.error",
		"verb", hctx.Verb,
		"hookSlot", slot,
		"entityType", hctx.EntityType,
		"threadId", ctx.ID().String(),
		"error", err.Error(),
	)
}

// log returns the configured logger or slog.Default() when none was wired.
func (b *BaseEngine) log() *slog.Logger {
	if b.logger == nil {
		return slog.Default()
	}
	return b.logger
}
