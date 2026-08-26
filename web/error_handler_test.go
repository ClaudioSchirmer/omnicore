package web

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
)

// newAppWithErrorHandler builds a Fiber app wired the same way bootstrap.Run
// wires the framework defaults: ErrorHandler from this package, then Recover
// + AppContextMiddleware. The pipeline carries the default translator so the
// 4 languages of the kernel notifications resolve.
func newAppWithErrorHandler() *fiber.App {
	pipe := newTestPipeline()
	app := fiber.New(fiber.Config{
		ErrorHandler: ErrorHandler(pipe),
	})
	app.Use(Recover())
	app.Use(AppContextMiddleware())
	return app
}

// decodeResponse reads a Response envelope from the HTTP body. Used by every
// case below — the wire shape is what we are asserting.
func decodeResponse(t *testing.T, body io.Reader) Response {
	t.Helper()
	raw, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var resp Response
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("unmarshal body %q: %v", raw, err)
	}
	return resp
}

// TestErrorHandler_PanicInMiddleware proves that a panic happening in a
// middleware (BEFORE the route handler) is recovered by Recover, surfaced
// to ErrorHandler, and emitted as the canonical 500 envelope — never as
// plain text. The panic value itself stays only on the server log.
func TestErrorHandler_PanicInMiddleware(t *testing.T) {
	app := newAppWithErrorHandler()
	app.Use(func(c fiber.Ctx) error {
		panic("boom from middleware")
	})
	app.Get("/x", func(c fiber.Ctx) error {
		return c.SendString("never reached")
	})

	req := httptest.NewRequest("GET", "/x", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != fiber.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("expected JSON Content-Type, got %q", ct)
	}

	body := decodeResponse(t, resp.Body)
	resp.Body.Close()
	if body.Success {
		t.Fatalf("expected success=false, got true")
	}
	if body.Status != fiber.StatusInternalServerError {
		t.Fatalf("expected envelope status 500, got %d", body.Status)
	}
	if len(body.Errors) != 1 {
		t.Fatalf("expected 1 errors entry, got %d", len(body.Errors))
	}
	if body.Errors[0].Context != "Server" {
		t.Fatalf("expected context=\"Server\", got %q", body.Errors[0].Context)
	}
	if len(body.Errors[0].Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(body.Errors[0].Messages))
	}
	msg := body.Errors[0].Messages[0]
	if msg.NotificationKey != "InternalServerErrorNotification" {
		t.Fatalf("expected NotificationKey=InternalServerErrorNotification, got %q", msg.NotificationKey)
	}
	if msg.Semantic != "Internal" {
		t.Fatalf("expected Semantic=Internal, got %q", msg.Semantic)
	}

	rawBytes, _ := json.Marshal(body)
	if strings.Contains(string(rawBytes), "boom from middleware") {
		t.Fatalf("panic value leaked into wire envelope: %s", rawBytes)
	}
}

// TestErrorHandler_PanicInHandler covers the more common case — panic from
// inside a route handler. Same surface as the middleware case: 500 envelope,
// no leak.
func TestErrorHandler_PanicInHandler(t *testing.T) {
	app := newAppWithErrorHandler()
	app.Get("/boom", func(c fiber.Ctx) error {
		panic("kaboom")
	})

	req := httptest.NewRequest("GET", "/boom", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != fiber.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", resp.StatusCode)
	}

	body := decodeResponse(t, resp.Body)
	resp.Body.Close()
	if len(body.Errors) != 1 || body.Errors[0].Messages[0].NotificationKey != "InternalServerErrorNotification" {
		t.Fatalf("expected InternalServerErrorNotification envelope, got %+v", body)
	}
	rawBytes, _ := json.Marshal(body)
	if strings.Contains(string(rawBytes), "kaboom") {
		t.Fatalf("panic value leaked: %s", rawBytes)
	}
}

// TestErrorHandler_RouteNotFound proves Fiber's 404 from an unmatched route
// reaches the ErrorHandler and is emitted as RouteNotFoundNotification with
// the METHOD + path on field.
func TestErrorHandler_RouteNotFound(t *testing.T) {
	app := newAppWithErrorHandler()

	req := httptest.NewRequest("POST", "/does-not-exist", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != fiber.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}

	body := decodeResponse(t, resp.Body)
	resp.Body.Close()
	if body.Status != fiber.StatusNotFound {
		t.Fatalf("expected envelope status 404, got %d", body.Status)
	}
	if len(body.Errors) != 1 {
		t.Fatalf("expected 1 errors entry, got %d", len(body.Errors))
	}
	if body.Errors[0].Context != "Route" {
		t.Fatalf("expected context=\"Route\", got %q", body.Errors[0].Context)
	}
	msg := body.Errors[0].Messages[0]
	if msg.NotificationKey != "RouteNotFoundNotification" {
		t.Fatalf("expected NotificationKey=RouteNotFoundNotification, got %q", msg.NotificationKey)
	}
	if msg.Semantic != "NotFound" {
		t.Fatalf("expected Semantic=NotFound, got %q", msg.Semantic)
	}
	if msg.Field != "POST /does-not-exist" {
		t.Fatalf("expected field=\"POST /does-not-exist\", got %q", msg.Field)
	}
}

// TestErrorHandler_TranslatesByAcceptLanguage proves the synthetic
// notifications go through the Pipeline / Translator so Accept-Language is
// honored — same path RespondFromResult uses for domain failures.
//
// Cases include explicit absence of the header and an unmapped language so
// the documented fallback behavior (LangENG — see web/app_context.go
// parseLanguage) is regression-proof.
func TestErrorHandler_TranslatesByAcceptLanguage(t *testing.T) {
	cases := []struct {
		header  string
		want500 string
	}{
		{"pt-BR", "Erro interno do servidor."},
		{"en", "Internal server error."},
		{"es", "Error interno del servidor."},
		{"fr", "Erreur interne du serveur."},
		{"", "Internal server error."},      // header absent → LangENG
		{"ja-JP", "Internal server error."}, // unmapped → LangENG
	}

	for _, tc := range cases {
		t.Run(tc.header, func(t *testing.T) {
			app := newAppWithErrorHandler()
			app.Get("/boom", func(c fiber.Ctx) error {
				panic("ignored")
			})

			req := httptest.NewRequest("GET", "/boom", nil)
			req.Header.Set("Accept-Language", tc.header)
			resp, err := app.Test(req)
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}

			body := decodeResponse(t, resp.Body)
			resp.Body.Close()
			got := body.Errors[0].Messages[0].Message
			if got != tc.want500 {
				t.Fatalf("Accept-Language=%q → message=%q, want %q", tc.header, got, tc.want500)
			}
		})
	}
}

// TestErrorHandler_MethodNotAllowed proves a *fiber.Error with code 405
// reaching the ErrorHandler (raised by a middleware/handler when the path
// matches but the HTTP method does not) is emitted as the canonical
// MethodNotAllowedNotification envelope, status 405, context "Route",
// field carrying "METHOD /path". A distinct sentinel string on the
// fiber.Error proves the original message never leaks to the wire.
func TestErrorHandler_MethodNotAllowed(t *testing.T) {
	const sentinel = "FIBER-405-SENTINEL-XYZ"
	app := newAppWithErrorHandler()
	app.All("/users/whoami", func(c fiber.Ctx) error {
		return fiber.NewError(fiber.StatusMethodNotAllowed, sentinel)
	})

	req := httptest.NewRequest("DELETE", "/users/whoami", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != fiber.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", resp.StatusCode)
	}

	body := decodeResponse(t, resp.Body)
	resp.Body.Close()
	if body.Status != fiber.StatusMethodNotAllowed {
		t.Fatalf("expected envelope status 405, got %d", body.Status)
	}
	if len(body.Errors) != 1 {
		t.Fatalf("expected 1 errors entry, got %d", len(body.Errors))
	}
	if body.Errors[0].Context != "Route" {
		t.Fatalf("expected context=\"Route\", got %q", body.Errors[0].Context)
	}
	msg := body.Errors[0].Messages[0]
	if msg.NotificationKey != "MethodNotAllowedNotification" {
		t.Fatalf("expected NotificationKey=MethodNotAllowedNotification, got %q", msg.NotificationKey)
	}
	if msg.Semantic != "MethodNotAllowed" {
		t.Fatalf("expected Semantic=MethodNotAllowed, got %q", msg.Semantic)
	}
	if msg.Field != "DELETE /users/whoami" {
		t.Fatalf("expected field=\"DELETE /users/whoami\", got %q", msg.Field)
	}
	rawBytes, _ := json.Marshal(body)
	if strings.Contains(string(rawBytes), sentinel) {
		t.Fatalf("fiber.Error message leaked: %s", rawBytes)
	}
}

// TestErrorHandler_PayloadTooLarge proves a *fiber.Error with code 413
// (the shape Fiber's BodyLimit middleware emits when a request body exceeds
// the configured limit — same shape any handler can raise via fiber.NewError)
// reaches the ErrorHandler and is emitted as PayloadTooLargeNotification,
// status 413, context "Request", field carrying "METHOD /path".
//
// The handler raises the error explicitly (rather than relying on
// fasthttp's in-memory test transport to forward an oversized body) so
// the test exercises OUR mapping deterministically, independent of
// Fiber/fasthttp's client-side body-limit enforcement.
func TestErrorHandler_PayloadTooLarge(t *testing.T) {
	const sentinel = "FIBER-413-SENTINEL-XYZ"
	app := newAppWithErrorHandler()
	app.Post("/echo", func(c fiber.Ctx) error {
		return fiber.NewError(fiber.StatusRequestEntityTooLarge, sentinel)
	})

	req := httptest.NewRequest("POST", "/echo", strings.NewReader("payload"))
	req.Header.Set("Content-Type", "text/plain")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != fiber.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d", resp.StatusCode)
	}

	body := decodeResponse(t, resp.Body)
	resp.Body.Close()
	if body.Status != fiber.StatusRequestEntityTooLarge {
		t.Fatalf("expected envelope status 413, got %d", body.Status)
	}
	if len(body.Errors) != 1 {
		t.Fatalf("expected 1 errors entry, got %d", len(body.Errors))
	}
	if body.Errors[0].Context != "Request" {
		t.Fatalf("expected context=\"Request\", got %q", body.Errors[0].Context)
	}
	msg := body.Errors[0].Messages[0]
	if msg.NotificationKey != "PayloadTooLargeNotification" {
		t.Fatalf("expected NotificationKey=PayloadTooLargeNotification, got %q", msg.NotificationKey)
	}
	if msg.Semantic != "PayloadTooLarge" {
		t.Fatalf("expected Semantic=PayloadTooLarge, got %q", msg.Semantic)
	}
	if msg.Field != "POST /echo" {
		t.Fatalf("expected field=\"POST /echo\", got %q", msg.Field)
	}
	rawBytes, _ := json.Marshal(body)
	if strings.Contains(string(rawBytes), sentinel) {
		t.Fatalf("fiber.Error message leaked: %s", rawBytes)
	}
}

// TestErrorHandler_ReadTimeout proves a *fiber.Error with code 408 (the shape
// Fiber's serverErrorHandler emits when the fasthttp read timeout fires — the
// client was too slow sending the request) reaches the ErrorHandler and is
// emitted as ReadTimeoutNotification, status 408, context "Request", field
// carrying "METHOD /path" — NOT a misleading 500.
func TestErrorHandler_ReadTimeout(t *testing.T) {
	const sentinel = "FIBER-408-SENTINEL-XYZ"
	app := newAppWithErrorHandler()
	app.Get("/slow", func(c fiber.Ctx) error {
		return fiber.NewError(fiber.StatusRequestTimeout, sentinel)
	})

	resp, err := app.Test(httptest.NewRequest("GET", "/slow", nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != fiber.StatusRequestTimeout {
		t.Fatalf("expected 408, got %d", resp.StatusCode)
	}

	body := decodeResponse(t, resp.Body)
	resp.Body.Close()
	if body.Status != fiber.StatusRequestTimeout {
		t.Fatalf("expected envelope status 408, got %d", body.Status)
	}
	if len(body.Errors) != 1 || body.Errors[0].Context != "Request" {
		t.Fatalf("expected 1 error in context \"Request\", got %+v", body.Errors)
	}
	msg := body.Errors[0].Messages[0]
	if msg.NotificationKey != "ReadTimeoutNotification" {
		t.Fatalf("expected NotificationKey=ReadTimeoutNotification, got %q", msg.NotificationKey)
	}
	if msg.Semantic != "RequestTimeout" {
		t.Fatalf("expected Semantic=RequestTimeout, got %q", msg.Semantic)
	}
	if msg.Field != "GET /slow" {
		t.Fatalf("expected field=\"GET /slow\", got %q", msg.Field)
	}
	if rawBytes, _ := json.Marshal(body); strings.Contains(string(rawBytes), sentinel) {
		t.Fatalf("fiber.Error message leaked: %s", rawBytes)
	}
}

// TestErrorHandler_FiberErrorOtherCode proves a *fiber.Error with a code
// the framework does NOT specialize (418 — the standing sentinel, and a status
// the vocabulary deliberately never maps) is treated as an unknown escape and
// falls through to the 500 envelope. The framework specializes
// 400 / 404 / 405 / 408 / 413 / 429 / 431 / 501; every other code stays as 500
// by design — services that need custom HTTP semantics must emit a
// NotificationCarrier instead of raising fiber.NewError.
func TestErrorHandler_FiberErrorOtherCode_FallsThroughToInternal(t *testing.T) {
	app := newAppWithErrorHandler()
	app.Get("/teapot", func(c fiber.Ctx) error {
		return fiber.NewError(fiber.StatusTeapot, "i am a teapot")
	})

	req := httptest.NewRequest("GET", "/teapot", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != fiber.StatusInternalServerError {
		t.Fatalf("expected 500 (teapot falls through), got %d", resp.StatusCode)
	}

	body := decodeResponse(t, resp.Body)
	resp.Body.Close()
	if body.Errors[0].Messages[0].NotificationKey != "InternalServerErrorNotification" {
		t.Fatalf("expected InternalServerErrorNotification, got %q", body.Errors[0].Messages[0].NotificationKey)
	}
	rawBytes, _ := json.Marshal(body)
	if strings.Contains(string(rawBytes), "i am a teapot") {
		t.Fatalf("fiber.Error message leaked: %s", rawBytes)
	}
}

// TestErrorHandler_TooManyRequests proves a middleware rejecting through
// fiber.ErrTooManyRequests reaches the canonical envelope as a typed 429.
// Before the branch existed this landed in the unknown-escape fallthrough and
// a rate-limited client was told the server had crashed.
func TestErrorHandler_TooManyRequests(t *testing.T) {
	const sentinel = "FIBER-429-SENTINEL-XYZ"
	app := newAppWithErrorHandler()
	app.Get("/limited", func(c fiber.Ctx) error {
		return fiber.NewError(fiber.StatusTooManyRequests, sentinel)
	})

	resp, err := app.Test(httptest.NewRequest("GET", "/limited", nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != fiber.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", resp.StatusCode)
	}

	body := decodeResponse(t, resp.Body)
	resp.Body.Close()
	if body.Status != fiber.StatusTooManyRequests {
		t.Fatalf("expected envelope status 429, got %d", body.Status)
	}
	if len(body.Errors) != 1 || body.Errors[0].Context != "Request" {
		t.Fatalf("expected 1 error in context \"Request\", got %+v", body.Errors)
	}
	msg := body.Errors[0].Messages[0]
	if msg.NotificationKey != "TooManyRequestsNotification" {
		t.Fatalf("expected NotificationKey=TooManyRequestsNotification, got %q", msg.NotificationKey)
	}
	if msg.Semantic != "TooManyRequests" {
		t.Fatalf("expected Semantic=TooManyRequests, got %q", msg.Semantic)
	}
	if msg.Field != "GET /limited" {
		t.Fatalf("expected field=\"GET /limited\", got %q", msg.Field)
	}
	if rawBytes, _ := json.Marshal(body); strings.Contains(string(rawBytes), sentinel) {
		t.Fatalf("fiber.Error message leaked: %s", rawBytes)
	}
}

// TestErrorHandler_TooManyRequests_PreservesRetryAfter proves the retry hint a
// limiter sets on the context before rejecting survives the envelope render.
// A 429 without Retry-After is half an answer, and the canonical Response
// carries no header slot — so the guarantee the framework can make is that it
// does not DROP what the middleware already set.
func TestErrorHandler_TooManyRequests_PreservesRetryAfter(t *testing.T) {
	app := newAppWithErrorHandler()
	app.Use(func(c fiber.Ctx) error {
		c.Set(fiber.HeaderRetryAfter, "30")
		return fiber.ErrTooManyRequests
	})
	app.Get("/limited", func(c fiber.Ctx) error { return nil })

	resp, err := app.Test(httptest.NewRequest("GET", "/limited", nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != fiber.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get(fiber.HeaderRetryAfter); got != "30" {
		t.Fatalf("expected Retry-After preserved as \"30\", got %q", got)
	}
}

// errorHandlerBranch is the shared shape of the transport-authored branches:
// a *fiber.Error code Fiber's serverErrorHandler can produce, answered with a
// typed notification in the canonical envelope, carrying METHOD /path and
// never leaking the fiber message.
type errorHandlerBranch struct {
	name     string
	code     int
	key      string
	semantic string
	context  string
}

// TestErrorHandler_TransportAuthoredBranches covers the three branches added
// when the audit of Fiber's serverErrorHandler (app.go, the switch that
// normalizes every fasthttp failure) showed it hands us statuses we were
// turning into 500: a request it could not read as HTTP at all, a header block
// over the read buffer, and an HTTP verb this server implements nowhere.
func TestErrorHandler_TransportAuthoredBranches(t *testing.T) {
	cases := []errorHandlerBranch{
		{"malformed request", fiber.StatusBadRequest, "MalformedRequestNotification", "Schema", "Request"},
		{"header block too large", fiber.StatusRequestHeaderFieldsTooLarge, "RequestHeaderFieldsTooLargeNotification", "RequestHeaderFieldsTooLarge", "Request"},
		{"unsupported HTTP method", fiber.StatusNotImplemented, "NotImplementedNotification", "NotImplemented", "Request"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const sentinel = "FIBER-SENTINEL-SHOULD-NOT-LEAK"
			app := newAppWithErrorHandler()
			app.Get("/probe", func(c fiber.Ctx) error {
				return fiber.NewError(tc.code, sentinel)
			})

			resp, err := app.Test(httptest.NewRequest("GET", "/probe", nil))
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}
			if resp.StatusCode != tc.code {
				t.Fatalf("expected %d, got %d", tc.code, resp.StatusCode)
			}

			body := decodeResponse(t, resp.Body)
			resp.Body.Close()
			if body.Status != tc.code {
				t.Fatalf("expected envelope status %d, got %d", tc.code, body.Status)
			}
			if len(body.Errors) != 1 || body.Errors[0].Context != tc.context {
				t.Fatalf("expected 1 error in context %q, got %+v", tc.context, body.Errors)
			}
			msg := body.Errors[0].Messages[0]
			if msg.NotificationKey != tc.key {
				t.Fatalf("expected NotificationKey=%s, got %q", tc.key, msg.NotificationKey)
			}
			if msg.Semantic != tc.semantic {
				t.Fatalf("expected Semantic=%s, got %q", tc.semantic, msg.Semantic)
			}
			if msg.Field != "GET /probe" {
				t.Fatalf("expected field=\"GET /probe\", got %q", msg.Field)
			}
			if rawBytes, _ := json.Marshal(body); strings.Contains(string(rawBytes), sentinel) {
				t.Fatalf("fiber.Error message leaked: %s", rawBytes)
			}
		})
	}
}

// TestErrorHandler_BadGatewayStaysInternal locks a deliberate NON-mapping.
// Fiber's serverErrorHandler raises ErrBadGateway (502) for a non-timeout
// net.Error while reading the request — a network failure on the CLIENT's
// connection. SemanticBadGateway means the opposite: an upstream this service
// depends on answered with something unusable. Rendering it as 502 would make
// the envelope assert something false, so it falls through to 500, which is
// merely uninformative. If someone "completes" the switch by adding a 502
// branch, this fails and points them here.
func TestErrorHandler_BadGatewayStaysInternal(t *testing.T) {
	app := newAppWithErrorHandler()
	app.Get("/upstream", func(c fiber.Ctx) error { return fiber.ErrBadGateway })

	resp, err := app.Test(httptest.NewRequest("GET", "/upstream", nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != fiber.StatusInternalServerError {
		t.Fatalf("expected 500 (502 is deliberately not specialized), got %d", resp.StatusCode)
	}
	body := decodeResponse(t, resp.Body)
	resp.Body.Close()
	if got := body.Errors[0].Messages[0].NotificationKey; got != "InternalServerErrorNotification" {
		t.Fatalf("expected InternalServerErrorNotification, got %q", got)
	}
}
