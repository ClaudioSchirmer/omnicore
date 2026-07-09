package web

import (
	"github.com/ClaudioSchirmer/omnicore/application/notifications"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/gofiber/fiber/v3"
)

// RespondFromResult renders a pipeline.Result[T] to the HTTP response:
//   - Success    → successStatus with Data populated
//   - Failure    → status derived from each notification's Semantic()
//     (default 422 Unprocessable Entity) with translated errors
//   - Exception  → 500 Internal Server Error in the canonical envelope carrying
//     InternalServerErrorNotification (English default, no translation —
//     RespondWithInternalServerError stays standalone so a panic inside the
//     handler cannot cascade into the error-response path itself; internals
//     not leaked)
//
// The Failure status is the first non-Validation Semantic found in the result's
// notifications, mapped via semanticToStatus. Falls through to 422 otherwise.
// Notifications declare their Semantic themselves — no global registration.
func RespondFromResult[T any](c fiber.Ctx, result pipeline.Result[T], successStatus int) error {
	switch {
	case result.IsSuccess():
		return RespondWithSuccess(c, successStatus, result.Value())

	case result.IsFailure():
		return Respond(c, ResponseFromContextDTOs(
			result.Notifications(),
			statusFromNotifications(result.Notifications()),
			"",
		))

	default:
		return RespondWithInternalServerError(c)
	}
}

// semanticToStatus maps the transport-agnostic NotificationSemantic to its
// canonical HTTP status code. A different transport (gRPC, GraphQL) would
// build its own table from the same enum.
var semanticToStatus = map[domain.NotificationSemantic]int{
	domain.SemanticValidation:       fiber.StatusUnprocessableEntity,   // 422
	domain.SemanticNotFound:         fiber.StatusNotFound,              // 404
	domain.SemanticConflict:         fiber.StatusConflict,              // 409
	domain.SemanticForbidden:        fiber.StatusForbidden,             // 403
	domain.SemanticUnauthorized:     fiber.StatusUnauthorized,          // 401
	domain.SemanticUnavailable:      fiber.StatusServiceUnavailable,    // 503
	domain.SemanticSchema:           fiber.StatusBadRequest,            // 400
	domain.SemanticInternal:         fiber.StatusInternalServerError,   // 500
	domain.SemanticMethodNotAllowed: fiber.StatusMethodNotAllowed,      // 405
	domain.SemanticPayloadTooLarge:  fiber.StatusRequestEntityTooLarge, // 413
	domain.SemanticGatewayTimeout:   fiber.StatusGatewayTimeout,        // 504
	domain.SemanticStateConflict:    fiber.StatusConflict,              // 409 (state/precondition flavor)
	domain.SemanticRequestTimeout:   fiber.StatusRequestTimeout,        // 408 (transport read timeout — client too slow)
}

// statusFromNotifications returns the HTTP status of the first message whose
// Semantic is not Validation, falling back to 422 when all messages are
// validation (the conventional "validation failed" status). The first
// non-validation Semantic wins so that a mixed-bag failure (e.g. one field
// validation + one conflict) surfaces the more specific transport semantic.
func statusFromNotifications(dtos []notifications.ContextDTO) int {
	for _, ctx := range dtos {
		for _, msg := range ctx.Messages {
			if msg.Semantic == domain.SemanticValidation {
				continue
			}
			if status, ok := semanticToStatus[msg.Semantic]; ok {
				return status
			}
		}
	}
	return fiber.StatusUnprocessableEntity
}
