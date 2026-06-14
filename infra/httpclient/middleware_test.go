package httpclient

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
)

// terminal returns the supplied response (or error) and records that it was
// reached so tests can assert short-circuit behavior.
func terminal(resp *http.Response, err error, reachedFlag *bool) roundTripper {
	return rtFunc(func(ctx context.Context, req *http.Request, obs *observation, _ roundTripper) (*http.Response, error) {
		if reachedFlag != nil {
			*reachedFlag = true
		}
		return resp, err
	})
}

func TestChain_Empty_Errors(t *testing.T) {
	c := newChain()
	_, err := c.dispatch(context.Background(), &http.Request{}, &observation{})
	if err == nil || !strings.Contains(err.Error(), "empty middleware chain") {
		t.Errorf("expected empty chain error, got %v", err)
	}
}

func TestChain_NoTerminal_Errors(t *testing.T) {
	// Layer always calls next; terminal layer is missing.
	layer := rtFunc(func(ctx context.Context, req *http.Request, obs *observation, next roundTripper) (*http.Response, error) {
		return next.RoundTrip(ctx, req, obs, nil)
	})
	c := newChain(layer)
	_, err := c.dispatch(context.Background(), &http.Request{}, &observation{})
	if err == nil || !strings.Contains(err.Error(), "without terminal layer") {
		t.Errorf("expected non-terminal chain error, got %v", err)
	}
}

func TestChain_Order_OutermostFirst(t *testing.T) {
	order := []string{}
	tag := func(name string) rtFunc {
		return func(ctx context.Context, req *http.Request, obs *observation, next roundTripper) (*http.Response, error) {
			order = append(order, "pre:"+name)
			resp, err := next.RoundTrip(ctx, req, obs, nil)
			order = append(order, "post:"+name)
			return resp, err
		}
	}
	resp := &http.Response{StatusCode: 200, Body: http.NoBody}
	c := newChain(tag("A"), tag("B"), terminal(resp, nil, nil))
	if _, err := c.dispatch(context.Background(), &http.Request{}, &observation{}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	want := []string{"pre:A", "pre:B", "post:B", "post:A"}
	for i, v := range want {
		if order[i] != v {
			t.Errorf("step %d = %q, want %q (order=%v)", i, order[i], v, order)
		}
	}
}

func TestChain_ShortCircuit_StopsBeforeTerminal(t *testing.T) {
	reachedTerminal := false
	cached := rtFunc(func(ctx context.Context, req *http.Request, obs *observation, _ roundTripper) (*http.Response, error) {
		obs.CacheStatus = "hit"
		return &http.Response{StatusCode: 200, Body: http.NoBody}, nil
	})
	c := newChain(cached, terminal(nil, nil, &reachedTerminal))
	obs := &observation{}
	if _, err := c.dispatch(context.Background(), &http.Request{}, obs); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if reachedTerminal {
		t.Error("terminal should not be invoked when an earlier layer short-circuits")
	}
	if obs.CacheStatus != "hit" {
		t.Errorf("obs.CacheStatus = %q, want hit", obs.CacheStatus)
	}
}

func TestChain_ErrorPropagation(t *testing.T) {
	wantErr := errors.New("boom")
	c := newChain(terminal(nil, wantErr, nil))
	_, err := c.dispatch(context.Background(), &http.Request{}, &observation{})
	if !errors.Is(err, wantErr) {
		t.Errorf("expected wrapped error %v, got %v", wantErr, err)
	}
}

func TestCorrelationMiddleware_InjectsHeaders(t *testing.T) {
	svc := &serviceClient{name: "s", threadID: "X-Thread-Id", requestID: "X-Request-ID"}
	req := &http.Request{Header: http.Header{}}
	obs := &observation{}

	appCtx := configuration.NewAppContextWithRandomID(configuration.LangPTBR)
	want := appCtx.ID().String()

	resp := &http.Response{StatusCode: 200, Body: http.NoBody}
	c := newChain(correlationMiddleware(svc), terminal(resp, nil, nil))
	if _, err := c.dispatch(appCtx, req, obs); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if got := req.Header.Get("X-Thread-Id"); got != want {
		t.Errorf("X-Thread-Id = %q, want %q", got, want)
	}
	if got := req.Header.Get("X-Request-ID"); got != want {
		t.Errorf("X-Request-ID = %q, want %q", got, want)
	}
	if obs.ThreadID != want {
		t.Errorf("obs.ThreadID = %q, want %q", obs.ThreadID, want)
	}
	if obs.threadIDHeader != "X-Thread-Id" {
		t.Errorf("obs.threadIDHeader = %q, want X-Thread-Id", obs.threadIDHeader)
	}
}

func TestCorrelationMiddleware_NoIdContext_LeavesHeaders(t *testing.T) {
	svc := &serviceClient{name: "s", threadID: "X-Thread-Id", requestID: "X-Request-ID"}
	req := &http.Request{Header: http.Header{}}
	obs := &observation{}

	resp := &http.Response{StatusCode: 200, Body: http.NoBody}
	c := newChain(correlationMiddleware(svc), terminal(resp, nil, nil))
	if _, err := c.dispatch(context.Background(), req, obs); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if req.Header.Get("X-Thread-Id") != "" {
		t.Errorf("X-Thread-Id should remain empty when context has no ID")
	}
}

func TestCorrelationMiddleware_RespectsExistingHeader(t *testing.T) {
	svc := &serviceClient{name: "s", threadID: "X-Thread-Id", requestID: "X-Request-ID"}
	req := &http.Request{Header: http.Header{}}
	req.Header.Set("X-Thread-Id", "preset")
	obs := &observation{}

	appCtx := configuration.NewAppContextWithRandomID(configuration.LangPTBR)
	resp := &http.Response{StatusCode: 200, Body: http.NoBody}
	c := newChain(correlationMiddleware(svc), terminal(resp, nil, nil))
	if _, err := c.dispatch(appCtx, req, obs); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if got := req.Header.Get("X-Thread-Id"); got != "preset" {
		t.Errorf("existing header should not be overwritten; got %q", got)
	}
}
