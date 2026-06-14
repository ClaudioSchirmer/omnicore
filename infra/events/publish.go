package events

import (
	"context"
	"log/slog"
	"time"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// Publisher emits domain events accumulated on entities through whatever
// transport the consumer wires (slog by default; see SlogPublisher). Used
// by the auditor's PublishAll to forward Events() after each successful
// write — distinct from the audit line itself (which carries snapshot/
// changes/transition semantics).
type Publisher interface {
	Publish(ctx domain.Context, event domain.Event) error
	PublishAll(ctx domain.Context, events []domain.DomainEvent) error
}

// SlogPublisher writes each domain event as a flat slog line with top-level
// attrs (threadId, entityType, eventType, actor, dateTime, message, values,
// exception). Vocabulary aligned with audit.AuditEvent so log queries match
// across both surfaces.
type SlogPublisher struct {
	logger *slog.Logger
}

// NewSlogPublisher builds the publisher; a nil logger falls back to
// slog.Default(). The slog handler's existing JSON formatting (level, time,
// msg) is preserved — only the event payload changes shape.
func NewSlogPublisher(logger *slog.Logger) *SlogPublisher {
	if logger == nil {
		logger = slog.Default()
	}
	return &SlogPublisher{logger: logger}
}

func (p *SlogPublisher) Publish(ctx domain.Context, event domain.Event) error {
	ev := EventLog{
		ThreadID:    ctx.ID().String(),
		EntityType:  event.ClassName(),
		EventType:   event.EventType().String(),
		Actor:       ctx.ActorSubject(),
		ActorIssuer: ctx.ActorIssuer(),
		DateTime:    time.Now().UTC(),
		Message:     event.Message(),
		Values:      event.Values(),
		Exception:   errString(event.Err()),
	}

	attrs := []slog.Attr{
		slog.String("threadId", ev.ThreadID),
		slog.String("eventType", ev.EventType),
		slog.String("actor", ev.Actor),
		slog.Time("dateTime", ev.DateTime),
	}
	if ev.EntityType != "" {
		attrs = append(attrs, slog.String("entityType", ev.EntityType))
	}
	if ev.ActorIssuer != "" {
		attrs = append(attrs, slog.String("actorIssuer", ev.ActorIssuer))
	}
	if ev.Message != "" {
		attrs = append(attrs, slog.String("message", ev.Message))
	}
	if ev.Values != nil {
		attrs = append(attrs, slog.Any("values", ev.Values))
	}
	if ev.Exception != "" {
		attrs = append(attrs, slog.String("exception", ev.Exception))
	}
	p.logger.LogAttrs(context.Background(), levelFor(event.EventType()), "event", attrs...)
	return nil
}

func (p *SlogPublisher) PublishAll(ctx domain.Context, events []domain.DomainEvent) error {
	for _, e := range events {
		if err := p.Publish(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

func levelFor(t domain.EventType) slog.Level {
	switch t {
	case domain.EventError:
		return slog.LevelError
	case domain.EventWarning:
		return slog.LevelWarn
	case domain.EventDebug:
		return slog.LevelDebug
	default:
		return slog.LevelInfo
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
