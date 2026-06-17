package httpclient

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/httpclient/binding"
)

// Call is the typed entry point for outbound HTTP. There is no parallel
// untyped public path — every cross-service call goes through this function.
//
//	ctx:      AppContext (or any context.Context — AppContext implements it)
//	c:        the HttpClient registry built at bootstrap
//	service:  YAML key under httpClient.services
//	endpoint: YAML key under that service's endpoints
//	req:      typed struct with `http:"..."` tags (path/query/header/headers/body)
//	Resp:     typed struct decoded via the endpoint's responseCodec
//
// Returns the decoded response and a nil error on 2xx. Non-2xx and
// acceptable-status matches both return *HttpError; the consumer branches on
// IsAcceptableStatus / IsRetriable. The response struct is still populated
// when the body decodes successfully, even on acceptable-status errors.
//
// The function emits exactly one slog observation record per call via the
// logging middleware. Header names in the framework's default block list are
// redacted before logging.
func Call[Req any, Resp any](
	ctx context.Context,
	c *HttpClient,
	service, endpoint string,
	req Req,
	opts ...InvokeOption,
) (Resp, error) {
	var zero Resp
	if c == nil {
		return zero, fmt.Errorf("httpclient: nil client")
	}
	if c.fake != nil {
		return fakeCall[Req, Resp](ctx, c, service, endpoint, req, opts)
	}
	svc, err := c.service(service)
	if err != nil {
		return zero, err
	}
	ep, ok := svc.endpoints[endpoint]
	if !ok {
		return zero, fmt.Errorf("httpclient: service %q has no endpoint %q", service, endpoint)
	}
	cfg := applyInvokeOptions(opts)

	meta := binding.EndpointMeta{
		Method:           cfg.effectiveMethod(ep.method),
		Path:             cfg.effectivePath(ep.path),
		RequestCodec:     cfg.effectiveRequestCodec(ep.requestCodec),
		ResponseCodec:    cfg.effectiveResponseCodec(ep.responseCodec),
		Headers:          ep.headers,
		AcceptableStatus: ep.acceptableStatus,
	}

	callCtx, cancel := applyTimeout(ctx, cfg.timeout)
	defer cancel()

	baseURL, err := c.resolveBaseURL(callCtx, service, svc.baseURL, cfg.baseURLOverride)
	if err != nil {
		obs := newObservation(service, endpoint, ep.method, svc.logBodies)
		obs.Err = err
		obs.URL = bestEffortURL(svc, cfg, meta)
		obs.ThreadID = correlationID(ctx)
		obs.emit(callCtx, c.logger)
		return zero, &HttpError{Service: service, Endpoint: endpoint, Method: ep.method, URL: obs.URL, Cause: err, Attempt: 1}
	}

	// Per-call client-cert override builds an ephemeral *http.Client with
	// a cloned transport that swaps the TLS certificate. The registry's
	// per-service transport stays intact so the pool isn't polluted.
	effectiveSvc := svc
	if cfg.clientCert != nil {
		clone, err := cloneServiceWithClientCert(svc, *cfg.clientCert)
		if err != nil {
			return zero, &HttpError{Method: ep.method, URL: baseURL + ep.path, Cause: err}
		}
		effectiveSvc = clone
	}
	_ = effectiveSvc // used below

	obs := newObservation(service, endpoint, ep.method, svc.logBodies)
	obs.noCache = cfg.noCache
	obs.cacheKey = cfg.cacheKey
	obs.idempotencyKey = cfg.idempotencyKey
	obs.streamingResponse = ep.responseStream || ep.responseSSE
	// Use meta.Path (the effective path after CallConfig overrides) rather
	// than ep.path (the YAML path) so the binding cache stores a plan whose
	// path-coverage check matches the path BuildRequest will dispatch
	// against. Passing the YAML path here while BuildRequest later uses the
	// override path would cache a stale validation error for any subsequent
	// inspection of the same type.
	obs.streamingRequest = binding.HasStreamingBody(req, meta.Path)

	// Streaming uploads are incompatible with HMAC signing: the policy
	// needs SHA256(body) for the canonical string, and a streaming body
	// cannot be hashed without buffering. Reject at call time with a
	// clear sentinel so the operator sees the conflict.
	if obs.streamingRequest && !svc.signing.disabled() {
		return zero, &HttpError{
			Service:  service,
			Endpoint: endpoint,
			Method:   ep.method,
			Cause:    fmt.Errorf("httpclient: service %q signs requests but endpoint %q has a streaming body (body,stream/body,multipart) — signing requires the body bytes", service, endpoint),
			Attempt:  1,
		}
	}

	httpReq, err := binding.BuildRequest(callCtx, baseURL, meta, req)
	if err != nil {
		obs.Err = fmt.Errorf("%w: %v", ErrRequestBuild, err)
		// No middleware ran yet — emit the observation directly so the failure
		// shows up in operator logs with the same shape as a transport error.
		obs.URL = baseURL + meta.Path
		obs.ThreadID = correlationID(ctx)
		obs.emit(callCtx, c.logger)
		return zero, obs.Err
	}
	applyInvokeExtras(httpReq, cfg)

	// SSE endpoints need to announce the content negotiation explicitly.
	// Only set the header when the caller did not — caller's preference
	// (e.g. a vendor-specific MIME) wins.
	if ep.responseSSE && httpReq.Header.Get("Accept") == "" {
		httpReq.Header.Set("Accept", "text/event-stream")
	}

	provider, revocation, err := c.resolveEffectiveAuthProvider(effectiveSvc, cfg)
	if err != nil {
		obs.Err = fmt.Errorf("%w: %v", ErrTokenAcquire, err)
		obs.URL = baseURL + meta.Path
		obs.ThreadID = correlationID(ctx)
		obs.emit(callCtx, c.logger)
		return zero, obs.Err
	}
	effectiveRetry := ep.retry
	if cfg.retryOverride != nil {
		effectiveRetry = resolveRetryOverride(meta.Method, cfg.retryOverride, ep.idempotency.enabled)
	}
	ch := buildChain(effectiveSvc, service, endpoint, ep, effectiveRetry, c.cacheStoreGetter(), c.breaker(service, endpoint), provider, revocation, c.logger)
	resp, err := ch.dispatch(callCtx, httpReq, obs)
	if err != nil {
		return zero, buildHttpError(service, endpoint, httpReq, nil, nil, 0, err, time.Duration(obs.DurationMS)*time.Millisecond, obs.Attempt)
	}

	bodyBytes := obs.ResponseBody
	acceptable := isAcceptable(resp.StatusCode, ep.acceptableStatus, cfg.acceptableStatus)

	if ep.responseStream {
		// 2xx → assemble StreamResponse, leave body open for the caller.
		// Non-2xx → drain + close body, surface HttpError with bytes so
		// the caller sees the upstream error envelope.
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return assembleStreamResponse[Resp](resp, service, endpoint, httpReq, obs)
		}
		drained, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		herr := buildHttpError(service, endpoint, httpReq, resp.Header, drained, resp.StatusCode, nil, time.Duration(obs.DurationMS)*time.Millisecond, obs.Attempt)
		herr.Acceptable = acceptable
		return zero, herr
	}

	if ep.responseSSE {
		// 2xx → start the SSE pump and return SSEResponse, leaving body
		// open until the caller Closes. Non-2xx → drain + close + error.
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return assembleSSEResponse[Resp](callCtx, resp, service, endpoint, httpReq, obs.Attempt)
		}
		drained, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		herr := buildHttpError(service, endpoint, httpReq, resp.Header, drained, resp.StatusCode, nil, time.Duration(obs.DurationMS)*time.Millisecond, obs.Attempt)
		herr.Acceptable = acceptable
		return zero, herr
	}

	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		var out Resp
		if err := binding.DecodeResponse(resp, meta, &out); err != nil {
			return zero, buildHttpError(service, endpoint, httpReq, resp.Header, bodyBytes, resp.StatusCode,
				fmt.Errorf("%w: %v", ErrResponseDecode, err), time.Duration(obs.DurationMS)*time.Millisecond, obs.Attempt)
		}
		return out, nil
	}

	herr := buildHttpError(service, endpoint, httpReq, resp.Header, bodyBytes, resp.StatusCode, nil, time.Duration(obs.DurationMS)*time.Millisecond, obs.Attempt)
	herr.Acceptable = acceptable
	if acceptable {
		var out Resp
		if err := binding.DecodeResponse(resp, meta, &out); err == nil {
			return out, herr
		}
	}
	return zero, herr
}

// assembleSSEResponse spawns the SSE pump and binds it into Resp. The
// caller MUST call Resp.Close() to release the underlying connection.
func assembleSSEResponse[Resp any](ctx context.Context, resp *http.Response, service, endpoint string, req *http.Request, attempt int) (Resp, error) {
	var zero Resp
	var ptr any = &zero
	target, ok := ptr.(*SSEResponse)
	if !ok {
		_ = resp.Body.Close()
		return zero, &HttpError{
			Service:  service,
			Endpoint: endpoint,
			Method:   req.Method,
			URL:      req.URL.String(),
			Status:   resp.StatusCode,
			Cause:    fmt.Errorf("%w: endpoint declares responseSSE: true but Resp is %T, not httpclient.SSEResponse", ErrResponseDecode, zero),
			Attempt:  attempt,
		}
	}
	*target = startSSEPump(ctx, resp.Body)
	return zero, nil
}

// assembleStreamResponse populates a StreamResponse from the open
// *http.Response and returns it as Resp. Verifies that Resp is in fact
// StreamResponse — any other Resp on a responseStream endpoint is a
// caller mistake and is reported as ErrResponseDecode before dialing
// would have made it observable.
func assembleStreamResponse[Resp any](resp *http.Response, service, endpoint string, req *http.Request, obs *observation) (Resp, error) {
	var zero Resp
	var ptr any = &zero
	target, ok := ptr.(*StreamResponse)
	if !ok {
		_ = resp.Body.Close()
		return zero, &HttpError{
			Service:  service,
			Endpoint: endpoint,
			Method:   req.Method,
			URL:      req.URL.String(),
			Status:   resp.StatusCode,
			Cause:    fmt.Errorf("%w: endpoint declares responseStream: true but Resp is %T, not httpclient.StreamResponse", ErrResponseDecode, zero),
			Attempt:  obs.Attempt,
		}
	}
	*target = StreamResponse{
		Body:          resp.Body,
		ContentType:   resp.Header.Get("Content-Type"),
		ContentLength: resp.ContentLength,
		Headers:       resp.Header.Clone(),
	}
	return zero, nil
}

// resolveBaseURL applies the precedence cascade:
//
//  1. per-call CallConfig.BaseURL override (when non-empty)
//  2. registered BaseURLResolver (when returns non-empty url, nil err)
//  3. YAML services.<name>.baseURL (when non-empty)
//  4. clear error
//
// A resolver error short-circuits with the resolver's error wrapped. When
// no resolver is registered, step 2 is skipped entirely so the zero-overhead
// path stays free of an extra interface call. The per-call override is the
// escape hatch for cases the resolver pattern does not fit (webhook callbacks,
// payload-driven routing, ad-hoc scripts); see CallConfig for the catalog.
func (c *HttpClient) resolveBaseURL(ctx context.Context, service, yamlBaseURL, perCallOverride string) (string, error) {
	if perCallOverride != "" {
		return perCallOverride, nil
	}
	if c.resolver == nil {
		if yamlBaseURL == "" {
			return "", fmt.Errorf("httpclient: service %q has no baseURL (YAML empty, no resolver, no CallConfig.BaseURL override)", service)
		}
		return yamlBaseURL, nil
	}
	resolved, err := c.resolver.Resolve(ctx, service)
	if err != nil {
		return "", fmt.Errorf("httpclient: resolve baseURL for service %q: %w", service, err)
	}
	if resolved != "" {
		return resolved, nil
	}
	if yamlBaseURL == "" {
		return "", fmt.Errorf("httpclient: service %q has no baseURL (resolver returned empty, YAML empty, no CallConfig.BaseURL override)", service)
	}
	return yamlBaseURL, nil
}

// applyTimeout layers a per-call timeout on top of the caller-supplied
// context. A non-positive value yields a no-op cancel so callers can defer
// it unconditionally.
func applyTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, d)
}

// correlationID resolves the request's correlation identifier from the
// context. AppContext satisfies domain.Context, so this works for both the
// typical request flow and any direct domain.Context implementation in
// tests or background jobs. Returns "" when neither interface is present.
func correlationID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if dc, ok := ctx.(domain.Context); ok {
		return dc.ID().String()
	}
	return ""
}

// applyInvokeExtras layers per-call header/query overrides on top of the
// already-assembled request. Headers replace existing keys (last write wins);
// query parameters append.
func applyInvokeExtras(req *http.Request, cfg *invokeConfig) {
	for k, v := range cfg.extraHeaders {
		req.Header.Set(k, v)
	}
	if len(cfg.extraQuery) > 0 {
		q := req.URL.Query()
		for k, vs := range cfg.extraQuery {
			for _, v := range vs {
				q.Add(k, v)
			}
		}
		req.URL.RawQuery = q.Encode()
	}
}

// captureRequestObservation records the request-side fields onto the
// observation. Body bytes are recovered from the *http.Request when present
// — BuildRequest stores them via bytes.Reader so we can re-read here without
// disturbing the wire send.
func captureRequestObservation(obs *observation, req *http.Request) {
	obs.URL = req.URL.String()
	obs.RequestHeaders = req.Header
	if req.Body == nil {
		return
	}
	if req.ContentLength > 0 {
		obs.RequestBytes = int(req.ContentLength)
	}
	if obs.streamingRequest {
		// Body stays as the caller's io.Reader; do not buffer it. Retry
		// is disabled at boot for streaming endpoints so the single read
		// the transport performs is the only one we need.
		return
	}
	data, err := io.ReadAll(req.Body)
	if err != nil {
		obs.Err = err
		return
	}
	req.Body = io.NopCloser(bytes.NewReader(data))
	if obs.RequestBytes == 0 {
		obs.RequestBytes = len(data)
	}
	obs.RequestBody = data
}

// isAcceptable reports whether status appears in either the endpoint's
// declared acceptable set or the per-call override.
func isAcceptable(status int, fromYAML, fromCall map[int]struct{}) bool {
	if _, ok := fromYAML[status]; ok {
		return true
	}
	_, ok := fromCall[status]
	return ok
}

// buildHttpError assembles the *HttpError carrying every diagnostic the
// caller might need to react. Method/URL are pulled from the *http.Request
// the call had already built so an early dial failure still surfaces them.
// attempt is the 1-indexed retry attempt count read from obs.Attempt by the
// caller, so the consumer can branch on "exhausted N tries" without
// inspecting the observation directly.
func buildHttpError(service, endpoint string, req *http.Request, headers http.Header, body []byte, status int, cause error, duration time.Duration, attempt int) *HttpError {
	method, urlStr := "", ""
	if req != nil {
		method = req.Method
		if req.URL != nil {
			urlStr = req.URL.String()
		}
	}
	if attempt < 1 {
		attempt = 1
	}
	return &HttpError{
		Service:  service,
		Endpoint: endpoint,
		Method:   method,
		URL:      urlStr,
		Status:   status,
		Headers:  headers,
		Body:     body,
		Duration: duration,
		Cause:    cause,
		Attempt:  attempt,
	}
}

// bestEffortURL returns the URL the caller intended to dial, preferring
// the per-call CallConfig.BaseURL override over the YAML baseURL and the
// effective endpoint path (which already reflects CallConfig.Path). Used
// in pre-dispatch error paths where the resolved URL is unavailable but
// the operator still needs the override visible in the slog observation.
func bestEffortURL(svc *serviceClient, cfg *invokeConfig, meta binding.EndpointMeta) string {
	base := ""
	if cfg != nil {
		base = cfg.baseURLOverride
	}
	if base == "" && svc != nil {
		base = svc.baseURL
	}
	return base + meta.Path
}
