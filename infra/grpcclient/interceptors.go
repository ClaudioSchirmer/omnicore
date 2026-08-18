package grpcclient

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"

	"connectrpc.com/connect"
	"go.opentelemetry.io/otel"
	otelcodes "go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/ClaudioSchirmer/omnicore/application/persistence"
	"github.com/ClaudioSchirmer/omnicore/infra/resilience"
)

// bearerCarrier is the minimal interface forward-bearer reads from the
// call context — AppContext implements it natively. Same seam as
// httpclient/auth's forward provider.
type bearerCarrier interface {
	BearerToken() string
}

// correlationInterceptor sends the same correlation metadata the
// httpclient sends: threadID (AppContext.ID), plus X-Request-ID so the
// downstream omnicore gRPC surface adopts the caller's id verbatim — the
// two planes join on one value.
func (s *service) correlationInterceptor() connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			if rc, ok := ctx.(persistence.RequestContext); ok {
				req.Header().Set("Threadid", rc.ID().String())
				req.Header().Set("X-Request-ID", rc.ID().String())
			}
			return next(ctx, req)
		}
	}
}

// tracingInterceptor starts the outbound client span and injects the W3C
// traceparent — outermost of the resilience layers so it times the full
// call including retries, exactly like the httpclient chain position 1.
// No-op (beyond a nil span) when tracing is off or no provider installed.
func (s *service) tracingInterceptor() connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			if !s.tracing {
				return next(ctx, req)
			}
			spanCtx, span := otel.Tracer("omnicore/grpcclient").Start(ctx,
				s.cfg.name+req.Spec().Procedure,
				trace.WithSpanKind(trace.SpanKindClient))
			defer span.End()
			otel.GetTextMapPropagator().Inject(spanCtx, propagation.HeaderCarrier(req.Header()))
			res, err := next(spanCtx, req)
			if err != nil {
				span.RecordError(err)
				span.SetStatus(otelcodes.Error, connect.CodeOf(err).String())
			}
			return res, err
		}
	}
}

// authInterceptor attaches the configured credential to the
// `authorization` metadata: forward re-sends the inbound caller's bearer
// (failing loudly when the AppContext has none — public route, auth
// disabled, background job), static attaches the configured token.
func (s *service) authInterceptor() connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			auth := s.cfg.auth
			if auth == nil {
				return next(ctx, req)
			}
			switch auth.Mode {
			case "forward":
				carrier, ok := ctx.(bearerCarrier)
				if !ok || carrier.BearerToken() == "" {
					return nil, connect.NewError(connect.CodeUnauthenticated,
						fmt.Errorf("grpcclient: service %q: forward auth requires a bearer on the AppContext (public route / auth disabled / background job)", s.cfg.name))
				}
				req.Header().Set("Authorization", "Bearer "+carrier.BearerToken())
			case "static":
				req.Header().Set("Authorization", "Bearer "+auth.Token)
			}
			return next(ctx, req)
		}
	}
}

// idempotencyInterceptor injects the per-call idempotency key. It sits
// OUTSIDE the retry interceptor on purpose: the header set here persists on
// the request across attempts, so every retry carries the SAME key — which
// is what makes retried writes safe against a deduping upstream (the exact
// semantics of the httpclient middleware).
func (s *service) idempotencyInterceptor() connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			idem := s.cfg.idempotency
			if idem == nil || !idem.Enabled {
				return next(ctx, req)
			}
			key, err := resilience.NewIdempotencyKey()
			if err != nil {
				return nil, connect.NewError(connect.CodeInternal,
					fmt.Errorf("grpcclient: generate idempotency key: %w", err))
			}
			req.Header().Set(idem.Header, key)
			return next(ctx, req)
		}
	}
}

// loggingInterceptor emits one slog line per logical call (attempts
// included in the elapsed time), mirroring the httpclient observation
// shape: service, procedure, code, duration.
func (s *service) loggingInterceptor() connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			start := time.Now()
			res, err := next(ctx, req)
			code := "ok"
			if err != nil {
				code = connect.CodeOf(err).String()
			}
			slog.InfoContext(ctx, "grpcclient call",
				slog.String("service", s.cfg.name),
				slog.String("procedure", req.Spec().Procedure),
				slog.String("code", code),
				// Milliseconds, matching the httpclient observation's durationMs;
				// slog renders a bare Duration as unit-less nanoseconds.
				slog.Float64("durationMs", float64(time.Since(start).Nanoseconds())/1e6),
			)
			return res, err
		}
	}
}

// retryInterceptor re-invokes the rest of the chain up to maxAttempts.
// Triggers: the configured connect codes plus transport-level dial errors
// (the sibling of the HTTP chain's network trigger). Sleep between attempts
// is context-aware via resilience.SleepCtx; the deadline the caller set
// keeps ruling — a ctx.Done() during backoff aborts immediately.
func (s *service) retryInterceptor() connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			policy := s.cfg.retry
			if policy.disabled() {
				return next(ctx, req)
			}
			var res connect.AnyResponse
			var err error
			for attempt := 1; ; attempt++ {
				res, err = next(ctx, req)
				if err == nil || attempt >= policy.maxAttempts || !s.shouldRetry(err) {
					return res, err
				}
				if !resilience.SleepCtx(ctx, resilience.Backoff(policy.backoff, attempt)) {
					return res, err
				}
			}
		}
	}
}

// shouldRetry classifies an attempt error against the service's triggers.
func (s *service) shouldRetry(err error) bool {
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	_, retry := s.cfg.retry.retryOn[connect.CodeOf(err)]
	return retry
}

// breakerInterceptor is the innermost resilience layer: every attempt (the
// retry loop re-enters here) consults the per-procedure state machine and
// feeds the outcome back — so attempts against an open circuit are rejected
// without dialing, and a recovering upstream sees a single half-open probe.
func (s *service) breakerInterceptor() connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			breaker := s.breakers.get(req.Spec().Procedure)
			if breaker == nil {
				return next(ctx, req)
			}
			allowed, state := breaker.Allow()
			if !allowed {
				return nil, connect.NewError(connect.CodeUnavailable,
					fmt.Errorf("grpcclient: circuit breaker open for %s%s", s.cfg.name, req.Spec().Procedure))
			}
			_ = state
			res, err := next(ctx, req)
			if err != nil {
				breaker.RecordFailure()
			} else {
				breaker.RecordSuccess()
			}
			return res, err
		}
	}
}

// deadlineInterceptor is the outermost layer: it applies the service's
// default timeout when the caller's ctx carries no earlier deadline —
// the per-service transport-timeout sibling of the httpclient chain.
// Connect turns the ctx deadline into the protocol timeout header, so the
// server observes the same budget.
func (s *service) deadlineInterceptor() connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			if _, has := ctx.Deadline(); !has && s.cfg.timeout > 0 {
				tctx, cancel := context.WithTimeout(ctx, s.cfg.timeout)
				defer cancel()
				ctx = tctx
			}
			return next(ctx, req)
		}
	}
}
