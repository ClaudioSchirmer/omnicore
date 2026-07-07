package grpcclient

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/internal/testpb"
)

const testProcedure = "/omnicore.grpctest.v1.GadgetService/CreateGadget"

// upstream is the controllable fake server: fails the first failN calls
// with failCode, then succeeds echoing selected request headers back.
type upstream struct {
	calls    atomic.Int64
	failN    int64
	failCode connect.Code

	lastAuth atomic.Value // string
	lastKeys chan string  // idempotency keys seen, buffered
	lastTID  atomic.Value // string
}

func newUpstream(failN int64, failCode connect.Code) *upstream {
	return &upstream{failN: failN, failCode: failCode, lastKeys: make(chan string, 16)}
}

func (u *upstream) handler(idemHeader string) (string, *httptest.Server) {
	fn := func(ctx context.Context, req *connect.Request[testpb.CreateGadgetRequest]) (*connect.Response[testpb.CreateGadgetResponse], error) {
		n := u.calls.Add(1)
		u.lastAuth.Store(req.Header().Get("Authorization"))
		u.lastTID.Store(req.Header().Get("Threadid"))
		if idemHeader != "" {
			u.lastKeys <- req.Header().Get(idemHeader)
		}
		if n <= u.failN {
			return nil, connect.NewError(u.failCode, errors.New("upstream failing"))
		}
		return connect.NewResponse(&testpb.CreateGadgetResponse{Id: "ok"}), nil
	}
	h := connect.NewUnaryHandler(testProcedure, fn)
	srv := httptest.NewServer(h)
	return srv.URL, srv
}

func clientFor(t *testing.T, svc ServiceConfig, name string) *Client {
	t.Helper()
	c, err := New(&Config{Services: map[string]ServiceConfig{name: svc}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func call(t *testing.T, c *Client, name string, ctx context.Context) (*connect.Response[testpb.CreateGadgetResponse], error) {
	t.Helper()
	httpClient, err := c.HTTPClient(name)
	if err != nil {
		t.Fatalf("HTTPClient: %v", err)
	}
	baseURL, err := c.BaseURL(name)
	if err != nil {
		t.Fatalf("BaseURL: %v", err)
	}
	copts, err := c.ClientOptions(name)
	if err != nil {
		t.Fatalf("ClientOptions: %v", err)
	}
	cc := connect.NewClient[testpb.CreateGadgetRequest, testpb.CreateGadgetResponse](httpClient, baseURL+testProcedure, clientOptionsToAny(copts)...)
	return cc.CallUnary(ctx, connect.NewRequest(&testpb.CreateGadgetRequest{Name: proto.String("x")}))
}

func clientOptionsToAny(opts []connect.ClientOption) []connect.ClientOption { return opts }

// --- config resolution ---

func TestResolveErrors(t *testing.T) {
	cases := []struct {
		name string
		svc  ServiceConfig
		want string
	}{
		{"noBaseURL", ServiceConfig{}, "baseURL is required"},
		{"badBackoff", ServiceConfig{BaseURL: "http://x", Retry: &RetryConfig{MaxAttempts: 3, Backoff: "warp"}}, "retry.backoff"},
		{"badRetryOn", ServiceConfig{BaseURL: "http://x", Retry: &RetryConfig{MaxAttempts: 3, RetryOn: []string{"nope"}}}, "not a connect code"},
		{"badAuthMode", ServiceConfig{BaseURL: "http://x", Auth: &AuthConfig{Mode: "oauth-dance"}}, "auth.mode"},
		{"staticNoToken", ServiceConfig{BaseURL: "http://x", Auth: &AuthConfig{Mode: "static"}}, "auth.token is required"},
		{"forwardWithToken", ServiceConfig{BaseURL: "http://x", Auth: &AuthConfig{Mode: "forward", Token: "t"}}, "not accepted with mode=forward"},
	}
	for _, tc := range cases {
		_, err := New(&Config{Services: map[string]ServiceConfig{"svc": tc.svc}})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want %q, got %v", tc.name, tc.want, err)
		}
	}
}

func TestDefaultsCascade(t *testing.T) {
	cfg := &Config{
		Defaults: Defaults{
			TimeoutSeconds: 7,
			Retry:          &RetryConfig{MaxAttempts: 3},
			CircuitBreaker: &BreakerConfig{Enabled: true, FailureThreshold: 5, SuccessThreshold: 1, OpenForMS: 100},
		},
		Services: map[string]ServiceConfig{"svc": {BaseURL: "http://x"}},
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s := c.services["svc"]
	if s.cfg.timeout != 7*time.Second {
		t.Fatalf("defaults timeout not applied: %v", s.cfg.timeout)
	}
	if s.cfg.retry.maxAttempts != 3 {
		t.Fatalf("defaults retry not applied")
	}
	if !s.cfg.breakerCfg.Enabled {
		t.Fatalf("defaults breaker not applied")
	}
	if _, ok := s.cfg.retry.retryOn[connect.CodeUnavailable]; !ok {
		t.Fatalf("retryOn default must include unavailable")
	}
}

func TestIdempotencyHeaderDefault(t *testing.T) {
	c := clientFor(t, ServiceConfig{BaseURL: "http://x", Idempotency: &IdempotencyConfig{Enabled: true}}, "svc")
	if got := c.services["svc"].cfg.idempotency.Header; got != defaultIdempotencyHeader {
		t.Fatalf("default header: %q", got)
	}
}

func TestNilConfigAndUnknownService(t *testing.T) {
	c, err := New(nil)
	if err != nil {
		t.Fatalf("nil config: %v", err)
	}
	if _, err := c.HTTPClient("ghost"); err == nil || !strings.Contains(err.Error(), "not declared") {
		t.Fatalf("unknown service: %v", err)
	}
	if _, err := c.BaseURL("ghost"); err == nil {
		t.Fatalf("BaseURL must fail")
	}
	if _, err := c.ClientOptions("ghost"); err == nil {
		t.Fatalf("ClientOptions must fail")
	}
	if _, err := For(c, "ghost", connect.NewClient[testpb.CreateGadgetRequest, testpb.CreateGadgetResponse]); err == nil {
		t.Fatalf("For must fail")
	}
}

// --- chain behavior against a live upstream ---

func TestRetryRecoversAndKeyIsStable(t *testing.T) {
	up := newUpstream(2, connect.CodeUnavailable)
	url, srv := up.handler("X-Idempotency-Key")
	defer srv.Close()

	c := clientFor(t, ServiceConfig{
		BaseURL:     url,
		Retry:       &RetryConfig{MaxAttempts: 3, Backoff: "constant", InitialDelayMS: 1, MaxDelayMS: 2},
		Idempotency: &IdempotencyConfig{Enabled: true},
	}, "svc")

	res, err := call(t, c, "svc", context.Background())
	if err != nil || res.Msg.GetId() != "ok" {
		t.Fatalf("retry must recover: res=%v err=%v", res, err)
	}
	if got := up.calls.Load(); got != 3 {
		t.Fatalf("want 3 attempts, got %d", got)
	}
	first := <-up.lastKeys
	if first == "" {
		t.Fatalf("idempotency key missing")
	}
	for i := 0; i < 2; i++ {
		if k := <-up.lastKeys; k != first {
			t.Fatalf("key changed across attempts: %q != %q", k, first)
		}
	}
}

func TestRetryDoesNotFireOnUnlistedCode(t *testing.T) {
	up := newUpstream(1, connect.CodeInvalidArgument)
	url, srv := up.handler("")
	defer srv.Close()

	c := clientFor(t, ServiceConfig{
		BaseURL: url,
		Retry:   &RetryConfig{MaxAttempts: 3, Backoff: "constant", InitialDelayMS: 1},
	}, "svc")

	_, err := call(t, c, "svc", context.Background())
	var cerr *connect.Error
	if !errors.As(err, &cerr) || cerr.Code() != connect.CodeInvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
	if got := up.calls.Load(); got != 1 {
		t.Fatalf("unlisted code must not retry: %d attempts", got)
	}
}

func TestBreakerOpensAndRejectsWithoutDialing(t *testing.T) {
	up := newUpstream(1000, connect.CodeInternal)
	url, srv := up.handler("")
	defer srv.Close()

	c := clientFor(t, ServiceConfig{
		BaseURL:        url,
		CircuitBreaker: &BreakerConfig{Enabled: true, FailureThreshold: 2, SuccessThreshold: 1, OpenForMS: 60_000},
	}, "svc")

	for i := 0; i < 2; i++ {
		if _, err := call(t, c, "svc", context.Background()); err == nil {
			t.Fatalf("expected failure")
		}
	}
	dialsBefore := up.calls.Load()
	_, err := call(t, c, "svc", context.Background())
	var cerr *connect.Error
	if !errors.As(err, &cerr) || cerr.Code() != connect.CodeUnavailable || !strings.Contains(cerr.Message(), "circuit breaker open") {
		t.Fatalf("want breaker rejection, got %v", err)
	}
	if up.calls.Load() != dialsBefore {
		t.Fatalf("open breaker must not dial")
	}
}

func TestAuthStaticAndCorrelation(t *testing.T) {
	up := newUpstream(0, connect.CodeUnknown)
	url, srv := up.handler("")
	defer srv.Close()

	c := clientFor(t, ServiceConfig{
		BaseURL: url,
		Auth:    &AuthConfig{Mode: "static", Token: "svc-token"},
	}, "svc")

	appCtx := configuration.NewAppContextWithRandomID(configuration.LangENG)
	appCtx.SetParent(context.Background())
	if _, err := call(t, c, "svc", appCtx); err != nil {
		t.Fatalf("call: %v", err)
	}
	if got := up.lastAuth.Load().(string); got != "Bearer svc-token" {
		t.Fatalf("static auth header: %q", got)
	}
	if got := up.lastTID.Load().(string); got != appCtx.ID().String() {
		t.Fatalf("threadID not propagated: %q", got)
	}
}

func TestAuthForward(t *testing.T) {
	up := newUpstream(0, connect.CodeUnknown)
	url, srv := up.handler("")
	defer srv.Close()

	c := clientFor(t, ServiceConfig{BaseURL: url, Auth: &AuthConfig{Mode: "forward"}}, "svc")

	// no bearer on a plain ctx → loud Unauthenticated, no dial
	before := up.calls.Load()
	_, err := call(t, c, "svc", context.Background())
	var cerr *connect.Error
	if !errors.As(err, &cerr) || cerr.Code() != connect.CodeUnauthenticated {
		t.Fatalf("want Unauthenticated, got %v", err)
	}
	if up.calls.Load() != before {
		t.Fatalf("missing bearer must not dial")
	}

	// bearer on the AppContext → forwarded verbatim
	appCtx := configuration.NewAppContextWithRandomID(configuration.LangENG)
	appCtx.SetParent(context.Background())
	appCtx.SetBearerToken("caller-token")
	if _, err := call(t, c, "svc", appCtx); err != nil {
		t.Fatalf("forward call: %v", err)
	}
	if got := up.lastAuth.Load().(string); got != "Bearer caller-token" {
		t.Fatalf("forwarded bearer: %q", got)
	}
}

func TestDefaultDeadlineApplied(t *testing.T) {
	sawDeadline := make(chan bool, 1)
	fn := func(ctx context.Context, req *connect.Request[testpb.CreateGadgetRequest]) (*connect.Response[testpb.CreateGadgetResponse], error) {
		_, has := ctx.Deadline()
		sawDeadline <- has
		return connect.NewResponse(&testpb.CreateGadgetResponse{Id: "ok"}), nil
	}
	srv := httptest.NewServer(connect.NewUnaryHandler(testProcedure, fn))
	defer srv.Close()

	c := clientFor(t, ServiceConfig{BaseURL: srv.URL, TimeoutSeconds: 5}, "svc")
	if _, err := call(t, c, "svc", context.Background()); err != nil {
		t.Fatalf("call: %v", err)
	}
	if !<-sawDeadline {
		t.Fatalf("default timeout must reach the server as a protocol deadline")
	}
}

func TestForConstructsWorkingClient(t *testing.T) {
	up := newUpstream(0, connect.CodeUnknown)
	url, srv := up.handler("")
	defer srv.Close()

	c := clientFor(t, ServiceConfig{BaseURL: url}, "svc")
	cc, err := For(c, "svc", func(hc connect.HTTPClient, base string, opts ...connect.ClientOption) *connect.Client[testpb.CreateGadgetRequest, testpb.CreateGadgetResponse] {
		return connect.NewClient[testpb.CreateGadgetRequest, testpb.CreateGadgetResponse](hc, base+testProcedure, opts...)
	})
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	res, err := cc.CallUnary(context.Background(), connect.NewRequest(&testpb.CreateGadgetRequest{Name: proto.String("x")}))
	if err != nil || res.Msg.GetId() != "ok" {
		t.Fatalf("For-built client: res=%v err=%v", res, err)
	}
}

func TestTracingPathRuns(t *testing.T) {
	up := newUpstream(0, connect.CodeUnknown)
	url, srv := up.handler("")
	defer srv.Close()
	c, err := New(&Config{Services: map[string]ServiceConfig{"svc": {BaseURL: url}}}, WithClientTracing(true))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := call(t, c, "svc", context.Background()); err != nil {
		t.Fatalf("tracing-enabled call: %v", err)
	}
	// error path records the span outcome
	upErr := newUpstream(1000, connect.CodeNotFound)
	urlErr, srvErr := upErr.handler("")
	defer srvErr.Close()
	c2, _ := New(&Config{Services: map[string]ServiceConfig{"svc": {BaseURL: urlErr}}}, WithClientTracing(true))
	if _, err := call(t, c2, "svc", context.Background()); err == nil {
		t.Fatalf("expected error")
	}
}

func TestWithTransportAndRequestID(t *testing.T) {
	up := newUpstream(0, connect.CodeUnknown)
	url, srv := up.handler("")
	defer srv.Close()

	// WithTransport: route through the default transport explicitly
	c, err := New(&Config{Services: map[string]ServiceConfig{"svc": {BaseURL: url}}},
		WithTransport(srv.Client().Transport))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	appCtx := configuration.NewAppContextWithRandomID(configuration.LangENG)
	appCtx.SetParent(context.Background())
	if _, err := call(t, c, "svc", appCtx); err != nil {
		t.Fatalf("call: %v", err)
	}
	if got := up.lastTID.Load().(string); got != appCtx.ID().String() {
		t.Fatalf("threadID: %q", got)
	}
}

func TestRetryAbortsWhenCtxCancelledDuringBackoff(t *testing.T) {
	up := newUpstream(1000, connect.CodeUnavailable)
	url, srv := up.handler("")
	defer srv.Close()

	c := clientFor(t, ServiceConfig{
		BaseURL: url,
		Retry:   &RetryConfig{MaxAttempts: 5, Backoff: "constant", InitialDelayMS: 200, MaxDelayMS: 200},
	}, "svc")

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := call(t, c, "svc", ctx)
	if err == nil {
		t.Fatalf("expected failure")
	}
	if time.Since(start) > time.Second {
		t.Fatalf("ctx cancel during backoff must abort promptly")
	}
	if got := up.calls.Load(); got >= 5 {
		t.Fatalf("cancel must cut the attempt budget short: %d", got)
	}
}

func TestPoolConfigAppliedToTransport(t *testing.T) {
	c := clientFor(t, ServiceConfig{
		BaseURL: "http://x",
		Pool:    &PoolConfig{MaxIdleConnsPerHost: 32},
	}, "svc")
	transport, ok := c.services["svc"].http.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("pool must install a tuned *http.Transport")
	}
	if transport.MaxIdleConnsPerHost != 32 {
		t.Fatalf("MaxIdleConnsPerHost: %d", transport.MaxIdleConnsPerHost)
	}
	if transport.MaxIdleConns < 32 {
		t.Fatalf("MaxIdleConns must accommodate the per-host pool: %d", transport.MaxIdleConns)
	}
}

func TestPoolConfigNegativeRejected(t *testing.T) {
	_, err := New(&Config{Services: map[string]ServiceConfig{"svc": {
		BaseURL: "http://x",
		Pool:    &PoolConfig{MaxIdleConnsPerHost: -1},
	}}})
	if err == nil || !strings.Contains(err.Error(), "pool values") {
		t.Fatalf("negative pool must fail: %v", err)
	}
}

func TestPoolLifetimeSweepRecyclesIdleConnections(t *testing.T) {
	var newConns atomic.Int64
	fn := func(context.Context, *connect.Request[testpb.CreateGadgetRequest]) (*connect.Response[testpb.CreateGadgetResponse], error) {
		return connect.NewResponse(&testpb.CreateGadgetResponse{Id: "ok"}), nil
	}
	srv := httptest.NewUnstartedServer(connect.NewUnaryHandler(testProcedure, fn))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			newConns.Add(1)
		}
	}
	srv.Start()
	defer srv.Close()

	c := clientFor(t, ServiceConfig{
		BaseURL: srv.URL,
		Pool:    &PoolConfig{ConnMaxLifetimeSeconds: 1},
	}, "svc")

	if _, err := call(t, c, "svc", context.Background()); err != nil {
		t.Fatalf("first call: %v", err)
	}
	time.Sleep(1200 * time.Millisecond) // sweep closes the idle pipe
	if _, err := call(t, c, "svc", context.Background()); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if got := newConns.Load(); got < 2 {
		t.Fatalf("lifetime sweep must force a re-dial: %d connections", got)
	}
}

func TestShouldRetryClassification(t *testing.T) {
	c := clientFor(t, ServiceConfig{
		BaseURL: "http://x",
		Retry:   &RetryConfig{MaxAttempts: 3, RetryOn: []string{"unavailable"}},
	}, "svc")
	s := c.services["svc"]
	if !s.shouldRetry(&net.DNSError{IsTimeout: true}) {
		t.Fatalf("transport net.Error must always retry")
	}
	if !s.shouldRetry(connect.NewError(connect.CodeUnavailable, errors.New("x"))) {
		t.Fatalf("listed code must retry")
	}
	if s.shouldRetry(connect.NewError(connect.CodeNotFound, errors.New("x"))) {
		t.Fatalf("unlisted code must not retry")
	}
}

func TestBreakerRecordsSuccess(t *testing.T) {
	up := newUpstream(0, connect.CodeUnknown)
	url, srv := up.handler("")
	defer srv.Close()
	c := clientFor(t, ServiceConfig{
		BaseURL:        url,
		CircuitBreaker: &BreakerConfig{Enabled: true, FailureThreshold: 2, SuccessThreshold: 1, OpenForMS: 100},
	}, "svc")
	for i := 0; i < 3; i++ {
		if _, err := call(t, c, "svc", context.Background()); err != nil {
			t.Fatalf("healthy upstream through enabled breaker: %v", err)
		}
	}
}

func TestPoolLifetimeOnlyWithoutMaxIdle(t *testing.T) {
	c := clientFor(t, ServiceConfig{
		BaseURL: "http://x",
		Pool:    &PoolConfig{ConnMaxLifetimeSeconds: 3600},
	}, "svc")
	if _, ok := c.services["svc"].http.Transport.(*http.Transport); !ok {
		t.Fatalf("pool with lifetime only must still install a cloned transport")
	}
}

func TestResolveRetryNilAndSingleAttempt(t *testing.T) {
	if p, err := resolveRetry("svc", nil); err != nil || !p.disabled() {
		t.Fatalf("nil retry: %+v %v", p, err)
	}
	if p, err := resolveRetry("svc", &RetryConfig{MaxAttempts: 1}); err != nil || !p.disabled() {
		t.Fatalf("single attempt: %+v %v", p, err)
	}
}

func TestPoolMaxIdleAboveTransportDefaultRaisesCeiling(t *testing.T) {
	c := clientFor(t, ServiceConfig{
		BaseURL: "http://x",
		Pool:    &PoolConfig{MaxIdleConnsPerHost: 256}, // above the transport's default MaxIdleConns (100)
	}, "svc")
	transport := c.services["svc"].http.Transport.(*http.Transport)
	if transport.MaxIdleConns != 256 {
		t.Fatalf("MaxIdleConns ceiling must rise with the per-host pool: %d", transport.MaxIdleConns)
	}
}
