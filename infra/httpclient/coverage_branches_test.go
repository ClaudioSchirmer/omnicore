package httpclient

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/ClaudioSchirmer/omnicore/infra/cache"
)

// ─── duration.go: UnmarshalYAML decode error ────────────────────────────────

func TestDuration_UnmarshalYAML_DecodeError(t *testing.T) {
	// A YAML mapping cannot decode into a string scalar → node.Decode errors.
	var d Duration
	err := yaml.Unmarshal([]byte("{a: 1, b: 2}"), &d)
	if err == nil {
		t.Fatal("expected decode error for a non-scalar yaml node")
	}
}

func TestDuration_UnmarshalYAML_BadDuration(t *testing.T) {
	var d Duration
	if err := yaml.Unmarshal([]byte(`"not-a-duration"`), &d); err == nil {
		t.Fatal("expected parse error for an invalid duration string")
	}
}

func TestDuration_UnmarshalYAML_Empty(t *testing.T) {
	var d Duration
	if err := yaml.Unmarshal([]byte(`""`), &d); err != nil {
		t.Fatalf("empty string should decode to 0, got %v", err)
	}
	if d.ToTime() != 0 {
		t.Errorf("empty duration = %v, want 0", d.ToTime())
	}
}

// ─── error.go: causeIsRetriable url.Error unwrap + net.OpError ───────────────

func TestCauseIsRetriable_URLErrorWrappingOpError(t *testing.T) {
	// A non-timeout *url.Error → unwrap → underlying *net.OpError is retriable.
	inner := &net.OpError{Op: "dial", Err: errors.New("connection refused")}
	ue := &url.Error{Op: "Get", URL: "http://x", Err: inner}
	if !causeIsRetriable(ue) {
		t.Fatal("url.Error wrapping a net.OpError must be retriable")
	}
}

func TestCauseIsRetriable_NetOpError(t *testing.T) {
	if !causeIsRetriable(&net.OpError{Op: "read", Err: errors.New("reset")}) {
		t.Fatal("a bare net.OpError must be retriable")
	}
}

// ─── observation.go: emit with nil logger falls back to default ──────────────

func TestObservation_Emit_NilLoggerUsesDefault(t *testing.T) {
	o := newObservation("svc", "ep", "GET", false)
	// Must not panic; the nil logger branch substitutes slog.Default().
	o.emit(context.Background(), nil)
}

// ─── redaction.go: redactPath through a non-object intermediate ──────────────

func TestRedactPath_IntermediateNotAnObject(t *testing.T) {
	// Three-level path where the middle node ("a") is a scalar — the walk
	// hits the non-object guard inside the intermediate loop.
	root := map[string]any{"a": "scalar"}
	if redactPath(root, "$.a.b.c") {
		t.Fatal("redactPath must return false when an intermediate node is not an object")
	}
}

func TestRedactPath_LeafNotAnObject(t *testing.T) {
	root := map[string]any{"a": "scalar"} // leaf parent is a scalar
	if redactPath(root, "$.a.b") {
		t.Fatal("redactPath must return false when the leaf parent is not an object")
	}
}

func TestRedactPath_LeafKeyMissing(t *testing.T) {
	root := map[string]any{"a": map[string]any{"x": 1}}
	if redactPath(root, "$.a.missing") {
		t.Fatal("redactPath must return false when the leaf key is absent")
	}
}

// ─── idempotency.go: disabled policy delegates ──────────────────────────────

func TestIdempotencyMiddleware_DisabledDelegates(t *testing.T) {
	reached := false
	mw := idempotencyMiddleware(idempotencyPolicy{enabled: false})
	req, _ := http.NewRequest("POST", "http://x/p", nil)
	obs := &observation{}
	_, err := mw.RoundTrip(context.Background(), req, obs, terminal(&http.Response{StatusCode: 200, Body: http.NoBody}, nil, &reached))
	if err != nil {
		t.Fatalf("disabled idempotency should delegate cleanly, got %v", err)
	}
	if !reached {
		t.Fatal("disabled idempotency must call next")
	}
}

// ─── options.go: WithConfig request/response codec overrides ─────────────────

func TestWithConfig_CodecOverrides(t *testing.T) {
	cfg := applyInvokeOptions([]InvokeOption{
		WithConfig(CallConfig{RequestCodec: "xml", ResponseCodec: "form"}),
	})
	if cfg.requestCodecOverride != "xml" {
		t.Errorf("requestCodecOverride = %q, want xml", cfg.requestCodecOverride)
	}
	if cfg.responseCodecOverride != "form" {
		t.Errorf("responseCodecOverride = %q, want form", cfg.responseCodecOverride)
	}
}

// ─── service_client.go: timeout cascade fallback + TLS asset error ───────────

func TestBuildServiceClient_TimeoutFallsBackToDefault(t *testing.T) {
	svc, err := buildServiceClient("s", ServiceConfig{
		Endpoints: map[string]EndpointConfig{"e": {Method: "GET", Path: "/x"}},
	}, Defaults{})
	if err != nil {
		t.Fatalf("buildServiceClient: %v", err)
	}
	if svc.httpClient.Timeout != defaultTimeout {
		t.Errorf("timeout = %v, want default %v", svc.httpClient.Timeout, defaultTimeout)
	}
}

func TestBuildServiceClient_TLSAssetError(t *testing.T) {
	_, err := buildServiceClient("s", ServiceConfig{
		Endpoints: map[string]EndpointConfig{"e": {Method: "GET", Path: "/x"}},
		TLS:       &TLSConfig{ClientCertFile: "/no/such/cert.pem", ClientKeyFile: "/no/such/key.pem"},
	}, Defaults{})
	if err == nil {
		t.Fatal("expected TLS asset load error for missing cert files")
	}
}

func TestAddRedactedHeader_InitializesSetAndSkipsEmpty(t *testing.T) {
	policy := &redactionPolicy{} // nil headerSet
	addRedactedHeader(policy, "")  // no-op for empty name
	if policy.headerSet != nil {
		t.Fatal("empty name must not initialize the header set")
	}
	addRedactedHeader(policy, "x-secret")
	if _, ok := policy.headerSet["X-Secret"]; !ok {
		t.Errorf("header not registered in canonical form: %v", policy.headerSet)
	}
	addRedactedHeader(nil, "x") // nil policy is a no-op (no panic)
}

// ─── breaker.go / breaker_middleware.go ─────────────────────────────────────

func TestBreaker_RecordSuccess_DisabledAndNil(t *testing.T) {
	var nilB *breakerState
	nilB.recordSuccess() // nil receiver — must not panic
	disabled := &breakerState{policy: breakerPolicy{enabled: false}}
	disabled.recordSuccess() // disabled guard — must not panic
}

func TestBreakerMiddleware_TransportErrorRecordsFailure(t *testing.T) {
	state := newBreakerState(breakerPolicy{enabled: true, failureThreshold: 5, openFor: time.Minute})
	mw := breakerMiddleware(state)
	req, _ := http.NewRequest("GET", "http://x/p", nil)
	obs := &observation{}
	wantErr := errors.New("dial failed")
	_, err := mw.RoundTrip(context.Background(), req, obs, terminal(nil, wantErr, nil))
	if !errors.Is(err, wantErr) {
		t.Fatalf("breaker should surface the transport error, got %v", err)
	}
}

// ─── call.go helpers ────────────────────────────────────────────────────────

func TestResolveBaseURL_NoResolverEmptyYAML(t *testing.T) {
	c := &HttpClient{}
	if _, err := c.resolveBaseURL(context.Background(), "svc", "", ""); err == nil {
		t.Fatal("expected error: no baseURL, no resolver, no override")
	}
}

func TestCaptureRequestObservation_BodyReadError(t *testing.T) {
	req, _ := http.NewRequest("POST", "http://x/p", nil)
	req.Body = errReadCloser{}
	req.ContentLength = 8
	obs := &observation{}
	captureRequestObservation(obs, req)
	if obs.Err == nil {
		t.Fatal("a body read error must be recorded on the observation")
	}
}

func TestCaptureRequestObservation_RequestBytesFromBodyLen(t *testing.T) {
	req, _ := http.NewRequest("POST", "http://x/p", io.NopCloser(stringReader("hello")))
	req.ContentLength = 0 // force the RequestBytes==0 fill-in branch
	obs := &observation{}
	captureRequestObservation(obs, req)
	if obs.RequestBytes != 5 {
		t.Errorf("RequestBytes = %d, want 5 (filled from body length)", obs.RequestBytes)
	}
	if string(obs.RequestBody) != "hello" {
		t.Errorf("RequestBody = %q", obs.RequestBody)
	}
}

func TestBuildHttpError_AttemptCoercedToOne(t *testing.T) {
	req, _ := http.NewRequest("GET", "http://x/p", nil)
	he := buildHttpError("s", "e", req, nil, nil, 500, errors.New("boom"), time.Second, 0)
	if he.Attempt != 1 {
		t.Errorf("attempt = %d, want coerced to 1", he.Attempt)
	}
	if he.Method != "GET" {
		t.Errorf("method = %q, want GET", he.Method)
	}
}

// ─── client.go: New error propagation + service lookup ───────────────────────

func TestNew_BuildServiceClientError(t *testing.T) {
	// Passes Validate (both cert files set together) but fails the asset load
	// inside buildServiceClient, so the error surfaces from New.
	cfg := &Config{
		Services: map[string]ServiceConfig{
			"svc": {
				BaseURL:   "https://x.example.com",
				Endpoints: map[string]EndpointConfig{"e": {Method: "GET", Path: "/x"}},
				TLS:       &TLSConfig{ClientCertFile: "/no/such/cert.pem", ClientKeyFile: "/no/such/key.pem"},
			},
		},
	}
	if _, err := New(cfg); err == nil {
		t.Fatal("expected New to surface the buildServiceClient TLS error")
	}
}

func TestBuildAuthRegistry_ProviderError(t *testing.T) {
	_, _, err := buildAuthRegistry(map[string]AuthProviderConfig{
		"bad": {Type: "oauth2-client-credentials"}, // missing tokenEndpoint/clientId
	})
	if err == nil {
		t.Fatal("expected buildAuthRegistry to surface the provider construction error")
	}
}

func TestService_UnknownNameOnPopulatedClient(t *testing.T) {
	cfg := &Config{Services: map[string]ServiceConfig{
		"known": {BaseURL: "https://x", Endpoints: map[string]EndpointConfig{"e": {Method: "GET", Path: "/x"}}},
	}}
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.service("nope"); err == nil {
		t.Fatal("expected unknown-service error on a populated client")
	}
}

func TestBuildInlineAuthProvider_NoCredential(t *testing.T) {
	if _, err := buildInlineAuthProvider(&InlineAuth{}); err == nil {
		t.Fatal("expected error when no inline credential is set")
	}
}

// ─── config validation: valid enum arms ─────────────────────────────────────

func TestValidateSigningConfig_ValidTimestampFormatAccepted(t *testing.T) {
	errs := validateSigningConfig("svc.signing", &SigningConfig{
		Type:            "hmac-sha256",
		Secret:          "s3cret",
		SignedHeaders:   []string{"X-Date"},
		TimestampHeader: "X-Date",
		TimestampFormat: "iso8601", // valid → no timestampFormat error
		SignatureHeader: "X-Signature",
	})
	for _, e := range errs {
		if strings.Contains(e, "timestampFormat") {
			t.Fatalf("a valid timestampFormat must not error, got: %v", errs)
		}
	}
}

func TestValidateAuthProviders_ValidCredentialsExchangeCodec(t *testing.T) {
	errs := validateAuthProviders(map[string]AuthProviderConfig{
		"kc": {
			Type:              "credentials-exchange",
			TokenEndpoint:     "https://idp/token",
			RequestFields:     map[string]string{"grant_type": "password"},
			ResponseTokenPath: "$.access_token",
			RequestCodec:      "json", // valid → no requestCodec error
		},
	})
	for _, e := range errs {
		if strings.Contains(e, "requestCodec") {
			t.Fatalf("a valid requestCodec must not error, got: %v", errs)
		}
	}
}

// ─── tls_config.go ──────────────────────────────────────────────────────────

func TestResolveTLSConfig_BadMinVersion(t *testing.T) {
	if _, err := resolveTLSConfig(nil, &TLSConfig{MinVersion: "9.9"}); err == nil {
		t.Fatal("expected error for unsupported minVersion")
	}
}

func TestMergeTLSConfig_CopiesDefaultCipherSuites(t *testing.T) {
	defaults := &TLSConfig{CipherSuites: []string{"a", "b"}}
	out := mergeTLSConfig(defaults, nil)
	if out == nil || len(out.CipherSuites) != 2 {
		t.Fatalf("merged cipher suites = %+v", out)
	}
	// The copy must be independent of the source slice.
	out.CipherSuites[0] = "mutated"
	if defaults.CipherSuites[0] != "a" {
		t.Fatal("mergeTLSConfig must deep-copy the cipher suites slice")
	}
}

func TestResolvePoolConfig_IdleTimeoutAndKeepAlives(t *testing.T) {
	disable := true
	_, _, idle, disableKA := resolvePoolConfig(nil, &PoolConfig{
		IdleConnTimeout:   Duration(45 * time.Second),
		DisableKeepAlives: &disable,
	})
	if idle != 45*time.Second {
		t.Errorf("idleTimeout = %v, want 45s", idle)
	}
	if !disableKA {
		t.Error("DisableKeepAlives should be true")
	}
}

func TestValidateTLSConfig_BadCipherSuites(t *testing.T) {
	errs := validateTLSConfig("svc.tls", &TLSConfig{CipherSuites: []string{"NOPE_CIPHER"}})
	if len(errs) == 0 {
		t.Fatal("expected a validation error for an unknown cipher suite")
	}
}

// ─── cache_middleware.go: bypass, next error, read error, sortedQuery nil ────

func TestSortedQuery_NilURL(t *testing.T) {
	if got := sortedQuery(nil); got != "" {
		t.Errorf("sortedQuery(nil) = %q, want empty", got)
	}
}

func cacheMW(store cache.Cache, policy cachePolicy) roundTripper {
	return cacheMiddleware("s", "e", func() cache.Cache { return store }, policy, false, nil, nil)
}

func TestCacheMiddleware_BypassWhenStoreNil(t *testing.T) {
	reached := false
	mw := cacheMW(nil, cachePolicy{enabled: true})
	req, _ := http.NewRequest("GET", "http://x/p", nil)
	obs := &observation{}
	_, err := mw.RoundTrip(context.Background(), req, obs, terminal(&http.Response{StatusCode: 200, Body: http.NoBody}, nil, &reached))
	if err != nil {
		t.Fatalf("bypass should delegate cleanly, got %v", err)
	}
	if obs.CacheStatus != "bypass" || !reached {
		t.Fatalf("expected bypass+delegate, got status=%q reached=%v", obs.CacheStatus, reached)
	}
}

func TestCacheMiddleware_NextErrorPropagates(t *testing.T) {
	mw := cacheMW(&scriptedCache{}, cachePolicy{enabled: true, ttl: time.Minute})
	req, _ := http.NewRequest("GET", "http://x/p", nil)
	obs := &observation{}
	wantErr := errors.New("upstream down")
	_, err := mw.RoundTrip(context.Background(), req, obs, terminal(nil, wantErr, nil))
	if !errors.Is(err, wantErr) {
		t.Fatalf("cache miss + next error must propagate, got %v", err)
	}
}

func TestCacheMiddleware_BodyReadErrorOnStore(t *testing.T) {
	mw := cacheMW(&scriptedCache{}, cachePolicy{enabled: true, ttl: time.Minute})
	req, _ := http.NewRequest("GET", "http://x/p", nil)
	obs := &observation{}
	resp := &http.Response{StatusCode: 200, Header: http.Header{}, Body: errReadCloser{}, ContentLength: 4}
	_, err := mw.RoundTrip(context.Background(), req, obs, terminal(resp, nil, nil))
	if err == nil {
		t.Fatal("a body read error while storing must surface")
	}
}

// ─── middleware.go: loggingMiddleware response read error ────────────────────

func TestLoggingMiddleware_ResponseBodyReadError(t *testing.T) {
	mw := loggingMiddleware(nil)
	req, _ := http.NewRequest("GET", "http://x/p", nil)
	obs := newObservation("s", "e", "GET", false)
	resp := &http.Response{StatusCode: 200, Header: http.Header{}, Body: errReadCloser{}, ContentLength: 4}
	_, err := mw.RoundTrip(context.Background(), req, obs, terminal(resp, nil, nil))
	if err == nil {
		t.Fatal("a response body read error must surface from the logging middleware")
	}
	if obs.Err == nil {
		t.Error("the read error should be recorded on the observation")
	}
}

// ─── auth_middleware.go: nil provider + revocation re-apply failure ─────────

func TestAuthMiddleware_NilProviderDelegates(t *testing.T) {
	reached := false
	mw := authMiddleware(nil, false)
	req, _ := http.NewRequest("GET", "http://x/p", nil)
	obs := &observation{}
	_, err := mw.RoundTrip(context.Background(), req, obs, terminal(&http.Response{StatusCode: 200, Body: http.NoBody}, nil, &reached))
	if err != nil || !reached {
		t.Fatalf("nil provider must delegate; err=%v reached=%v", err, reached)
	}
}

// flakyRevocable applies successfully the first time and errors on every
// re-apply, driving the revocation-retry failure branch.
type flakyRevocable struct{ applies int }

func (f *flakyRevocable) Name() string { return "flaky" }
func (f *flakyRevocable) Apply(*http.Request) error {
	f.applies++
	if f.applies > 1 {
		return errors.New("token re-acquire failed")
	}
	return nil
}
func (f *flakyRevocable) Invalidate() {}

func TestAuthMiddleware_RevocationReapplyError(t *testing.T) {
	prov := &flakyRevocable{}
	mw := authMiddleware(prov, true)
	req, _ := http.NewRequest("GET", "http://x/p", nil)
	obs := &observation{}
	resp401 := &http.Response{StatusCode: http.StatusUnauthorized, Body: http.NoBody}
	_, err := mw.RoundTrip(context.Background(), req, obs, terminal(resp401, nil, nil))
	var he *HttpError
	if !errors.As(err, &he) || !errors.Is(he.Cause, ErrTokenAcquire) {
		t.Fatalf("expected ErrTokenAcquire on re-apply failure, got %v", err)
	}
	if prov.applies != 2 {
		t.Errorf("expected exactly two Apply attempts, got %d", prov.applies)
	}
}

// ─── sse.go: handleSSELine no-colon field + startSSEPump default event ───────

func TestHandleSSELine_NoColonTreatedAsField(t *testing.T) {
	st := &sseParserState{}
	handleSSELine("data", st) // no colon → whole line is the field name "data"
	st2 := &sseParserState{}
	handleSSELine("unknownfield", st2) // no colon, unrecognized field → ignored
	if len(st.dataLines) != 1 || st.dataLines[0] != "" {
		t.Errorf("bare 'data' line should append an empty value, got %+v", st.dataLines)
	}
}

func TestStartSSEPump_DefaultEventTypeIsMessage(t *testing.T) {
	body := io.NopCloser(stringReader("data: hello\n\n"))
	sse := startSSEPump(context.Background(), body)
	select {
	case ev, ok := <-sse.Events:
		if !ok {
			t.Fatal("events channel closed before delivering an event")
		}
		if ev.Event != "message" {
			t.Errorf("default event type = %q, want message", ev.Event)
		}
		if string(ev.Data) != "hello" {
			t.Errorf("data = %q, want hello", ev.Data)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for SSE event")
	}
	_ = sse.Close()
}

func TestStartSSEPump_ContextCancelClosesEvents(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	ctx, cancel := context.WithCancel(context.Background())
	sse := startSSEPump(ctx, pr)
	cancel() // triggers the ctx.Done watcher → closeFn → reader unblocks
	select {
	case _, ok := <-sse.Events:
		// Either an event then close, or an immediate close — both fine; the
		// point is the goroutine drains and closes the channel.
		_ = ok
	case <-time.After(2 * time.Second):
		t.Fatal("context cancel did not shut down the SSE pump")
	}
}

// ─── call.go: per-call client certificate clone path ────────────────────────

func TestCall_WithClientCert_ClonesService(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	_, _, cert := genCertPairFiles(t)
	skip := true
	cfg := &Config{
		Services: map[string]ServiceConfig{
			"svc": {
				BaseURL:   srv.URL,
				TLS:       &TLSConfig{InsecureSkipVerify: &skip},
				Endpoints: map[string]EndpointConfig{"call": {Method: "GET", Path: "/x"}},
			},
		},
	}
	c, err := New(cfg, WithLogger(discardLogger()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	type req struct{}
	type resp struct {
		OK bool `json:"ok"`
	}
	out, err := Call[req, resp](newCtx(t), c, "svc", "call", req{}, WithClientCert(cert))
	if err != nil {
		t.Fatalf("Call with client cert: %v", err)
	}
	if !out.OK {
		t.Errorf("response = %+v, want OK true", out)
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// stringReader is a tiny helper since strings.NewReader returns *strings.Reader
// (no Close); wrap it where an io.Reader suffices.
func stringReader(s string) io.Reader { return &byteSliceReader{data: []byte(s)} }

type byteSliceReader struct {
	data []byte
	pos  int
}

func (r *byteSliceReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}
