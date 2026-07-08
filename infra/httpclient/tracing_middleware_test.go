package httpclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// tracingMiddleware runs on the globally registered tracer — a no-op provider
// by default — so the span lifecycle (start, header injection point, error
// recording, 5xx status) is exercisable without an OTel exporter.

func TestTracingMiddleware_SuccessErrorAnd5xx(t *testing.T) {
	mw := tracingMiddleware("payments", "getInvoice")

	mkReq := func() *http.Request { return httptest.NewRequest("GET", "http://svc/invoices/1", nil) }

	t.Run("success", func(t *testing.T) {
		next := rtFunc(func(context.Context, *http.Request, *observation, roundTripper) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Status: "200 OK"}, nil
		})
		resp, err := mw.RoundTrip(context.Background(), mkReq(), &observation{}, next)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("success case: %v, %v", resp, err)
		}
	})
	t.Run("transportError", func(t *testing.T) {
		boom := errors.New("dial refused")
		next := rtFunc(func(context.Context, *http.Request, *observation, roundTripper) (*http.Response, error) {
			return nil, boom
		})
		if _, err := mw.RoundTrip(context.Background(), mkReq(), &observation{}, next); !errors.Is(err, boom) {
			t.Fatalf("expected the transport error back, got %v", err)
		}
	})
	t.Run("serverFailure5xx", func(t *testing.T) {
		next := rtFunc(func(context.Context, *http.Request, *observation, roundTripper) (*http.Response, error) {
			return &http.Response{StatusCode: 503, Status: "503 Service Unavailable"}, nil
		})
		resp, err := mw.RoundTrip(context.Background(), mkReq(), &observation{}, next)
		if err != nil || resp.StatusCode != 503 {
			t.Fatalf("5xx case: %v, %v", resp, err)
		}
	})
}
