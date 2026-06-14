package httpclient_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/infra/httpclient"
	"github.com/ClaudioSchirmer/omnicore/infra/httpclient/binding"
)

type fakeGetUserRequest struct {
	ID string `http:"path,id"`
}

type fakeGetUserResponse struct {
	ID    string `json:"id"`
	Email string `json:"email"`
}

type fakeListUsersRequest struct {
	Tenant string `http:"query,tenant"`
	Tag    string `http:"header,X-Tag"`
}

type fakeListUsersResponse struct {
	Items []fakeGetUserResponse `json:"items"`
}

type fakeCreateUserRequest struct {
	Body fakeGetUserResponse `http:"body,json"`
}

func registerGetUser(f *httpclient.Fake) {
	f.Register("kc", "fetchUser", binding.EndpointMeta{
		Method:        http.MethodGet,
		Path:          "/users/{id}",
		RequestCodec:  "json",
		ResponseCodec: "json",
	})
}

func TestFake_ReturnsTypedResponse(t *testing.T) {
	fake := httpclient.NewFake()
	registerGetUser(fake)
	fake.WhenCalled("kc", "fetchUser").
		Return(fakeGetUserResponse{ID: "abc", Email: "x@y"})

	got, err := httpclient.Call[fakeGetUserRequest, fakeGetUserResponse](
		context.Background(), fake.Client(),
		"kc", "fetchUser", fakeGetUserRequest{ID: "abc"},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ID != "abc" || got.Email != "x@y" {
		t.Fatalf("unexpected payload: %#v", got)
	}
}

func TestFake_ReturnsErrorStatus(t *testing.T) {
	fake := httpclient.NewFake()
	registerGetUser(fake)
	fake.WhenCalled("kc", "fetchUser").
		ReturnError(http.StatusInternalServerError, []byte(`{"msg":"boom"}`))

	_, err := httpclient.Call[fakeGetUserRequest, fakeGetUserResponse](
		context.Background(), fake.Client(),
		"kc", "fetchUser", fakeGetUserRequest{ID: "abc"},
	)
	var he *httpclient.HttpError
	if !errors.As(err, &he) {
		t.Fatalf("expected *HttpError, got %T: %v", err, err)
	}
	if he.Status != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", he.Status)
	}
	if string(he.Body) != `{"msg":"boom"}` {
		t.Fatalf("unexpected body: %s", he.Body)
	}
}

func TestFake_ReturnsTransportError(t *testing.T) {
	fake := httpclient.NewFake()
	registerGetUser(fake)
	sentinel := errors.New("simulated dial failure")
	fake.WhenCalled("kc", "fetchUser").ReturnTransportError(sentinel)

	_, err := httpclient.Call[fakeGetUserRequest, fakeGetUserResponse](
		context.Background(), fake.Client(),
		"kc", "fetchUser", fakeGetUserRequest{ID: "abc"},
	)
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel to wrap, got %v", err)
	}
}

func TestFake_UnstubbedCallFails(t *testing.T) {
	fake := httpclient.NewFake()

	_, err := httpclient.Call[fakeGetUserRequest, fakeGetUserResponse](
		context.Background(), fake.Client(),
		"kc", "fetchUser", fakeGetUserRequest{ID: "abc"},
	)
	if !errors.Is(err, httpclient.ErrFakeUnstubbed) {
		t.Fatalf("expected ErrFakeUnstubbed, got %v", err)
	}
}

func TestFake_MatchPath(t *testing.T) {
	fake := httpclient.NewFake()
	registerGetUser(fake)
	fake.WhenCalled("kc", "fetchUser").
		MatchPath("id", "abc").Always().
		Return(fakeGetUserResponse{ID: "abc", Email: "a@b"})

	if _, err := httpclient.Call[fakeGetUserRequest, fakeGetUserResponse](
		context.Background(), fake.Client(),
		"kc", "fetchUser", fakeGetUserRequest{ID: "abc"},
	); err != nil {
		t.Fatalf("matching call failed: %v", err)
	}

	_, err := httpclient.Call[fakeGetUserRequest, fakeGetUserResponse](
		context.Background(), fake.Client(),
		"kc", "fetchUser", fakeGetUserRequest{ID: "xyz"},
	)
	if !errors.Is(err, httpclient.ErrFakeUnstubbed) {
		t.Fatalf("non-matching call should be unstubbed, got %v", err)
	}
}

func TestFake_MatchQuery(t *testing.T) {
	fake := httpclient.NewFake()
	fake.Register("kc", "listUsers", binding.EndpointMeta{
		Method: http.MethodGet, Path: "/users",
	})
	fake.WhenCalled("kc", "listUsers").
		MatchQuery("tenant", "acme").
		Return(fakeListUsersResponse{Items: []fakeGetUserResponse{{ID: "a"}}})

	got, err := httpclient.Call[fakeListUsersRequest, fakeListUsersResponse](
		context.Background(), fake.Client(),
		"kc", "listUsers", fakeListUsersRequest{Tenant: "acme"},
	)
	if err != nil {
		t.Fatalf("matching call failed: %v", err)
	}
	if len(got.Items) != 1 || got.Items[0].ID != "a" {
		t.Fatalf("unexpected payload: %#v", got)
	}

	_, err = httpclient.Call[fakeListUsersRequest, fakeListUsersResponse](
		context.Background(), fake.Client(),
		"kc", "listUsers", fakeListUsersRequest{Tenant: "other"},
	)
	if !errors.Is(err, httpclient.ErrFakeUnstubbed) {
		t.Fatalf("non-matching query should be unstubbed, got %v", err)
	}
}

func TestFake_MatchHeader(t *testing.T) {
	fake := httpclient.NewFake()
	fake.Register("kc", "listUsers", binding.EndpointMeta{
		Method: http.MethodGet, Path: "/users",
	})
	fake.WhenCalled("kc", "listUsers").
		MatchHeader("X-Tag", "alpha").
		Return(fakeListUsersResponse{})

	if _, err := httpclient.Call[fakeListUsersRequest, fakeListUsersResponse](
		context.Background(), fake.Client(),
		"kc", "listUsers", fakeListUsersRequest{Tag: "alpha"},
	); err != nil {
		t.Fatalf("matching header call failed: %v", err)
	}
	_, err := httpclient.Call[fakeListUsersRequest, fakeListUsersResponse](
		context.Background(), fake.Client(),
		"kc", "listUsers", fakeListUsersRequest{Tag: "beta"},
	)
	if !errors.Is(err, httpclient.ErrFakeUnstubbed) {
		t.Fatalf("non-matching header should be unstubbed, got %v", err)
	}
}

func TestFake_RecordsCalls(t *testing.T) {
	fake := httpclient.NewFake()
	registerGetUser(fake)
	fake.WhenCalled("kc", "fetchUser").Always().
		Return(fakeGetUserResponse{ID: "abc"})

	for _, id := range []string{"a", "b", "c"} {
		if _, err := httpclient.Call[fakeGetUserRequest, fakeGetUserResponse](
			context.Background(), fake.Client(),
			"kc", "fetchUser", fakeGetUserRequest{ID: id},
		); err != nil {
			t.Fatalf("call failed: %v", err)
		}
	}
	calls := fake.Calls("kc", "fetchUser")
	if len(calls) != 3 {
		t.Fatalf("expected 3 recorded calls, got %d", len(calls))
	}
	if got := calls[1].Path["id"]; got != "b" {
		t.Fatalf("call[1].Path[id]=%q want b", got)
	}
}

func TestFake_TimesLimitsMatches(t *testing.T) {
	fake := httpclient.NewFake()
	registerGetUser(fake)
	fake.WhenCalled("kc", "fetchUser").Times(2).
		Return(fakeGetUserResponse{ID: "abc"})

	for i := 0; i < 2; i++ {
		if _, err := httpclient.Call[fakeGetUserRequest, fakeGetUserResponse](
			context.Background(), fake.Client(),
			"kc", "fetchUser", fakeGetUserRequest{ID: "abc"},
		); err != nil {
			t.Fatalf("call %d failed: %v", i, err)
		}
	}
	_, err := httpclient.Call[fakeGetUserRequest, fakeGetUserResponse](
		context.Background(), fake.Client(),
		"kc", "fetchUser", fakeGetUserRequest{ID: "abc"},
	)
	if !errors.Is(err, httpclient.ErrFakeUnstubbed) {
		t.Fatalf("third call should be unstubbed after Times(2), got %v", err)
	}
}

func TestFake_AssertExpectationsFails(t *testing.T) {
	fake := httpclient.NewFake()
	registerGetUser(fake)
	fake.WhenCalled("kc", "fetchUser").Times(2).
		Return(fakeGetUserResponse{ID: "abc"})

	if _, err := httpclient.Call[fakeGetUserRequest, fakeGetUserResponse](
		context.Background(), fake.Client(),
		"kc", "fetchUser", fakeGetUserRequest{ID: "abc"},
	); err != nil {
		t.Fatalf("call failed: %v", err)
	}
	if err := fake.AssertExpectations(); err == nil {
		t.Fatalf("expected AssertExpectations to report a mismatch (1/2 calls)")
	}
}

func TestFake_AssertExpectationsPasses(t *testing.T) {
	fake := httpclient.NewFake()
	registerGetUser(fake)
	fake.WhenCalled("kc", "fetchUser").Times(1).
		Return(fakeGetUserResponse{ID: "abc"})

	if _, err := httpclient.Call[fakeGetUserRequest, fakeGetUserResponse](
		context.Background(), fake.Client(),
		"kc", "fetchUser", fakeGetUserRequest{ID: "abc"},
	); err != nil {
		t.Fatalf("call failed: %v", err)
	}
	if err := fake.AssertExpectations(); err != nil {
		t.Fatalf("AssertExpectations failed unexpectedly: %v", err)
	}
}

func TestFake_Reset(t *testing.T) {
	fake := httpclient.NewFake()
	registerGetUser(fake)
	fake.WhenCalled("kc", "fetchUser").Always().
		Return(fakeGetUserResponse{ID: "abc"})

	if _, err := httpclient.Call[fakeGetUserRequest, fakeGetUserResponse](
		context.Background(), fake.Client(),
		"kc", "fetchUser", fakeGetUserRequest{ID: "abc"},
	); err != nil {
		t.Fatalf("call failed: %v", err)
	}
	if len(fake.Calls("kc", "fetchUser")) != 1 {
		t.Fatalf("expected 1 recorded call before reset")
	}
	fake.Reset()
	if len(fake.Calls("kc", "fetchUser")) != 0 {
		t.Fatalf("expected 0 recorded calls after reset")
	}
	_, err := httpclient.Call[fakeGetUserRequest, fakeGetUserResponse](
		context.Background(), fake.Client(),
		"kc", "fetchUser", fakeGetUserRequest{ID: "abc"},
	)
	if !errors.Is(err, httpclient.ErrFakeUnstubbed) {
		t.Fatalf("after reset, call should be unstubbed, got %v", err)
	}
}

func TestFake_BindingTagsAreExercised(t *testing.T) {
	fake := httpclient.NewFake()
	registerGetUser(fake)
	fake.WhenCalled("kc", "fetchUser").Always().
		Return(fakeGetUserResponse{ID: "abc"})

	if _, err := httpclient.Call[fakeGetUserRequest, fakeGetUserResponse](
		context.Background(), fake.Client(),
		"kc", "fetchUser", fakeGetUserRequest{ID: "abc"},
	); err != nil {
		t.Fatalf("call failed: %v", err)
	}
	calls := fake.Calls("kc", "fetchUser")
	if len(calls) != 1 {
		t.Fatalf("expected 1 recorded call")
	}
	want := "http://fake.invalid/users/abc"
	if calls[0].URL != want {
		t.Fatalf("URL=%q want %q", calls[0].URL, want)
	}
	if calls[0].Path["id"] != "abc" {
		t.Fatalf("Path[id]=%q want abc", calls[0].Path["id"])
	}
}

func TestFake_JSONBodyRoundTrip(t *testing.T) {
	fake := httpclient.NewFake()
	fake.Register("kc", "createUser", binding.EndpointMeta{
		Method: http.MethodPost, Path: "/users",
	})
	fake.WhenCalled("kc", "createUser").
		Return(fakeGetUserResponse{ID: "abc", Email: "x@y"})

	got, err := httpclient.Call[fakeCreateUserRequest, fakeGetUserResponse](
		context.Background(), fake.Client(),
		"kc", "createUser",
		fakeCreateUserRequest{Body: fakeGetUserResponse{ID: "abc", Email: "x@y"}},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ID != "abc" {
		t.Fatalf("got=%#v", got)
	}
	calls := fake.Calls("kc", "createUser")
	if len(calls) != 1 {
		t.Fatalf("expected 1 recorded call")
	}
	if string(calls[0].Body) == "" {
		t.Fatalf("expected request body to be captured")
	}
}

func TestFake_WithExtraHeaderPropagated(t *testing.T) {
	fake := httpclient.NewFake()
	registerGetUser(fake)
	fake.WhenCalled("kc", "fetchUser").Always().
		Return(fakeGetUserResponse{ID: "abc"})

	if _, err := httpclient.Call[fakeGetUserRequest, fakeGetUserResponse](
		context.Background(), fake.Client(),
		"kc", "fetchUser", fakeGetUserRequest{ID: "abc"},
		httpclient.WithExtraHeader("X-Tenant", "acme"),
	); err != nil {
		t.Fatalf("call failed: %v", err)
	}
	calls := fake.Calls("kc", "fetchUser")
	if got := calls[0].Headers.Get("X-Tenant"); got != "acme" {
		t.Fatalf("X-Tenant=%q want acme", got)
	}
}
