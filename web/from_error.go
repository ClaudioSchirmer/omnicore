package web

import (
	"errors"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/notifications"
	"github.com/ClaudioSchirmer/omnicore/application/translation"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/gofiber/fiber/v3"
)

// ResponseFromError discriminates the error type:
//   - any error implementing domain.NotificationCarrier (DomainError,
//     ApplicationError, InfrastructureError) → 422 with translated contexts
//   - anything else → 500
func ResponseFromError(err error, translator *translation.Translator, lang configuration.Language) Response {
	if err == nil {
		return Response{
			Success: true,
			Status:  fiber.StatusOK,
		}
	}

	var carrier domain.NotificationCarrier
	if errors.As(err, &carrier) {
		if translator != nil {
			dtos := notifications.ToContextDTOs(translator, lang, carrier.NotificationContexts())
			return ResponseFromContextDTOs(dtos, fiber.StatusUnprocessableEntity, "")
		}
		return ResponseFromContexts(carrier.NotificationContexts(), fiber.StatusUnprocessableEntity, "")
	}

	return Response{
		Success:     false,
		Status:      fiber.StatusInternalServerError,
		Description: "Internal Server Error",
	}
}

func RespondFromError(c fiber.Ctx, err error, translator *translation.Translator, lang configuration.Language) error {
	return Respond(c, ResponseFromError(err, translator, lang))
}
