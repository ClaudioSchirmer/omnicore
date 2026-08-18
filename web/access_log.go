package web

import (
	"context"
	"log/slog"
	"time"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/gofiber/fiber/v3"
)

// accessLogMessage is the inbound sibling of the httpclient observation's
// "http.outbound" record — one line per served request, same vocabulary.
const accessLogMessage = "http.inbound"

// AccessLog is the framework's inbound request log: ONE structured record per
// request, emitted through the service's slog.Logger.
//
// It replaces Fiber's logger middleware, whose plaintext template was wrong for
// this stack in three ways:
//
//   - Format. Every other line the framework emits is JSON through slog; the
//     plaintext access line broke ingestion for the one record an operator
//     greps most.
//   - Timestamp. Fiber's template renders a timestamp cached by a background
//     ticker, so an access line could be a second behind the structured record
//     describing the SAME request — the two looked like different events.
//   - Correlation. It carried no threadId, so the access line and the
//     "pipeline failure" record for one request could not be joined, and the
//     tracing slog handler had no context to stamp traceId/spanId from.
//
// The middleware sits OUTERMOST — before Recover and AppContextMiddleware — so
// nothing escapes it: the recover middleware turns a panic into a returned
// error, which arrives here as the chain error like any other. It still reads
// the AppContext AFTER c.Next() returns, by which point the inner middleware
// has stored it; that is where threadId and the trace context come from. A
// request that never reached AppContextMiddleware (an unrouted path, a panic in
// an earlier middleware) simply logs without them rather than inventing an id
// that appears in no other record.
//
// Like Fiber's logger, it invokes the app's ErrorHandler itself when the chain
// returns an error, so the status it records is the FINAL status sent to the
// client, not the pre-error-handler 200.
func AccessLog(logger *slog.Logger) fiber.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return func(c fiber.Ctx) error {
		start := time.Now()

		chainErr := c.Next()
		if chainErr != nil {
			if err := c.App().ErrorHandler(c, chainErr); err != nil {
				_ = c.SendStatus(fiber.StatusInternalServerError)
			}
		}
		elapsed := time.Since(start)

		// The AppContext doubles as the log context: the tracing slog handler
		// stamps traceId/spanId from it, joining this line to the request's
		// span exactly like pipeline failures.
		logCtx := context.Background()
		attrs := make([]slog.Attr, 0, 8)
		if appCtx, ok := c.Locals(appContextKey).(*configuration.AppContext); ok {
			logCtx = appCtx
			attrs = append(attrs, slog.String("threadId", appCtx.ID().String()))
		}

		status := c.Response().StatusCode()
		attrs = append(attrs,
			slog.String("method", c.Method()),
			slog.String("path", c.Path()),
			slog.Int("status", status),
			// Milliseconds, matching the httpclient observation: slog renders a
			// bare time.Duration as a unit-less nanosecond count.
			slog.Float64("durationMs", float64(elapsed.Nanoseconds())/1e6),
			slog.String("ip", c.IP()),
		)
		// The matched route template, when routing resolved one — the
		// low-cardinality grouping key (/users/:id), next to the concrete path.
		// Gated on c.Matched() because on an UNROUTED request Fiber leaves the
		// context pointing at the last candidate it examined — a middleware
		// entry whose Path is "/" — so an unguarded read labels every 404 as
		// route "/". Matched() is false exactly when no non-middleware route
		// took the request.
		if route := c.Route(); c.Matched() && route != nil &&
			route.Path != "" && route.Path != c.Path() {
			attrs = append(attrs, slog.String("route", route.Path))
		}

		// Level follows the STATUS, not the chain error: the ErrorHandler
		// renders a 404/422 from an error too, and a client mistake is not a
		// server warning. The error text still rides the record when present.
		level := slog.LevelInfo
		if status >= fiber.StatusInternalServerError {
			level = slog.LevelWarn
		}
		if chainErr != nil {
			attrs = append(attrs, slog.String("err", chainErr.Error()))
		}
		logger.LogAttrs(logCtx, level, accessLogMessage, attrs...)

		// The chain error was handled above (the response is already written),
		// so it must not propagate — mirrors Fiber's own logger middleware.
		return nil
	}
}
