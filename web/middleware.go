package web

import (
	"log/slog"
	"runtime/debug"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/recover"
)

// Recover catches panics escaping downstream middleware or route handlers,
// logs the panic value + stack trace via slog (LevelError), and surfaces a
// generic error so the registered fiber.ErrorHandler — ErrorHandler in this
// package — emits the canonical Response envelope. The panic value itself
// never leaves the server log.
func Recover() fiber.Handler {
	return recover.New(recover.Config{
		EnableStackTrace: true,
		StackTraceHandler: func(c fiber.Ctx, e any) {
			slog.Default().Error("panic recovered",
				slog.String("method", c.Method()),
				slog.String("path", c.Path()),
				slog.Any("panic", e),
				slog.String("stack", string(debug.Stack())),
			)
		},
	})
}
