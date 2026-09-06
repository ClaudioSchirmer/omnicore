package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/notifications"
	"github.com/ClaudioSchirmer/omnicore/application/translation"
	"github.com/ClaudioSchirmer/omnicore/domain"
)

type Pipeline struct {
	translator *translation.Translator
	logger     *slog.Logger
}

func New(translator *translation.Translator) *Pipeline {
	if translator == nil {
		translator = translation.Default()
	}
	return &Pipeline{
		translator: translator,
		logger:     slog.Default(),
	}
}

func (p *Pipeline) WithLogger(l *slog.Logger) *Pipeline {
	if l != nil {
		p.logger = l
	}
	return p
}

func (p *Pipeline) Translator() *translation.Translator {
	return p.translator
}

func Run[T any](p *Pipeline, ctx *configuration.AppContext, fn func() (T, error)) (result Result[T]) {
	if ctx == nil {
		return contextNotInitialized[T](p)
	}

	defer func() {
		if r := recover(); r != nil {
			err := fmt.Errorf("panic recovered: %v", r)
			p.logException(ctx, err)
			result = Exception[T](err)
		}
	}()

	v, err := fn()
	if err == nil {
		return Success(v)
	}

	// A blown request deadline (http.requestTimeoutSeconds) reaches here as
	// context.DeadlineExceeded from pgx/mongo/httpclient. Map it to a 504 before
	// the carrier/exception branches so a timeout never masquerades as a 500.
	if errors.Is(err, context.DeadlineExceeded) {
		return requestTimedOut[T](p, ctx)
	}

	var carrier domain.NotificationCarrier
	if errors.As(err, &carrier) {
		p.logFailure(ctx, err, carrier)
		dtos := notifications.ToContextDTOs(p.translator, ctx.Language(), carrier.NotificationContexts())
		return Failure[T](dtos)
	}

	p.logException(ctx, err)
	return Exception[T](err)
}

func Dispatch[TReq Request, TRes any](
	p *Pipeline,
	ctx *configuration.AppContext,
	req TReq,
	h Handler[TReq, TRes],
) Result[TRes] {
	if ctx == nil {
		// Run reports the missing-context failure; no span without a context to
		// hang it on.
		return Run(p, ctx, func() (TRes, error) {
			return h.Handle(ctx, req)
		})
	}

	span, end := beginDispatchSpan(ctx, req)
	defer end()

	result := Run(p, ctx, func() (TRes, error) {
		return h.Handle(ctx, req)
	})
	recordDispatchOutcome(span, result.Err())
	return result
}

func DispatchAll[TReq Request, TRes any](
	p *Pipeline,
	ctx *configuration.AppContext,
	req TReq,
	handlers []Handler[TReq, TRes],
) []Result[TRes] {
	out := make([]Result[TRes], 0, len(handlers))
	for _, h := range handlers {
		out = append(out, Dispatch(p, ctx, req, h))
	}
	return out
}

func requestTimedOut[T any](p *Pipeline, ctx *configuration.AppContext) Result[T] {
	p.logger.WarnContext(ctx, "pipeline request timeout",
		slog.String("threadId", ctx.ID().String()),
	)
	nctx := domain.NewNotificationContext("Request")
	nctx.AddNotificationMessage(domain.NotificationMessage{
		Override:     "request",
		Notification: notifications.RequestTimeoutNotification{},
	})
	dtos := notifications.ToContextDTOs(p.translator, ctx.Language(),
		[]*domain.NotificationContext{nctx})
	return Failure[T](dtos)
}

func contextNotInitialized[T any](p *Pipeline) Result[T] {
	ctx := domain.NewNotificationContext("Pipeline")
	ctx.AddNotificationMessage(domain.NotificationMessage{
		Override:     "context",
		FuncName:     "Run",
		Notification: notifications.ContextNotInitializedNotification{},
	})
	dtos := notifications.ToContextDTOs(p.translator, configuration.LangENG,
		[]*domain.NotificationContext{ctx})
	return Failure[T](dtos)
}

func (p *Pipeline) logFailure(ctx *configuration.AppContext, err error, carrier domain.NotificationCarrier) {
	// InfoContext so the tracing slog handler stamps traceId/spanId from the
	// dispatch span the AppContext carries during Run.
	p.logger.InfoContext(ctx, "pipeline failure",
		slog.String("threadId", ctx.ID().String()),
		slog.String("errorType", fmt.Sprintf("%T", err)),
		slog.Int("contexts", len(carrier.NotificationContexts())),
	)
}

func (p *Pipeline) logException(ctx *configuration.AppContext, err error) {
	threadID := "nil"
	logCtx := context.Background()
	if ctx != nil {
		threadID = ctx.ID().String()
		logCtx = ctx
	}
	p.logger.ErrorContext(logCtx, "pipeline exception",
		slog.String("threadId", threadID),
		slog.String("error", err.Error()),
	)
}
