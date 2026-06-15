package web

import (
	"net/http"

	"github.com/ClaudioSchirmer/omnicore/application/notifications"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/gofiber/fiber/v3"
)

type Response struct {
	Success     bool    `json:"success"`
	Status      int     `json:"status"`
	Description string  `json:"description"`
	Data        any     `json:"data,omitempty"`
	// Pagination is `any` because the slot carries two legitimate shapes:
	// PaginationInfo on regular paged listings (has_next/has_prev/cursors +
	// total) and TotalOnlyPagination on count-only requests (only total).
	// Both are typed Go structs — there is no untyped map on the wire.
	// `omitempty` skips the slot when the response is not pagination-shaped.
	Pagination any     `json:"pagination,omitempty"`
	Errors     []Error `json:"errors,omitempty"`
}

// PaginationInfo lives top-level on a Response (not nested under Data) so that
// a GET list endpoint exposes the items as `data` and the cursor envelope as
// `pagination`, instead of forcing clients into `data.items` / `data.has_next`.
// Populated by HandleQueryWithParams on success.
type PaginationInfo struct {
	HasNext    bool   `json:"has_next"`
	HasPrev    bool   `json:"has_prev"`
	NextCursor string `json:"next_cursor,omitempty"`
	PrevCursor string `json:"prev_cursor,omitempty"`
	Total      int64  `json:"total"`
}

// TotalOnlyPagination is the pagination shape emitted when the wire request
// declared `?onlyTotal=true`. The reader short-circuits to a count primitive
// and the wrapper drops the `data` slot AND the listing-only fields
// (has_next/has_prev/cursors) so the response carries strictly what the
// consumer asked for — `pagination.total`.
type TotalOnlyPagination struct {
	Total int64 `json:"total"`
}

type Error struct {
	Context  string         `json:"context"`
	Messages []ErrorMessage `json:"messages"`
}

type ErrorMessage struct {
	// NotificationKey is the unqualified Go type name of the source Notification
	// (e.g. "RecordNotFoundNotification"). Clients can branch UI on it without
	// parsing translated message strings.
	NotificationKey string `json:"notificationKey,omitempty"`
	Field           string `json:"field,omitempty"`
	Value           string `json:"value,omitempty"`
	FuncName        string `json:"funcName,omitempty"`
	Message         string `json:"message"`
	// Semantic is the transport-agnostic classification of the source Notification
	// as a string ("Validation", "NotFound", "Conflict", "Forbidden",
	// "Unauthorized", "Unavailable"). Clients can branch UI without parsing the
	// HTTP status code.
	Semantic string `json:"semantic,omitempty"`
}

func Respond(c fiber.Ctx, resp Response) error {
	return c.Status(resp.Status).JSON(resp)
}

func RespondWithSuccess(c fiber.Ctx, status int, data any) error {
	return Respond(c, Response{
		Success:     true,
		Status:      status,
		Description: http.StatusText(status),
		Data:        data,
	})
}

func RespondWithStatus(c fiber.Ctx, status int) error {
	return Respond(c, Response{
		Success:     status >= 200 && status < 400,
		Status:      status,
		Description: http.StatusText(status),
	})
}

func RespondWithBadRequest(c fiber.Ctx) error {
	return RespondWithStatus(c, fiber.StatusBadRequest)
}

func RespondWithUnauthorized(c fiber.Ctx) error {
	return RespondWithStatus(c, fiber.StatusUnauthorized)
}

func RespondWithForbidden(c fiber.Ctx) error {
	return RespondWithStatus(c, fiber.StatusForbidden)
}

func RespondWithNotFound(c fiber.Ctx) error {
	return RespondWithStatus(c, fiber.StatusNotFound)
}

// RespondWithInternalServerError emits the canonical failure envelope carrying
// InternalServerErrorNotification — same JSON shape as every other framework
// rejection — for paths that cannot route through the Pipeline. Called from
// RespondFromResult on the Exception branch (a panic caught by pipeline.Run's
// defer/recover) and from openapi/ spec-assembly bail-outs.
//
// The message is translated against AppContext.Language() via the Translator
// registered with SetTranslator (called by bootstrap.Run). Falls back to the
// English default when no Translator is registered or when the catalog has
// no entry for the requested language — the helper must never fail because
// it IS the canonical 500 path. Translator.GetOr does not panic on its own
// (RWMutex + map lookup); the original fear of the error-response path
// cascading a handler panic was overly defensive — the lookup is at most a
// no-op fallback.
func RespondWithInternalServerError(c fiber.Ctx) error {
	n := notifications.InternalServerErrorNotification{}
	msg := "Internal server error."
	if tr := registeredTranslator(); tr != nil {
		msg = tr.GetOr(AppContext(c).Language(), domain.NotificationKey(n), msg)
	}
	return Respond(c, Response{
		Success:     false,
		Status:      fiber.StatusInternalServerError,
		Description: http.StatusText(fiber.StatusInternalServerError),
		Errors: []Error{{
			Context: "Server",
			Messages: []ErrorMessage{{
				NotificationKey: domain.NotificationKey(n),
				Message:         msg,
				Semantic:        n.Semantic().String(),
			}},
		}},
	})
}
