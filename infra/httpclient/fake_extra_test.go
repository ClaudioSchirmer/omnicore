package httpclient_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/infra/httpclient"
	"github.com/ClaudioSchirmer/omnicore/infra/httpclient/binding"
)

// --- ReturnBytes --------------------------------------------------------

func TestFake_ReturnBytes_RawBody(t *testing.T) {
	fake := httpclient.NewFake()
	registerGetUser(fake)
	fake.WhenCalled("kc", "fetchUser").
		ReturnBytes([]byte(`{"id":"raw","email":"r@x"}`), "application/json")

	got, err := httpclient.Call[fakeGetUserRequest, fakeGetUserResponse](
		context.Background(), fake.Client(),
		"kc", "fetchUser", fakeGetUserRequest{ID: "raw"},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ID != "raw" || got.Email != "r@x" {
		t.Fatalf("unexpected payload: %#v", got)
	}
}

// --- Status override ----------------------------------------------------

func TestFake_Status_OverridesCode(t *testing.T) {
	fake := httpclient.NewFake()
	registerGetUser(fake)
	// Return defaults to 200; Status(202) overrides while keeping a decodable body.
	fake.WhenCalled("kc", "fetchUser").
		Status(http.StatusAccepted).
		Return(fakeGetUserResponse{ID: "abc"})

	got, err := httpclient.Call[fakeGetUserRequest, fakeGetUserResponse](
		context.Background(), fake.Client(),
		"kc", "fetchUser", fakeGetUserRequest{ID: "abc"},
	)
	if err != nil {
		t.Fatalf("202 is a 2xx success; got error %v", err)
	}
	if got.ID != "abc" {
		t.Fatalf("payload mismatch: %#v", got)
	}
}

func TestFake_Status_NonSuccessYieldsHttpError(t *testing.T) {
	fake := httpclient.NewFake()
	registerGetUser(fake)
	fake.WhenCalled("kc", "fetchUser").
		Status(http.StatusTeapot).
		ReturnBytes([]byte(`teapot`), "text/plain")

	_, err := httpclient.Call[fakeGetUserRequest, fakeGetUserResponse](
		context.Background(), fake.Client(),
		"kc", "fetchUser", fakeGetUserRequest{ID: "abc"},
	)
	var he *httpclient.HttpError
	if !errors.As(err, &he) {
		t.Fatalf("expected *HttpError, got %T: %v", err, err)
	}
	if he.Status != http.StatusTeapot {
		t.Fatalf("status = %d, want 418", he.Status)
	}
}

// --- WithHeader ---------------------------------------------------------

func TestFake_WithHeader_OnResponse(t *testing.T) {
	fake := httpclient.NewFake()
	fake.Register("kc", "raw", binding.EndpointMeta{
		Method: http.MethodGet, Path: "/raw",
	})
	// A response struct with no tagged fields decodes the whole body; use a
	// header-carrying error response to assert the header surfaces on HttpError.
	fake.WhenCalled("kc", "raw").
		Status(http.StatusServiceUnavailable).
		WithHeader("Retry-After", "30").
		ReturnBytes([]byte(`down`), "text/plain")

	type emptyReq struct{}
	type emptyResp struct{}
	_, err := httpclient.Call[emptyReq, emptyResp](
		context.Background(), fake.Client(), "kc", "raw", emptyReq{},
	)
	var he *httpclient.HttpError
	if !errors.As(err, &he) {
		t.Fatalf("expected *HttpError, got %T: %v", err, err)
	}
	if got := he.Headers.Get("Retry-After"); got != "30" {
		t.Fatalf("Retry-After header = %q, want 30", got)
	}
}

// --- WithFakeLogger -----------------------------------------------------

func TestNewFake_WithFakeLogger(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	// Non-nil logger is installed; nil option and nil logger are tolerated.
	fake := httpclient.NewFake(
		httpclient.WithFakeLogger(logger),
		httpclient.WithFakeLogger(nil),
		nil,
	)
	registerGetUser(fake)
	fake.WhenCalled("kc", "fetchUser").Return(fakeGetUserResponse{ID: "abc"})

	if _, err := httpclient.Call[fakeGetUserRequest, fakeGetUserResponse](
		context.Background(), fake.Client(),
		"kc", "fetchUser", fakeGetUserRequest{ID: "abc"},
	); err != nil {
		t.Fatalf("call failed: %v", err)
	}
	// The fake short-circuits the middleware chain, so it need not log; the
	// assertion here is simply that a custom logger is accepted without panic
	// and the call still succeeds.
}
