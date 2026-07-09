package httpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"

	"github.com/ClaudioSchirmer/omnicore/infra/httpclient/binding"
)

// Fake is the in-memory testing harness for the outbound HTTP subsystem.
// Construct one per test, hand fake.Client() to the code under test, register
// stubs via WhenCalled, then read the recorded Calls() for assertions.
//
// The fake short-circuits the middleware chain entirely — no retry, cache,
// breaker, auth, or transport runs. That makes tests deterministic and fast
// at the cost of not exercising the runtime middleware; integration testing
// of those layers belongs to the QA suites against a real httptest.Server.
//
// The real binding layer DOES run on every call, so request structs with
// http:"..." tags are validated and serialized the same way the production
// client validates them — handler bugs around tag misuse surface in fake
// tests too.
type Fake struct {
	client *HttpClient
}

// FakeOption customizes the harness.
type FakeOption func(*Fake)

// WithFakeLogger swaps the slog.Logger the fake's underlying client carries.
// Defaults to slog.Default(). Useful when tests want to silence output or
// assert on log records via a slog handler.
func WithFakeLogger(l *slog.Logger) FakeOption {
	return func(f *Fake) {
		if l != nil {
			f.client.logger = l
		}
	}
}

// NewFake constructs an in-memory test harness with a *HttpClient ready to
// hand into consumer code. The returned *HttpClient carries the same exported
// surface as the production client — drop-in for any constructor parameter
// typed *HttpClient.
func NewFake(opts ...FakeOption) *Fake {
	reg := &fakeRegistry{
		specs: map[string]binding.EndpointMeta{},
		stubs: map[string][]*FakeStub{},
		calls: map[string][]FakeCall{},
	}
	f := &Fake{
		client: &HttpClient{
			logger: slog.Default(),
			fake:   reg,
		},
	}
	for _, o := range opts {
		if o != nil {
			o(f)
		}
	}
	return f
}

// Client returns the *HttpClient to wire into the code under test. Calling
// httpclient.Call[Req, Resp] against this client routes through the fake's
// stub registry instead of the network.
func (f *Fake) Client() *HttpClient {
	return f.client
}

// Register sets the endpoint metadata explicitly. Optional — when omitted,
// the first WhenCalled(svc, ep) auto-registers a default spec:
// method GET, path "/{endpoint}", json/json codecs. Use Register when the
// endpoint declares path placeholders, a non-GET method, or non-JSON codecs
// and the test depends on those for matching / serialization.
func (f *Fake) Register(service, endpoint string, meta binding.EndpointMeta) {
	key := fakeKey(service, endpoint)
	f.client.fake.mu.Lock()
	defer f.client.fake.mu.Unlock()
	f.client.fake.specs[key] = normalizeFakeMeta(endpoint, meta)
}

// WhenCalled registers a new stub for (service, endpoint). The returned
// FakeStub starts as "200 OK, empty body, matches one call" — chain the
// builder methods to refine. If the (service, endpoint) pair has no
// explicit Register, defaults are inferred on first use.
func (f *Fake) WhenCalled(service, endpoint string) *FakeStub {
	key := fakeKey(service, endpoint)
	f.client.fake.mu.Lock()
	defer f.client.fake.mu.Unlock()
	if _, ok := f.client.fake.specs[key]; !ok {
		f.client.fake.specs[key] = defaultFakeMeta(endpoint)
	}
	stub := &FakeStub{
		service:  service,
		endpoint: endpoint,
		status:   http.StatusOK,
		headers:  http.Header{},
		times:    1,
	}
	f.client.fake.stubs[key] = append(f.client.fake.stubs[key], stub)
	return stub
}

// Calls returns every recorded call for (service, endpoint), in invocation
// order. Returns an empty slice when nothing has been recorded.
func (f *Fake) Calls(service, endpoint string) []FakeCall {
	key := fakeKey(service, endpoint)
	f.client.fake.mu.Lock()
	defer f.client.fake.mu.Unlock()
	src := f.client.fake.calls[key]
	if len(src) == 0 {
		return nil
	}
	out := make([]FakeCall, len(src))
	copy(out, src)
	return out
}

// Reset clears every stub and every recorded call. Useful between subtests
// when the same Fake instance is reused.
func (f *Fake) Reset() {
	f.client.fake.mu.Lock()
	defer f.client.fake.mu.Unlock()
	f.client.fake.specs = map[string]binding.EndpointMeta{}
	f.client.fake.stubs = map[string][]*FakeStub{}
	f.client.fake.calls = map[string][]FakeCall{}
}

// AssertExpectations returns a non-nil error when any registered stub did
// not match the number of calls it expected. A stub's expectation is the
// Times(n) value set on it (default 1; Always() disables the check).
//
// Use as:
//
//	t.Cleanup(func() { require.NoError(t, fake.AssertExpectations()) })
func (f *Fake) AssertExpectations() error {
	f.client.fake.mu.Lock()
	defer f.client.fake.mu.Unlock()
	var problems []string
	for key, stubs := range f.client.fake.stubs {
		for i, s := range stubs {
			if s.always {
				continue
			}
			if s.matched != s.times {
				problems = append(problems, fmt.Sprintf(
					"stub %s#%d: expected %d call(s), got %d",
					key, i, s.times, s.matched,
				))
			}
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("httpclient.Fake: unmet expectations:\n  - %s",
		strings.Join(problems, "\n  - "))
}

// FakeStub is the fluent builder for a single registered response.
type FakeStub struct {
	service  string
	endpoint string

	status        int
	body          []byte
	contentType   string
	headers       http.Header
	transportErr  error
	matchers      []func(FakeCall) bool
	times         int
	always        bool
	matched       int
	returnsSet    bool
	returnsValue  any
	usesValueBody bool
}

// Return registers a typed response value. The framework JSON-marshals the
// value and DecodeResponse on the call side decodes it back into the typed
// Resp — the round-trip matches the production path for json endpoints.
// Status defaults to 200; override with Status(code).
//
// Use ReturnBytes when the endpoint uses a non-JSON codec or when the test
// needs control over the raw body bytes.
func (s *FakeStub) Return(value any) *FakeStub {
	s.returnsSet = true
	s.returnsValue = value
	s.usesValueBody = true
	if s.contentType == "" {
		s.contentType = "application/json"
	}
	return s
}

// ReturnBytes registers a raw response body with explicit Content-Type.
// Status defaults to 200; override with Status(code).
func (s *FakeStub) ReturnBytes(body []byte, contentType string) *FakeStub {
	s.returnsSet = true
	s.body = append([]byte(nil), body...)
	s.contentType = contentType
	s.usesValueBody = false
	return s
}

// ReturnError registers a non-2xx response. The body is the raw bytes
// (typically an error envelope). The fake returns *HttpError to the caller
// matching the production shape.
func (s *FakeStub) ReturnError(status int, body []byte) *FakeStub {
	s.returnsSet = true
	s.status = status
	s.body = append([]byte(nil), body...)
	s.usesValueBody = false
	return s
}

// ReturnTransportError registers a transport-level failure (timeout, dial
// error, connection refused). The fake returns *HttpError{Status: 0, Cause:
// cause} matching the production shape.
func (s *FakeStub) ReturnTransportError(cause error) *FakeStub {
	s.returnsSet = true
	s.transportErr = cause
	return s
}

// Status overrides the response status code. Defaults to 200 for Return /
// ReturnBytes; ReturnError sets it explicitly.
func (s *FakeStub) Status(code int) *FakeStub {
	s.status = code
	return s
}

// WithHeader adds a response header. Repeats append per http.Header
// semantics.
func (s *FakeStub) WithHeader(key, value string) *FakeStub {
	s.headers.Add(key, value)
	return s
}

// Times sets how many calls this stub matches before becoming exhausted.
// Default is 1. Subsequent calls fall through to the next matching stub.
// n must be >= 1 — for "this endpoint must never be called", omit the stub
// entirely and assert len(fake.Calls(svc, ep)) == 0 in the test.
func (s *FakeStub) Times(n int) *FakeStub {
	if n < 1 {
		n = 1
	}
	s.times = n
	return s
}

// Always makes the stub match every call without exhausting. AssertExpectations
// skips Always() stubs.
func (s *FakeStub) Always() *FakeStub {
	s.always = true
	return s
}

// Match adds a custom predicate. All registered predicates must return true
// for the stub to match. The supplied FakeCall has Service, Endpoint, Method,
// URL, Path, Query, Headers, Body populated.
func (s *FakeStub) Match(pred func(FakeCall) bool) *FakeStub {
	if pred != nil {
		s.matchers = append(s.matchers, pred)
	}
	return s
}

// MatchPath restricts the stub to calls where the named path placeholder
// equals value. Convenience wrapper around Match.
func (s *FakeStub) MatchPath(name, value string) *FakeStub {
	return s.Match(func(c FakeCall) bool {
		got, ok := c.Path[name]
		return ok && got == value
	})
}

// MatchQuery restricts the stub to calls carrying ?key=value. Convenience
// wrapper around Match. Repeated query values match any occurrence.
func (s *FakeStub) MatchQuery(key, value string) *FakeStub {
	return s.Match(func(c FakeCall) bool {
		for _, v := range c.Query[key] {
			if v == value {
				return true
			}
		}
		return false
	})
}

// MatchHeader restricts the stub to calls carrying the named outbound
// header with the given value. Convenience wrapper around Match.
func (s *FakeStub) MatchHeader(key, value string) *FakeStub {
	return s.Match(func(c FakeCall) bool {
		for _, v := range c.Headers.Values(key) {
			if v == value {
				return true
			}
		}
		return false
	})
}

// FakeCall is the recorded view of one outbound invocation. Tests read
// these via fake.Calls(service, endpoint) to assert what the consumer sent.
type FakeCall struct {
	Service  string
	Endpoint string
	Method   string
	URL      string
	Path     map[string]string
	Query    url.Values
	Headers  http.Header
	Body     []byte
}

// fakeRegistry is the unexported state attached to a fake HttpClient. The
// real production HttpClient leaves the field nil.
type fakeRegistry struct {
	mu    sync.Mutex
	specs map[string]binding.EndpointMeta
	stubs map[string][]*FakeStub
	calls map[string][]FakeCall
}

func fakeKey(service, endpoint string) string {
	return service + "|" + endpoint
}

func defaultFakeMeta(endpoint string) binding.EndpointMeta {
	return binding.EndpointMeta{
		Method:        http.MethodGet,
		Path:          "/" + endpoint,
		RequestCodec:  "json",
		ResponseCodec: "json",
	}
}

func normalizeFakeMeta(endpoint string, meta binding.EndpointMeta) binding.EndpointMeta {
	if meta.Method == "" {
		meta.Method = http.MethodGet
	} else {
		meta.Method = strings.ToUpper(meta.Method)
	}
	if meta.Path == "" {
		meta.Path = "/" + endpoint
	}
	if meta.RequestCodec == "" {
		meta.RequestCodec = "json"
	}
	if meta.ResponseCodec == "" {
		meta.ResponseCodec = "json"
	}
	return meta
}

// fakeCall is the dispatch entry point invoked from Call when c.fake != nil.
// Runs real binding (request build + response decode) but routes the response
// through the in-memory stub registry instead of an http.Transport.
func fakeCall[Req any, Resp any](
	ctx context.Context,
	c *HttpClient,
	service, endpoint string,
	req Req,
	opts []InvokeOption,
) (Resp, error) {
	var zero Resp
	if ctx == nil {
		ctx = context.Background()
	}
	cfg := applyInvokeOptions(opts)
	callCtx, cancel := applyTimeout(ctx, cfg.timeout)
	defer cancel()

	key := fakeKey(service, endpoint)
	c.fake.mu.Lock()
	meta, ok := c.fake.specs[key]
	if !ok {
		meta = defaultFakeMeta(endpoint)
		c.fake.specs[key] = meta
	}
	c.fake.mu.Unlock()

	httpReq, err := binding.BuildRequest(callCtx, "http://fake.invalid", meta, req)
	if err != nil {
		return zero, &HttpError{
			Service:  service,
			Endpoint: endpoint,
			Method:   meta.Method,
			URL:      "http://fake.invalid" + meta.Path,
			Cause:    fmt.Errorf("%w: %v", ErrRequestBuild, err),
			Attempt:  1,
		}
	}
	applyInvokeExtras(httpReq, cfg)

	call := buildFakeCall(service, endpoint, meta, httpReq)
	c.fake.mu.Lock()
	c.fake.calls[key] = append(c.fake.calls[key], call)
	stub := pickFakeStub(c.fake.stubs[key], call)
	c.fake.mu.Unlock()

	if stub == nil {
		return zero, &HttpError{
			Service:  service,
			Endpoint: endpoint,
			Method:   call.Method,
			URL:      call.URL,
			Cause:    ErrFakeUnstubbed,
			Attempt:  1,
		}
	}

	if stub.transportErr != nil {
		return zero, &HttpError{
			Service:  service,
			Endpoint: endpoint,
			Method:   call.Method,
			URL:      call.URL,
			Cause:    stub.transportErr,
			Attempt:  1,
		}
	}

	resp, body, err := buildFakeResponse(stub, httpReq)
	if err != nil {
		return zero, &HttpError{
			Service:  service,
			Endpoint: endpoint,
			Method:   call.Method,
			URL:      call.URL,
			Cause:    fmt.Errorf("%w: %v", ErrResponseDecode, err),
			Attempt:  1,
		}
	}
	defer func() { _ = resp.Body.Close() }()

	acceptable := isAcceptable(resp.StatusCode, meta.AcceptableStatus, cfg.acceptableStatus)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		var out Resp
		if err := binding.DecodeResponse(resp, meta, &out); err != nil {
			return zero, &HttpError{
				Service:  service,
				Endpoint: endpoint,
				Method:   call.Method,
				URL:      call.URL,
				Status:   resp.StatusCode,
				Headers:  resp.Header,
				Body:     body,
				Cause:    fmt.Errorf("%w: %v", ErrResponseDecode, err),
				Attempt:  1,
			}
		}
		return out, nil
	}

	herr := &HttpError{
		Service:    service,
		Endpoint:   endpoint,
		Method:     call.Method,
		URL:        call.URL,
		Status:     resp.StatusCode,
		Headers:    resp.Header,
		Body:       body,
		Acceptable: acceptable,
		Attempt:    1,
	}
	if acceptable {
		var out Resp
		if err := binding.DecodeResponse(resp, meta, &out); err == nil {
			return out, herr
		}
	}
	return zero, herr
}

// buildFakeCall extracts the recorded view of the call from the assembled
// *http.Request. Path placeholders are recovered by matching the spec path
// against the actual URL.Path.
func buildFakeCall(service, endpoint string, meta binding.EndpointMeta, req *http.Request) FakeCall {
	c := FakeCall{
		Service:  service,
		Endpoint: endpoint,
		Method:   req.Method,
		URL:      req.URL.String(),
		Path:     extractPathParams(meta.Path, req.URL.Path),
		Query:    req.URL.Query(),
		Headers:  req.Header.Clone(),
	}
	if req.Body != nil {
		data, err := io.ReadAll(req.Body)
		if err == nil {
			c.Body = data
			req.Body = io.NopCloser(bytes.NewReader(data))
		}
	}
	return c
}

// pathParamRegex caches the compiled regex per spec path so repeated calls
// don't re-parse the placeholder syntax.
var pathParamRegex sync.Map // map[string]*pathParamMatcher

type pathParamMatcher struct {
	re    *regexp.Regexp
	names []string
}

// extractPathParams matches actualPath against specPath and returns the
// resolved placeholders. specPath has {name} markers; actualPath is the
// URL.Path after binding substitution (URL-decoded values). Returns nil when
// no placeholders exist or the match fails.
func extractPathParams(specPath, actualPath string) map[string]string {
	if !strings.Contains(specPath, "{") {
		return nil
	}
	m := compilePathMatcher(specPath)
	if m == nil {
		return nil
	}
	matches := m.re.FindStringSubmatch(actualPath)
	if len(matches) == 0 {
		return nil
	}
	out := make(map[string]string, len(m.names))
	for i, name := range m.names {
		raw := matches[i+1]
		if decoded, err := url.PathUnescape(raw); err == nil {
			out[name] = decoded
		} else {
			out[name] = raw
		}
	}
	return out
}

var placeholderFinder = regexp.MustCompile(`\{([^}]+)\}`)

func compilePathMatcher(specPath string) *pathParamMatcher {
	if v, ok := pathParamRegex.Load(specPath); ok {
		return v.(*pathParamMatcher)
	}
	var (
		names   []string
		pattern strings.Builder
		lastEnd int
	)
	pattern.WriteString("^")
	for _, m := range placeholderFinder.FindAllStringSubmatchIndex(specPath, -1) {
		// m = [start, end, nameStart, nameEnd]
		pattern.WriteString(regexp.QuoteMeta(specPath[lastEnd:m[0]]))
		pattern.WriteString(`([^/]+)`)
		names = append(names, specPath[m[2]:m[3]])
		lastEnd = m[1]
	}
	pattern.WriteString(regexp.QuoteMeta(specPath[lastEnd:]))
	pattern.WriteString("$")
	re, err := regexp.Compile(pattern.String())
	if err != nil {
		return nil
	}
	mp := &pathParamMatcher{re: re, names: names}
	pathParamRegex.Store(specPath, mp)
	return mp
}

// pickFakeStub walks the registered stubs in order, returning the first one
// whose matchers all return true and which still has remaining matches.
// Increments the matched counter on the chosen stub.
func pickFakeStub(stubs []*FakeStub, call FakeCall) *FakeStub {
	for _, s := range stubs {
		if !s.always && s.matched >= s.times {
			continue
		}
		ok := true
		for _, m := range s.matchers {
			if !m(call) {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		s.matched++
		return s
	}
	return nil
}

// buildFakeResponse materializes an *http.Response from the stub. The body
// bytes are returned alongside so the caller's HttpError can carry them
// without re-reading the body (DecodeResponse closes it).
func buildFakeResponse(stub *FakeStub, req *http.Request) (*http.Response, []byte, error) {
	body := stub.body
	if stub.usesValueBody {
		data, err := json.Marshal(stub.returnsValue)
		if err != nil {
			return nil, nil, err
		}
		body = data
	}

	status := stub.status
	if status == 0 {
		status = http.StatusOK
	}

	headers := stub.headers.Clone()
	if headers == nil {
		headers = http.Header{}
	}
	if stub.contentType != "" && headers.Get("Content-Type") == "" {
		headers.Set("Content-Type", stub.contentType)
	}

	bodyCopy := append([]byte(nil), body...)
	resp := &http.Response{
		StatusCode:    status,
		Status:        fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:        headers,
		Body:          io.NopCloser(bytes.NewReader(bodyCopy)),
		ContentLength: int64(len(bodyCopy)),
		Request:       req,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
	}
	return resp, bodyCopy, nil
}
