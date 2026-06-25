package web

import (
	"context"
	"strings"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/trace"
)

const appContextKey = "omnicore.appCtx"

type appContextOptions struct {
	traceServerSpan bool
}

// AppContextOption configures AppContextMiddleware.
type AppContextOption func(*appContextOptions)

// WithServerSpanTracing gates the inbound server span (the tracing `http`
// instrument subsystem). bootstrap passes tracing.Instruments(SubHTTP); left
// off (the default, so existing callers and tests stay unchanged) the middleware
// builds only the AppContext and never starts a server span — the chain is then
// identical to the pre-tracing path. When off, no inbound traceparent is
// extracted and no correlation/trace-id bridge runs; the dispatch span roots the
// trace locally. No-op anyway when no tracer provider is installed.
func WithServerSpanTracing(on bool) AppContextOption {
	return func(o *appContextOptions) { o.traceServerSpan = on }
}

// AppContextMiddleware populates a *configuration.AppContext per request
// from the HTTP headers:
//
//	X-Request-ID    → ctx.ID (generates a new UUID if absent or invalid)
//	Accept-Language → ctx.Language (default LangENG; iterates over Language enum)
//
// Always returns X-Request-ID in the response for client-side correlation.
// Pass WithServerSpanTracing(true) to start the inbound server span (bootstrap
// does so when the tracing `http` instrument is enabled).
//
// Registered automatically by bootstrap.Run. Anyone using bootstrap.Build/Serve
// manually calls it explicitly:
//
//	app.Use(fwweb.AppContextMiddleware())
func AppContextMiddleware(opts ...AppContextOption) fiber.Handler {
	var o appContextOptions
	for _, opt := range opts {
		opt(&o)
	}
	return func(c fiber.Ctx) error {
		var span trace.Span
		var spanCtx context.Context
		if o.traceServerSpan {
			// Inbound server span: continues an upstream trace when a traceparent
			// arrives, else roots a new one.
			spanCtx, span = startServerSpan(c)
			defer span.End()
			c.SetContext(spanCtx)
		}

		id := parseRequestID(c.Get("X-Request-ID"))
		lang := parseLanguage(c.Get("Accept-Language"))
		ctx := configuration.NewAppContext(id, lang)
		if span != nil {
			// The pipeline starts the business span from this context, making it a
			// child of the server span (fiber's Ctx.Value does not delegate to the
			// SetContext'd context, so it cannot ride the parent chain).
			ctx.SetTraceContext(spanCtx)
			// Keep CorrelationID == active trace_id so logs, traces, and
			// integration_events.correlation_id all join on one value. Skipped on
			// the no-op path (invalid span context).
			if sc := span.SpanContext(); sc.IsValid() {
				ctx.SetCorrelationID(uuidFromTraceID(sc.TraceID()))
			}
		}
		c.Locals(appContextKey, ctx)
		c.Set("X-Request-ID", id.String())

		err := c.Next()

		// Rename to the low-cardinality route template now that routing has
		// matched (avoids one span name per concrete id in the collector).
		if span != nil {
			if route := c.Route(); route != nil && route.Path != "" {
				span.SetName(c.Method() + " " + route.Path)
			}
		}
		return err
	}
}

// AppContext extracts the AppContext from the request, with a safe fallback
// if the middleware was bypassed (tests, route outside the middleware tree).
func AppContext(c fiber.Ctx) *configuration.AppContext {
	if v, ok := c.Locals(appContextKey).(*configuration.AppContext); ok {
		return v
	}
	return configuration.NewAppContextWithRandomID(configuration.LangENG)
}

func parseRequestID(header string) uuid.UUID {
	if header != "" {
		if parsed, err := uuid.Parse(header); err == nil {
			return parsed
		}
	}
	return uuid.New()
}

// parseLanguage matches by header prefix against the Language enum name.
// Iterates over all known Language values (including future ones), in
// declaration order. Case-insensitive match, fallback LangENG (English is
// the canonical default — Accept-Language absent or naming a language the
// framework does not ship a catalog for both resolve to LangENG).
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
