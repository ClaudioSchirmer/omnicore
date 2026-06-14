package web

import (
	"net/http"

	"github.com/ClaudioSchirmer/omnicore/application/notifications"
	"github.com/ClaudioSchirmer/omnicore/application/translation"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/web/openapi"
	"github.com/gofiber/fiber/v2"
)

// PermissionGate produces the openapi.Gate the framework registers via
// openapi.SetGate at boot. Captures the Translator so the 403 envelope's
// message can be translated against AppContext.Language() per request
// without going through Pipeline — the gate runs before the handler, has no
// pipeline.Result to thread, and the work to emit a translated envelope is
// just a Translator.GetOr lookup. Falls back to the English default when
// the catalog has no entry for the request's language.
//
// Wired by bootstrap.Run:
//
//	openapi.SetGate(fwweb.PermissionGate(deps.Translator))
//
// Consumer services using bootstrap.Build + bootstrap.Serve manually call
// the same two lines themselves.
func PermissionGate(tr *translation.Translator) openapi.Gate {
	return func(handler fiber.Handler, permission string) fiber.Handler {
		return func(c *fiber.Ctx) error {
			if !authorizationEnabled() {
				// Master switch off — the spec still carries the
				// RequiredPermission entry (consumers see the intended
				// gate ahead of the runtime flip), but the per-request
				// check no-ops so services can annotate routes before
				// the operator turns the layer on.
				return handler(c)
			}
			id := AppContext(c).Identity()
			if id != nil && id.HasPermission(permission) {
				return handler(c)
			}
			return respondMissingPermission(c, tr, permission)
		}
	}
}

// respondMissingPermission renders the canonical 403 envelope with shape
// identical to the framework's other rejections (Authorization context,
// MissingPermissionNotification, field "permission", value = the declared
// permission string). Translates the message manually via the Translator
// since the gate has no Pipeline to route through.
func respondMissingPermission(c *fiber.Ctx, tr *translation.Translator, permission string) error {
	n := notifications.MissingPermissionNotification{}
	key := domain.NotificationKey(n)
	msg := "Missing required permission." // English default — matches eng.go catalog
	if tr != nil {
		msg = tr.GetOr(AppContext(c).Language(), key, msg)
	}
	return Respond(c, Response{
		Success:     false,
		Status:      fiber.StatusForbidden,
		Description: http.StatusText(fiber.StatusForbidden),
		Errors: []Error{{
			Context: "Authorization",
			Messages: []ErrorMessage{{
				NotificationKey: key,
				Field:           "permission",
				Value:           permission,
				Message:         msg,
				Semantic:        n.Semantic().String(),
			}},
		}},
	})
}
