package pipeline

import (
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
	return Run(p, ctx, func() (TRes, error) {
		return h.Handle(ctx, req)
	})
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

func contextNotInitialized[T any](p *Pipeline) Result[T] {
	ctx := domain.NewNotificationContext("Pipeline")
	ctx.AddNotificationMessage(domain.NotificationMessage{
		FieldName:    "context",
		FuncName:     "Run",
		Notification: notifications.ContextNotInitializedNotification{},
	})
	dtos := notifications.ToContextDTOs(p.translator, configuration.LangENG,
		[]*domain.NotificationContext{ctx})
	return Failure[T](dtos)
}

func (p *Pipeline) logFailure(ctx *configuration.AppContext, err error, carrier domain.NotificationCarrier) {
	p.logger.Info("pipeline failure",
		slog.String("threadId", ctx.ID().String()),
		slog.String("errorType", fmt.Sprintf("%T", err)),
		slog.Int("contexts", len(carrier.NotificationContexts())),
	)
}

func (p *Pipeline) logException(ctx *configuration.AppContext, err error) {
	threadID := "nil"
	if ctx != nil {
		threadID = ctx.ID().String()
	}
	p.logger.Error("pipeline exception",
		slog.String("threadId", threadID),
		slog.String("error", err.Error()),
	)
}
