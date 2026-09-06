package web

import (
	"errors"

	"github.com/ClaudioSchirmer/omnicore/application/notifications"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/gofiber/fiber/v3"
)

// ErrorHandler returns a fiber.ErrorHandler that funnels every error escaping
// a route, middleware or panic recovery through the canonical Response
// envelope — same shape consumers already see from RespondFromResult.
//
// Four classes of input are handled:
//
//  1. NotificationCarrier — an error implementing the cross-layer contract
//     (DomainError, ApplicationError, InfrastructureError) escaped without
//     reaching RespondFromResult. Translated via the Pipeline and emitted
//     with the status derived from its own notifications' Semantic().
//
//  2. *fiber.Error with a code the framework specializes. Fiber's
//     serverErrorHandler normalizes every transport-level failure fasthttp
//     reports into a small set of statuses before the app's handler runs, and
//     this switch answers each of them with its typed notification carrying
//     "METHOD /path" on FieldName, so clients branch UI without parsing the
//     translated message:
//
//     400 — the request could not be read as HTTP at all (Fiber's catch-all
//     for a parse failure) → MalformedRequestNotification. The underlying
//     parse error stays off the wire.
//     404 — the router could not match METHOD + path → RouteNotFoundNotification.
//     405 — the path matches but the method is not registered (or fasthttp
//     reported ErrGetOnly) → MethodNotAllowedNotification.
//     408 — the fasthttp read timeout fired while reading the request; the
//     client was too slow → ReadTimeoutNotification.
//     413 — the body exceeds the configured BodyLimit → PayloadTooLargeNotification.
//     429 — a rate-limit middleware rejected through fiber.ErrTooManyRequests;
//     the framework ships no limiter, but it renders one's refusal →
//     TooManyRequestsNotification.
//     431 — the header block or request line did not fit the read buffer
//     (fasthttp's ErrSmallBuffer) → RequestHeaderFieldsTooLargeNotification.
//     501 — an HTTP request method this server implements nowhere →
//     NotImplementedNotification.
//
//     One status Fiber produces is deliberately NOT specialized: its
//     ErrBadGateway (502), raised for a non-timeout net.Error while reading
//     the request. That is a network failure on the CLIENT's connection, while
//     SemanticBadGateway means an upstream this service depends on answered
//     with something unusable — the opposite claim. Rendering it as 502 would
//     make the envelope assert something false, so it falls through to 500,
//     which is merely uninformative.
//
//  3. *fiber.Error with any other code — by design, treated as an unknown
//     escape and emitted as InternalServerErrorNotification with status 500.
//     Services that need custom HTTP semantics MUST emit a NotificationCarrier
//     with the appropriate Semantic() instead of calling fiber.NewError —
//     the "one canonical path" rule still applies.
//
//  4. Any other error — typically a panic recovered by fwweb.Recover() and
//     reported through Fiber's error chain. Emitted as
//     InternalServerErrorNotification with status 500. The underlying cause
//     stays only on the server log; the wire envelope never carries the
//     panic value or stack trace.
//
// Wired by bootstrap.Run into fiber.Config.ErrorHandler. Services that build
// the Fiber app manually via bootstrap.Build + bootstrap.Serve plug it in
// the same way:
//
//	app := fiber.New(fiber.Config{
//	    ErrorHandler: fwweb.ErrorHandler(deps.Pipeline),
//	})
func ErrorHandler(pipe *pipeline.Pipeline) fiber.ErrorHandler {
	return func(c fiber.Ctx, err error) error {
		if err == nil {
			return nil
		}

		var carrier domain.NotificationCarrier
		if errors.As(err, &carrier) {
			return respondCarrier(c, pipe, carrier)
		}

		var fe *fiber.Error
		if errors.As(err, &fe) {
			switch fe.Code {
			case fiber.StatusNotFound:
				return respondRouteNotFound(c, pipe)
			case fiber.StatusMethodNotAllowed:
				return respondMethodNotAllowed(c, pipe)
			case fiber.StatusRequestEntityTooLarge:
				return respondPayloadTooLarge(c, pipe)
			case fiber.StatusRequestTimeout:
				return respondReadTimeout(c, pipe)
			case fiber.StatusTooManyRequests:
				return respondTooManyRequests(c, pipe)
			case fiber.StatusBadRequest:
				return respondMalformedRequest(c, pipe)
			case fiber.StatusRequestHeaderFieldsTooLarge:
				return respondRequestHeaderFieldsTooLarge(c, pipe)
			case fiber.StatusNotImplemented:
				return respondNotImplemented(c, pipe)
			}
		}

		return respondInternalError(c, pipe)
	}
}

// respondCarrier translates the carrier's notifications via the Pipeline
// (so Accept-Language is honored) and emits the canonical envelope. Status
// derives from the messages' Semantic, falling back to 422 when every
// message is validation-flavored.
func respondCarrier(c fiber.Ctx, pipe *pipeline.Pipeline, carrier domain.NotificationCarrier) error {
	dtos := notifications.ToContextDTOs(pipe.Translator(), AppContext(c).Language(), carrier.NotificationContexts())
	return Respond(c, ResponseFromContextDTOs(dtos, statusFromNotifications(dtos), ""))
}

// respondRouteNotFound emits a single RouteNotFoundNotification carrying
// "METHOD /path" as FieldName. SemanticNotFound maps to 404.
func respondRouteNotFound(c fiber.Ctx, pipe *pipeline.Pipeline) error {
	ctx := domain.NewNotificationContext("Route")
	ctx.AddNotificationMessage(domain.NotificationMessage{
		Override:     c.Method() + " " + c.Path(),
		Notification: notifications.RouteNotFoundNotification{},
	})
	return respondViaPipeline(c, pipe, ctx)
}

// respondMethodNotAllowed emits a single MethodNotAllowedNotification carrying
// "METHOD /path" as FieldName. SemanticMethodNotAllowed maps to 405.
func respondMethodNotAllowed(c fiber.Ctx, pipe *pipeline.Pipeline) error {
	ctx := domain.NewNotificationContext("Route")
	ctx.AddNotificationMessage(domain.NotificationMessage{
		Override:     c.Method() + " " + c.Path(),
		Notification: notifications.MethodNotAllowedNotification{},
	})
	return respondViaPipeline(c, pipe, ctx)
}

// respondPayloadTooLarge emits a single PayloadTooLargeNotification carrying
// "METHOD /path" as FieldName. SemanticPayloadTooLarge maps to 413.
func respondPayloadTooLarge(c fiber.Ctx, pipe *pipeline.Pipeline) error {
	ctx := domain.NewNotificationContext("Request")
	ctx.AddNotificationMessage(domain.NotificationMessage{
		Override:     c.Method() + " " + c.Path(),
		Notification: notifications.PayloadTooLargeNotification{},
	})
	return respondViaPipeline(c, pipe, ctx)
}

// respondReadTimeout emits a single ReadTimeoutNotification carrying
// "METHOD /path" as FieldName. SemanticRequestTimeout maps to 408 — the
// transport read timeout (client too slow), distinct from the 504 handler
// deadline. Reached via Fiber's serverErrorHandler, which maps a fasthttp
// read-deadline net timeout to ErrRequestTimeout before this handler runs.
func respondReadTimeout(c fiber.Ctx, pipe *pipeline.Pipeline) error {
	ctx := domain.NewNotificationContext("Request")
	ctx.AddNotificationMessage(domain.NotificationMessage{
		Override:     c.Method() + " " + c.Path(),
		Notification: notifications.ReadTimeoutNotification{},
	})
	return respondViaPipeline(c, pipe, ctx)
}

// respondTooManyRequests emits a single TooManyRequestsNotification carrying
// "METHOD /path" as FieldName. SemanticTooManyRequests maps to 429. The
// framework ships no rate limiter — this branch exists so a middleware that
// rejects through fiber.ErrTooManyRequests (Fiber's own limiter does when its
// LimitReached handler returns the error rather than writing a bare status)
// lands in the canonical envelope instead of the 500 an unrecognized code
// used to produce. Any Retry-After the limiter already set on the context
// survives: this path only writes status + body.
func respondTooManyRequests(c fiber.Ctx, pipe *pipeline.Pipeline) error {
	ctx := domain.NewNotificationContext("Request")
	ctx.AddNotificationMessage(domain.NotificationMessage{
		Override:     c.Method() + " " + c.Path(),
		Notification: notifications.TooManyRequestsNotification{},
	})
	return respondViaPipeline(c, pipe, ctx)
}

// respondMalformedRequest emits a single MalformedRequestNotification carrying
// "METHOD /path" as FieldName. SemanticSchema maps to 400. This is Fiber's
// catch-all branch — a request it could not read as HTTP and had no more
// specific status for. It used to fall through to 500: the client sent
// something unreadable and was told the server had crashed.
func respondMalformedRequest(c fiber.Ctx, pipe *pipeline.Pipeline) error {
	ctx := domain.NewNotificationContext("Request")
	ctx.AddNotificationMessage(domain.NotificationMessage{
		Override:     c.Method() + " " + c.Path(),
		Notification: notifications.MalformedRequestNotification{},
	})
	return respondViaPipeline(c, pipe, ctx)
}

// respondRequestHeaderFieldsTooLarge emits a single
// RequestHeaderFieldsTooLargeNotification carrying "METHOD /path" as FieldName.
// SemanticRequestHeaderFieldsTooLarge maps to 431 — the header sibling of the
// 413 above. fasthttp raises ErrSmallBuffer while parsing the header block or
// the request line and Fiber normalizes it to this status.
func respondRequestHeaderFieldsTooLarge(c fiber.Ctx, pipe *pipeline.Pipeline) error {
	ctx := domain.NewNotificationContext("Request")
	ctx.AddNotificationMessage(domain.NotificationMessage{
		Override:     c.Method() + " " + c.Path(),
		Notification: notifications.RequestHeaderFieldsTooLargeNotification{},
	})
	return respondViaPipeline(c, pipe, ctx)
}

// respondNotImplemented emits a single NotImplementedNotification carrying
// "METHOD /path" as FieldName. SemanticNotImplemented maps to 501. Reached when
// fasthttp reports an unsupported HTTP request method — a verb this server
// implements nowhere, which is the transport-level reading of the same "the
// server does not do this" the service-level emitter makes.
func respondNotImplemented(c fiber.Ctx, pipe *pipeline.Pipeline) error {
	ctx := domain.NewNotificationContext("Request")
	ctx.AddNotificationMessage(domain.NotificationMessage{
		Override:     c.Method() + " " + c.Path(),
		Notification: notifications.NotImplementedNotification{},
	})
	return respondViaPipeline(c, pipe, ctx)
}

// respondInternalError emits a single InternalServerErrorNotification.
// SemanticInternal maps to 500. The wire envelope deliberately carries no
// field, no value, no description of the underlying cause — the panic value
// and stack trace are logged by fwweb.Recover() before reaching this point.
func respondInternalError(c fiber.Ctx, pipe *pipeline.Pipeline) error {
	ctx := domain.NewNotificationContext("Server")
	ctx.AddNotificationMessage(domain.NotificationMessage{
		Notification: notifications.InternalServerErrorNotification{},
	})
	return respondViaPipeline(c, pipe, ctx)
}

// respondViaPipeline routes the synthetic NotificationContext through
// pipeline.Run so the messages are translated against AppContext.Language()
// before serialization — same path RespondFromResult uses on Failure.
func respondViaPipeline(c fiber.Ctx, pipe *pipeline.Pipeline, ctx *domain.NotificationContext) error {
	derr := domain.NewDomainError([]*domain.NotificationContext{ctx})
	result := pipeline.Run(pipe, AppContext(c), func() (struct{}, error) {
		return struct{}{}, derr
	})
	return Respond(c, ResponseFromContextDTOs(
		result.Notifications(),
		statusFromNotifications(result.Notifications()),
		"",
	))
}
