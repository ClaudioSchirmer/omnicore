package web

import (
	"strings"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
)

const appContextKey = "omnicore.appCtx"

// AppContextMiddleware populates a *configuration.AppContext per request
// from the HTTP headers:
//
//	X-Request-ID    → ctx.ID (generates a new UUID if absent or invalid)
//	Accept-Language → ctx.Language (default LangENG; iterates over Language enum)
//
// Always returns X-Request-ID in the response for client-side correlation.
//
// Registered automatically by bootstrap.Run. Anyone using bootstrap.Build/Serve
// manually calls it explicitly:
//
//	app.Use(fwweb.AppContextMiddleware())
func AppContextMiddleware() fiber.Handler {
	return func(c fiber.Ctx) error {
		id := parseRequestID(c.Get("X-Request-ID"))
		lang := parseLanguage(c.Get("Accept-Language"))
		ctx := configuration.NewAppContext(id, lang)
		c.Locals(appContextKey, ctx)
		c.Set("X-Request-ID", id.String())
		return c.Next()
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
