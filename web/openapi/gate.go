package openapi

import (
	"sync"

	"github.com/gofiber/fiber/v3"
)

// Gate is the callback the framework registers at boot so Mount / MountRaw
// can substitute a permission-checked handler when fwopenapi.RequirePermission
// is declared on the route. The returned fiber.Handler short-circuits with
// the canonical 403 envelope when the request's Identity does not carry the
// required permission; otherwise it delegates to the original handler.
//
// The signature is package-private through the type alias: web.PermissionGate
// produces a Gate, openapi.SetGate stores it, openapi.Mount/MountRaw consume
// it. Consumer services never implement Gate themselves — the framework
// owns the runtime semantics.
//
// Lives as an indirection because web/openapi cannot import web/ (web/
// already imports web/openapi via spec_command.go and spec_query.go — going
// the other way would cycle). The web package, which has access to
// AppContext, Response, and the Translator, builds the gate; openapi calls
// it via this callback.
type Gate func(handler fiber.Handler, permission string) fiber.Handler

var (
	gateMu       sync.RWMutex
	registeredGate Gate
)

// SetGate registers the framework's PermissionGate at boot. Called once by
// bootstrap.Run with the gate produced by web.PermissionGate(deps.Translator).
// Idempotent and concurrent-safe; in practice called once per process.
//
// Calling Mount / MountRaw with a RequirePermission option BEFORE SetGate has
// been called (or in a service whose bootstrap path skips it) panics at the
// Mount call site — the caller declared a gate the framework cannot enforce.
// Forces the operational discipline of wiring authorization at boot, never
// later.
func SetGate(g Gate) {
	gateMu.Lock()
	defer gateMu.Unlock()
	registeredGate = g
}

func resolveGate() Gate {
	gateMu.RLock()
	defer gateMu.RUnlock()
	return registeredGate
}
