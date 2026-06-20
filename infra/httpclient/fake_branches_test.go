package httpclient

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/infra/httpclient/binding"
)

// ─── normalizeFakeMeta: empty method + empty path defaults ───────────────────

func TestNormalizeFakeMeta_Defaults(t *testing.T) {
	out := normalizeFakeMeta("listThings", binding.EndpointMeta{}) // empty method+path
	if out.Method != http.MethodGet {
		t.Errorf("method = %q, want GET", out.Method)
	}
	if out.Path != "/listThings" {
		t.Errorf("path = %q, want /listThings", out.Path)
	}
	if out.RequestCodec != "json" || out.ResponseCodec != "json" {
		t.Errorf("codecs = %q/%q, want json/json", out.RequestCodec, out.ResponseCodec)
	}
}

// ─── extractPathParams: non-matching path + unescape fallback ────────────────

func TestExtractPathParams_NoMatchReturnsNil(t *testing.T) {
	if got := extractPathParams("/users/{id}", "/totally/different"); got != nil {
		t.Errorf("expected nil for a non-matching path, got %v", got)
	}
}

func TestExtractPathParams_UnescapeFallback(t *testing.T) {
	// "%zz" is not a valid percent escape, so PathUnescape fails and the raw
	// value is kept.
	got := extractPathParams("/u/{id}", "/u/%zz")
	if got["id"] != "%zz" {
		t.Errorf("id = %q, want raw %%zz on unescape failure", got["id"])
	}
}

// ─── buildFakeResponse: zero status, nil headers, marshal error ──────────────

func TestBuildFakeResponse_ZeroStatusAndNilHeaders(t *testing.T) {
	req, _ := http.NewRequest("GET", "http://fake/x", nil)
	resp, body, err := buildFakeResponse(&FakeStub{}, req) // status 0, nil headers, no body
	if err != nil {
		t.Fatalf("buildFakeResponse: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want defaulted 200", resp.StatusCode)
	}
	if resp.Header == nil {
		t.Error("headers must be initialized to a non-nil map")
	}
	if len(body) != 0 {
		t.Errorf("body = %q, want empty", body)
	}
}

func TestBuildFakeResponse_MarshalError(t *testing.T) {
	req, _ := http.NewRequest("GET", "http://fake/x", nil)
	stub := &FakeStub{usesValueBody: true, returnsValue: make(chan int)} // chan is unmarshalable
	if _, _, err := buildFakeResponse(stub, req); err == nil {
		t.Fatal("expected json marshal error for an unmarshalable return value")
	}
}

// ─── Call-driven fake branches ───────────────────────────────────────────────

type fbReq struct {
	ID string `http:"path,id"`
}
type fbResp struct {
	Name string `json:"name"`
}

func TestFake_WhenCalled_AutoRegistersDefaultSpec(t *testing.T) {
	fake := NewFake()
	// No Register — WhenCalled must auto-register the default GET /{endpoint}.
	fake.WhenCalled("svc", "ping").Return(fbResp{Name: "ok"})
	out, err := Call[struct{}, fbResp](context.Background(), fake.Client(), "svc", "ping", struct{}{})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if out.Name != "ok" {
		t.Errorf("name = %q, want ok", out.Name)
	}
}

func TestFake_AssertExpectations_SkipsAlways(t *testing.T) {
	fake := NewFake()
	fake.WhenCalled("svc", "ping").Always().Return(fbResp{Name: "x"})
	// Never called; Always() stubs are exempt from the expectation check.
	if err := fake.AssertExpectations(); err != nil {
		t.Fatalf("Always() stub must be exempt, got %v", err)
	}
}

func TestFake_Times_ClampsBelowOne(t *testing.T) {
	fake := NewFake()
	stub := fake.WhenCalled("svc", "ping").Times(0).Return(fbResp{Name: "x"})
	if stub.times != 1 {
		t.Errorf("Times(0) should clamp to 1, got %d", stub.times)
	}
}

func TestFake_MatchQuery_NoMatchFallsThrough(t *testing.T) {
	fake := NewFake()
	fake.WhenCalled("svc", "ping").MatchQuery("tenant", "acme").Always().Return(fbResp{Name: "x"})
	// The call carries no ?tenant=acme, so the MatchQuery predicate returns
	// false and no stub matches → unstubbed error.
	_, err := Call[struct{}, fbResp](context.Background(), fake.Client(), "svc", "ping", struct{}{})
	if !errors.Is(err, ErrFakeUnstubbed) {
		t.Fatalf("expected ErrFakeUnstubbed when MatchQuery does not match, got %v", err)
	}
}

func TestFake_MatchHeader_NoMatchFallsThrough(t *testing.T) {
	fake := NewFake()
	fake.WhenCalled("svc", "ping").MatchHeader("X-Tenant", "acme").Always().Return(fbResp{Name: "x"})
	_, err := Call[struct{}, fbResp](context.Background(), fake.Client(), "svc", "ping", struct{}{})
	if !errors.Is(err, ErrFakeUnstubbed) {
		t.Fatalf("expected ErrFakeUnstubbed when MatchHeader does not match, got %v", err)
	}
}

func TestFake_NilContextDefaultsToBackground(t *testing.T) {
	fake := NewFake()
	fake.WhenCalled("svc", "ping").Return(fbResp{Name: "ok"})
	//nolint:staticcheck // intentionally passing a nil context to exercise the guard
	out, err := Call[struct{}, fbResp](nil, fake.Client(), "svc", "ping", struct{}{})
	if err != nil {
		t.Fatalf("nil ctx should default to Background, got %v", err)
	}
	if out.Name != "ok" {
		t.Errorf("name = %q, want ok", out.Name)
	}
}

// fbBadReq is used only here so the binding plan cache (keyed by type+role)
// inspects it for the first time against a placeholder path.
type fbBadReq struct {
	Tenant string `http:"query,tenant"`
}

func TestFake_BuildRequestError(t *testing.T) {
	fake := NewFake()
	// Endpoint declares a {id} placeholder, but the request type has no
	// matching http:"path,id" field → binding.BuildRequest fails.
	fake.Register("svc", "get", binding.EndpointMeta{
		Method: http.MethodGet, Path: "/things/{id}", RequestCodec: "json", ResponseCodec: "json",
	})
	fake.WhenCalled("svc", "get").Return(fbResp{Name: "x"})
	_, err := Call[fbBadReq, fbResp](context.Background(), fake.Client(), "svc", "get", fbBadReq{Tenant: "acme"})
	if !errors.Is(err, ErrRequestBuild) {
		t.Fatalf("expected ErrRequestBuild for a missing path field, got %v", err)
	}
}

func TestFake_ReturnUnmarshalableYieldsDecodeError(t *testing.T) {
	fake := NewFake()
	fake.WhenCalled("svc", "ping").Return(make(chan int)) // chan is unmarshalable
	_, err := Call[struct{}, fbResp](context.Background(), fake.Client(), "svc", "ping", struct{}{})
	if !errors.Is(err, ErrResponseDecode) {
		t.Fatalf("expected ErrResponseDecode when the stub value cannot marshal, got %v", err)
	}
}

func TestFake_DecodeResponseErrorOn2xx(t *testing.T) {
	fake := NewFake()
	fake.WhenCalled("svc", "ping").ReturnBytes([]byte(`{not valid json`), "application/json")
	_, err := Call[struct{}, fbResp](context.Background(), fake.Client(), "svc", "ping", struct{}{})
	if !errors.Is(err, ErrResponseDecode) {
		t.Fatalf("expected ErrResponseDecode on malformed 2xx body, got %v", err)
	}
}

func TestFake_AcceptableStatusDecodesBody(t *testing.T) {
	fake := NewFake()
	fake.WhenCalled("svc", "ping").
		Status(http.StatusNotFound).
		ReturnBytes([]byte(`{"name":"absent"}`), "application/json")
	out, err := Call[struct{}, fbResp](context.Background(), fake.Client(), "svc", "ping", struct{}{},
		WithConfig(CallConfig{AcceptableStatus: []int{http.StatusNotFound}}))
	var he *HttpError
	if !errors.As(err, &he) || !he.Acceptable {
		t.Fatalf("expected an acceptable HttpError, got %v", err)
	}
	if out.Name != "absent" {
		t.Errorf("acceptable body should decode, got name=%q", out.Name)
	}
}
