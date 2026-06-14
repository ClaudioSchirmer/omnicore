package web

import (
	"errors"

	"github.com/ClaudioSchirmer/omnicore/application/notifications"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/gofiber/fiber/v2"
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
//  2. *fiber.Error with a code the framework specializes — 404 (router
//     could not match METHOD + path), 405 (path matches but the method
//     is not registered) and 413 (body exceeds the configured BodyLimit).
//     Each is emitted as its typed notification (RouteNotFoundNotification,
//     MethodNotAllowedNotification, PayloadTooLargeNotification) carrying
//     "METHOD /path" on FieldName so clients can branch UI without parsing
//     the translated message.
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
	return func(c *fiber.Ctx, err error) error {
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
			}
		}

		return respondInternalError(c, pipe)
	}
}

// respondCarrier translates the carrier's notifications via the Pipeline
// (so Accept-Language is honored) and emits the canonical envelope. Status
// derives from the messages' Semantic, falling back to 422 when every
// message is validation-flavored.
func respondCarrier(c *fiber.Ctx, pipe *pipeline.Pipeline, carrier domain.NotificationCarrier) error {
	dtos := notifications.ToContextDTOs(pipe.Translator(), AppContext(c).Language(), carrier.NotificationContexts())
	return Respond(c, ResponseFromContextDTOs(dtos, statusFromNotifications(dtos), ""))
}

// respondRouteNotFound emits a single RouteNotFoundNotification carrying
// "METHOD /path" as FieldName. SemanticNotFound maps to 404.
func respondRouteNotFound(c *fiber.Ctx, pipe *pipeline.Pipeline) error {
	ctx := domain.NewNotificationContext("Route")
	ctx.AddNotificationMessage(domain.NotificationMessage{
		FieldName:    c.Method() + " " + c.Path(),
		Notification: notifications.RouteNotFoundNotification{},
	})
	return respondViaPipeline(c, pipe, ctx)
}

// respondMethodNotAllowed emits a single MethodNotAllowedNotification carrying
// "METHOD /path" as FieldName. SemanticMethodNotAllowed maps to 405.
func respondMethodNotAllowed(c *fiber.Ctx, pipe *pipeline.Pipeline) error {
	ctx := domain.NewNotificationContext("Route")
	ctx.AddNotificationMessage(domain.NotificationMessage{
		FieldName:    c.Method() + " " + c.Path(),
		Notification: notifications.MethodNotAllowedNotification{},
	})
	return respondViaPipeline(c, pipe, ctx)
}

// respondPayloadTooLarge emits a single PayloadTooLargeNotification carrying
// "METHOD /path" as FieldName. SemanticPayloadTooLarge maps to 413.
func respondPayloadTooLarge(c *fiber.Ctx, pipe *pipeline.Pipeline) error {
	ctx := domain.NewNotificationContext("Request")
	ctx.AddNotificationMessage(domain.NotificationMessage{
		FieldName:    c.Method() + " " + c.Path(),
		Notification: notifications.PayloadTooLargeNotification{},
	})
	return respondViaPipeline(c, pipe, ctx)
}

// respondInternalError emits a single InternalServerErrorNotification.
// SemanticInternal maps to 500. The wire envelope deliberately carries no
// field, no value, no description of the underlying cause — the panic value
// and stack trace are logged by fwweb.Recover() before reaching this point.
func respondInternalError(c *fiber.Ctx, pipe *pipeline.Pipeline) error {
	ctx := domain.NewNotificationContext("Server")
	ctx.AddNotificationMessage(domain.NotificationMessage{
		Notification: notifications.InternalServerErrorNotification{},
	})
	return respondViaPipeline(c, pipe, ctx)
}

// respondViaPipeline routes the synthetic NotificationContext through
// pipeline.Run so the messages are translated against AppContext.Language()
// before serialization — same path RespondFromResult uses on Failure.
func respondViaPipeline(c *fiber.Ctx, pipe *pipeline.Pipeline, ctx *domain.NotificationContext) error {
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
