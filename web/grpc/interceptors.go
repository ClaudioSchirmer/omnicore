package grpc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/notifications"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/web/authcore"
)

type appCtxKey struct{}

// AppContextFrom extracts the per-RPC AppContext installed by the
// registry's interceptor chain, with the same safe fallback web.AppContext
// has (tests, handlers invoked outside the chain).
func AppContextFrom(ctx context.Context) *configuration.AppContext {
	if v, ok := ctx.Value(appCtxKey{}).(*configuration.AppContext); ok {
		return v
	}
	return configuration.NewAppContextWithRandomID(configuration.LangENG)
}

// recoveryInterceptor is the outermost safety net: a panic anywhere below
// (interceptors, mappers; pipeline.Dispatch has its own recover) becomes
// INTERNAL without leaking the panic value to the wire.
func (r *Registry) recoveryInterceptor() connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (res connect.AnyResponse, err error) {
			defer func() {
				if p := recover(); p != nil {
					slog.ErrorContext(ctx, "grpc surface panic recovered",
						slog.String("procedure", req.Spec().Procedure),
						slog.String("panic", fmt.Sprintf("%v", p)),
					)
					res, err = nil, errInternal()
				}
			}()
			return next(ctx, req)
		}
	}
}

// appContextInterceptor mirrors web.AppContextMiddleware for the gRPC
// surface: per-RPC AppContext from the request metadata —
//
//	X-Request-ID    → ctx.ID (falls back to the httpclient's `threadID`
//	                  metadata key, then to a fresh UUID)
//	Accept-Language → ctx.Language (default LangENG)
//
// — an optional inbound server span (EnableServerSpanTracing), and the
// cancellation parent: the RPC context already carries the protocol
// deadline (Connect binds the timeout header into ctx), composed with the
// server-side ceiling from SetRequestTimeout when configured. Always echoes
// X-Request-ID on the response for client-side correlation.
func (r *Registry) appContextInterceptor() connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			id := parseRequestID(firstNonEmpty(
				req.Header().Get("X-Request-ID"),
				req.Header().Get("Threadid"),
			))
			lang := parseLanguage(req.Header().Get("Accept-Language"))
			appCtx := configuration.NewAppContext(id, lang)

			var span trace.Span
			if r.traceServerSpan {
				// Continue an upstream trace when a traceparent arrives, else
				// root a new one. Procedure is already low-cardinality.
				carrier := propagation.HeaderCarrier(req.Header())
				parent := otel.GetTextMapPropagator().Extract(ctx, carrier)
				spanCtx, s := otel.Tracer("omnicore/web/grpc").Start(parent, req.Spec().Procedure,
					trace.WithSpanKind(trace.SpanKindServer))
				span = s
				defer span.End()
				ctx = spanCtx
				appCtx.SetTraceContext(spanCtx)
				// CorrelationID == active trace_id, same bridge as the HTTP
				// surface, so logs/traces/integration_events join on one value.
				if sc := span.SpanContext(); sc.IsValid() {
					appCtx.SetCorrelationID(uuid.UUID(sc.TraceID()))
				}
			}

			// Own the cancellation parent: the RPC ctx (deadline included) or
			// its server-side-bounded wrap.
			if r.requestTimeout > 0 {
				tctx, cancel := context.WithTimeout(ctx, r.requestTimeout)
				defer cancel()
				appCtx.SetParent(tctx)
			} else {
				appCtx.SetParent(ctx)
			}

			res, err := next(context.WithValue(ctx, appCtxKey{}, appCtx), req)

			if span != nil && err != nil {
				span.RecordError(err)
				span.SetStatus(codes.Error, connect.CodeOf(err).String())
			}
			if res != nil {
				res.Header().Set("X-Request-ID", id.String())
			} else if err != nil {
				var cerr *connect.Error
				if errors.As(err, &cerr) {
					cerr.Meta().Set("X-Request-ID", id.String())
				}
			}
			return res, err
		}
	}
}

// authInterceptor is the gRPC shell over the shared JWT core
// (web/authcore) — the sibling of web.AuthMiddleware: bearer from the
// `authorization` metadata → Identity on the AppContext, with the same
// notification vocabulary and translated envelope on failure. A no-op
// until EnableAuth arms it (auth.mode=disabled, dev only).
func (r *Registry) authInterceptor() connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			if r.posture != PostureInherit {
				// internal/mtls plane: the trust boundary is the plane; a
				// bearer, when present, is attribution (see posture.go).
				return r.internalAuth(ctx, req, next)
			}
			if r.auth == nil || r.isPublicProcedure(req.Spec().Procedure) {
				return next(ctx, req)
			}
			appCtx := AppContextFrom(ctx)
			identity, token, verr := r.auth.ValidateAuthorization(ctx, req.Header().Get("Authorization"))
			if verr != nil {
				return nil, r.authFailure(appCtx, notificationForFailure(verr.Failure))
			}
			appCtx.SetBearerToken(token)
			appCtx.SetIdentity(identity)
			if r.authPolicy.TenantRequired && identity.TenantID() == "" {
				return nil, r.authFailure(appCtx, notifications.TenantMissingNotification{})
			}
			return next(ctx, req)
		}
	}
}

// authFailure renders the rejection with the same shape as every other
// framework rejection: NotificationContext "Authorization", one
// notification, translated against the request's language — the connect
// sibling of web.respondAuthFailure.
func (r *Registry) authFailure(appCtx *configuration.AppContext, n domain.Notification) *connect.Error {
	nctx := domain.NewNotificationContext("Authorization")
	nctx.AddNotificationMessage(domain.NotificationMessage{Notification: n})
	dtos := notifications.ToContextDTOs(r.pipe.Translator(), appCtx.Language(),
		[]*domain.NotificationContext{nctx})
	return ErrorFromNotifications(dtos)
}

// notificationForFailure maps the core's classification to the canonical
// auth notifications — the same table the Fiber shell dispatches on.
func notificationForFailure(f authcore.Failure) domain.Notification {
	switch f {
	case authcore.FailureMissingToken:
		return notifications.MissingAuthorizationNotification{}
	case authcore.FailureExpiredToken:
		return notifications.ExpiredTokenNotification{}
	default:
		return notifications.InvalidTokenNotification{}
	}
}

// --- small local mirrors of web's private request helpers ---

func parseRequestID(header string) uuid.UUID {
	if header != "" {
		if parsed, err := uuid.Parse(header); err == nil {
			return parsed
		}
	}
	return uuid.New()
}

func parseLanguage(header string) configuration.Language {
	if header == "" {
		return configuration.LangENG
	}
	lower := strings.ToLower(header)
	for _, lang := range configuration.AllLanguages() {
		prefix := strings.ToLower(lang.HTTPPrefix())
		if prefix != "" && strings.HasPrefix(lower, prefix) {
			return lang
		}
	}
	return configuration.LangENG
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
