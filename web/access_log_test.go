package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
)

// newAppWithAccessLog wires the app the way bootstrap does: ErrorHandler,
// Recover, THEN the access log (outside AppContextMiddleware, so it observes
// the whole chain), then the AppContext.
func newAppWithAccessLog(logger *slog.Logger, withAppContext bool) *fiber.App {
	pipe := newTestPipeline()
	app := fiber.New(fiber.Config{ErrorHandler: ErrorHandler(pipe)})
	app.Use(AccessLog(logger))
	app.Use(Recover())
	if withAppContext {
		app.Use(AppContextMiddleware())
	}
	return app
}

// accessLogRecord runs one request and returns the single decoded access-log
// record it produced.
func accessLogRecord(t *testing.T, app *fiber.App, buf *bytes.Buffer, method, path string) map[string]any {
	t.Helper()
	resp, err := app.Test(httptest.NewRequest(method, path, nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	_ = resp.Body.Close()

	var rec map[string]any
	lines := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n"))
	found := 0
	for _, line := range lines {
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("access log line is not JSON: %q", line)
		}
		if m["msg"] == accessLogMessage {
			rec = m
			found++
		}
	}
	if found != 1 {
		t.Fatalf("expected exactly 1 %q record, got %d — log was: %s", accessLogMessage, found, buf.String())
	}
	return rec
}

func newCapturingLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

// TestAccessLog_EmitsStructuredRecord asserts the happy path emits ONE JSON
// record carrying the request vocabulary — the whole point of replacing
// Fiber's plaintext template.
func TestAccessLog_EmitsStructuredRecord(t *testing.T) {
	logger, buf := newCapturingLogger()
	app := newAppWithAccessLog(logger, true)
	app.Get("/users/:id", func(c fiber.Ctx) error { return c.SendString("ok") })

	rec := accessLogRecord(t, app, buf, "GET", "/users/42")

	if rec["level"] != "INFO" {
		t.Errorf("level = %v, want INFO", rec["level"])
	}
	if rec["method"] != "GET" {
		t.Errorf("method = %v, want GET", rec["method"])
	}
	if rec["path"] != "/users/42" {
		t.Errorf("path = %v, want /users/42", rec["path"])
	}
	if rec["status"] != float64(fiber.StatusOK) {
		t.Errorf("status = %v, want 200", rec["status"])
	}
	if rec["route"] != "/users/:id" {
		t.Errorf("route = %v, want the low-cardinality template /users/:id", rec["route"])
	}
	if _, ok := rec["durationMs"].(float64); !ok {
		t.Errorf("durationMs missing or not numeric: %v", rec["durationMs"])
	}
	// The correlation field that the plaintext line never carried: it MUST
	// match the X-Request-ID the AppContext middleware echoed back.
	if rec["threadId"] == nil || rec["threadId"] == "" {
		t.Errorf("threadId missing — the access line cannot be joined to the request's other records: %v", rec)
	}
}

// TestAccessLog_ThreadIDMatchesRequestID pins the correlation end to end: the
// id on the access record is the same one the client sent and got echoed.
func TestAccessLog_ThreadIDMatchesRequestID(t *testing.T) {
	logger, buf := newCapturingLogger()
	app := newAppWithAccessLog(logger, true)
	app.Get("/x", func(c fiber.Ctx) error { return c.SendString("ok") })

	const requestID = "2f1c4b4e-0f5b-4d6a-9c2e-4a1d3b5c6d7e"
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("X-Request-ID", requestID)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	_ = resp.Body.Close()

	var rec map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
		t.Fatalf("log line is not JSON: %q", buf.String())
	}
	if rec["threadId"] != requestID {
		t.Errorf("threadId = %v, want the inbound X-Request-ID %s", rec["threadId"], requestID)
	}
	if got := resp.Header.Get("X-Request-ID"); got != requestID {
		t.Errorf("echoed X-Request-ID = %q, want %q", got, requestID)
	}
}

// TestAccessLog_RecordsFinalStatusOfAnErrorRoute is the reason the middleware
// calls the ErrorHandler itself: read before that, the response is still 200.
// An unclassified fiber.Error is 500 by ErrorHandler policy (documented there),
// so the FINAL status this record must carry is 500 — never the 200 that was
// on the response when c.Next() returned.
func TestAccessLog_RecordsFinalStatusOfAnErrorRoute(t *testing.T) {
	logger, buf := newCapturingLogger()
	app := newAppWithAccessLog(logger, true)
	app.Get("/boom", func(c fiber.Ctx) error {
		return fiber.NewError(fiber.StatusTeapot, "teapot")
	})

	rec := accessLogRecord(t, app, buf, "GET", "/boom")

	if rec["status"] != float64(fiber.StatusInternalServerError) {
		t.Errorf("status = %v, want the FINAL 500, not the pre-ErrorHandler 200", rec["status"])
	}
	if rec["err"] != "teapot" {
		t.Errorf("err = %v, want the chain error text", rec["err"])
	}
	if rec["level"] != "WARN" {
		t.Errorf("level = %v, want WARN for a 5xx", rec["level"])
	}
}

// TestAccessLog_UnroutedRequestIsLoggedOnce covers the 404 that produced the
// favicon lines in the boot log: no route matches, the ErrorHandler renders
// the envelope, and exactly one access record comes out.
func TestAccessLog_UnroutedRequestIsLoggedOnce(t *testing.T) {
	logger, buf := newCapturingLogger()
	app := newAppWithAccessLog(logger, true)
	app.Get("/known", func(c fiber.Ctx) error { return c.SendString("ok") })

	rec := accessLogRecord(t, app, buf, "GET", "/favicon.ico")

	if rec["status"] != float64(fiber.StatusNotFound) {
		t.Errorf("status = %v, want 404", rec["status"])
	}
	if rec["path"] != "/favicon.ico" {
		t.Errorf("path = %v, want /favicon.ico", rec["path"])
	}
	if rec["level"] != "INFO" {
		t.Errorf("level = %v, want INFO for a 404", rec["level"])
	}
	// No route matched, so none is reported — Fiber's context still points at
	// the last middleware entry it examined, which would mislabel this as "/".
	if _, present := rec["route"]; present {
		t.Errorf("route should be absent on an unrouted request, got %v", rec["route"])
	}
}

// TestAccessLog_PanicStillLogged is why the middleware is registered OUTSIDE
// Recover: a panicking request used to leave no access record at all, because
// the unwind skipped the logging that came after c.Next().
func TestAccessLog_PanicStillLogged(t *testing.T) {
	logger, buf := newCapturingLogger()
	app := newAppWithAccessLog(logger, true)
	app.Get("/panic", func(c fiber.Ctx) error { panic("boom") })

	rec := accessLogRecord(t, app, buf, "GET", "/panic")

	if rec["status"] != float64(fiber.StatusInternalServerError) {
		t.Errorf("status = %v, want 500", rec["status"])
	}
	if rec["level"] != "WARN" {
		t.Errorf("level = %v, want WARN for a 5xx", rec["level"])
	}
	if rec["path"] != "/panic" {
		t.Errorf("path = %v, want /panic", rec["path"])
	}
}

// TestAccessLog_WithoutAppContextOmitsThreadID proves the middleware degrades
// instead of inventing an id that appears in no other record.
func TestAccessLog_WithoutAppContextOmitsThreadID(t *testing.T) {
	logger, buf := newCapturingLogger()
	app := newAppWithAccessLog(logger, false)
	app.Get("/x", func(c fiber.Ctx) error { return c.SendString("ok") })

	rec := accessLogRecord(t, app, buf, "GET", "/x")

	if _, present := rec["threadId"]; present {
		t.Errorf("threadId should be absent with no AppContext, got %v", rec["threadId"])
	}
	if rec["status"] != float64(fiber.StatusOK) {
		t.Errorf("status = %v, want 200", rec["status"])
	}
}

// TestAccessLog_NilLoggerFallsBackToDefault keeps the middleware usable from a
// consumer that has not built a logger yet.
func TestAccessLog_NilLoggerFallsBackToDefault(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	pipe := newTestPipeline()
	app := fiber.New(fiber.Config{ErrorHandler: ErrorHandler(pipe)})
	app.Use(AccessLog(nil))
	app.Get("/x", func(c fiber.Ctx) error { return c.SendString("ok") })

	resp, err := app.Test(httptest.NewRequest("GET", "/x", nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	_ = resp.Body.Close()

	if !bytes.Contains(buf.Bytes(), []byte(accessLogMessage)) {
		t.Errorf("expected the default logger to receive the record, log was: %s", buf.String())
	}
}

// TestAccessLog_ErrorHandlerFailureFallsBackTo500 covers the guard the
// middleware inherits from Fiber's logger: when the app's ErrorHandler itself
// fails, the response must still get a status before the record is written —
// otherwise the log would report the pre-error 200 for a request the client
// never got an answer to.
func TestAccessLog_ErrorHandlerFailureFallsBackTo500(t *testing.T) {
	logger, buf := newCapturingLogger()
	app := fiber.New(fiber.Config{
		ErrorHandler: func(fiber.Ctx, error) error { return errors.New("error handler failed") },
	})
	app.Use(AccessLog(logger))
	app.Get("/boom", func(c fiber.Ctx) error { return errors.New("handler failed") })

	rec := accessLogRecord(t, app, buf, "GET", "/boom")

	if rec["status"] != float64(fiber.StatusInternalServerError) {
		t.Errorf("status = %v, want the 500 fallback", rec["status"])
	}
	if rec["level"] != "WARN" {
		t.Errorf("level = %v, want WARN", rec["level"])
	}
}
