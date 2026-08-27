package httpclient

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The state-kind labels moved to infra/resilience (BreakerState.String);
// their label coverage lives in that package's tests. Here we keep the
// shim-level contract: a fresh breaker snapshots "closed".
func TestBreakerState_FreshIsClosed(t *testing.T) {
	b := newBreakerState(breakerPolicy{enabled: true, failureThreshold: 1, successThreshold: 1, openFor: time.Minute})
	if got := b.snapshotState(); got != "closed" {
		t.Errorf("fresh breaker snapshot = %q, want closed", got)
	}
}

// snapshotState on a nil/disabled breaker returns "closed" without locking.
func TestBreakerState_SnapshotState_NilAndDisabled(t *testing.T) {
	var nilB *breakerState
	if got := nilB.snapshotState(); got != "closed" {
		t.Errorf("nil breaker snapshot = %q, want closed", got)
	}
	disabled := &breakerState{policy: breakerPolicy{enabled: false}}
	if got := disabled.snapshotState(); got != "closed" {
		t.Errorf("disabled breaker snapshot = %q, want closed", got)
	}
	// drive an enabled breaker to open through the public seam (the state
	// field moved into resilience.Breaker)
	enabled := newBreakerState(breakerPolicy{enabled: true, failureThreshold: 1, successThreshold: 1, openFor: time.Minute})
	enabled.recordFailure()
	if got := enabled.snapshotState(); got != "open" {
		t.Errorf("enabled open breaker snapshot = %q, want open", got)
	}
}

// correlationID degenerate inputs: nil ctx and a plain context (no RequestContext).
func TestCorrelationID_Degenerate(t *testing.T) {
	if got := correlationID(nil); got != "" { //nolint:staticcheck // SA1012: nil ctx is the degenerate input under test.
		t.Errorf("correlationID(nil) = %q, want empty", got)
	}
	if got := correlationID(context.Background()); got != "" {
		t.Errorf("correlationID(plain ctx) = %q, want empty", got)
	}
}

// drainAndClose: nil response, nil body, and a real body all return nil.
type closeTracker struct {
	io.Reader
	closed bool
}

func (c *closeTracker) Close() error { c.closed = true; return nil }

func TestDrainAndClose_Branches(t *testing.T) {
	if err := drainAndClose(nil); err != nil {
		t.Errorf("nil response: %v", err)
	}
	if err := drainAndClose(&http.Response{}); err != nil {
		t.Errorf("nil body: %v", err)
	}
	body := &closeTracker{Reader: strings.NewReader("payload")}
	if err := drainAndClose(&http.Response{Body: body}); err != nil {
		t.Errorf("with body: %v", err)
	}
	if !body.closed {
		t.Error("body should have been closed (drained then closed)")
	}
}

// assembleSSEResponse rejects a Resp type that is not SSEResponse.
func TestAssembleSSEResponse_WrongRespType(t *testing.T) {
	resp := &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader("data: x\n\n")),
		Header:     http.Header{},
	}
	req, _ := http.NewRequest(http.MethodGet, "http://x/y", nil)
	// Resp is string, not SSEResponse → ErrResponseDecode.
	_, err := assembleSSEResponse[string](context.Background(), resp, "svc", "ep", req, 1)
	if err == nil {
		t.Fatal("expected ErrResponseDecode for a non-SSE Resp type")
	}
	if he, ok := err.(*HttpError); !ok || he.Service != "svc" {
		t.Errorf("expected *HttpError with service svc, got %v", err)
	}
}

// resolveRetryOverride: RespectRetryAfter honored; POST clamped to 1 attempt
// without idempotency, but allowed when idempotency present.
func TestResolveRetryOverride_Branches(t *testing.T) {
	yes := true
	override := &RetryOverride{
		MaxAttempts:       5,
		Backoff:           "constant",
		InitialDelay:      10 * time.Millisecond,
		MaxDelay:          50 * time.Millisecond,
		RetryOn:           []string{"503"},
		RespectRetryAfter: &yes,
	}
	// GET allows retry → full budget.
	p := resolveRetryOverride(http.MethodGet, override, false)
	if p.maxAttempts != 5 {
		t.Errorf("GET override maxAttempts = %d, want 5", p.maxAttempts)
	}
	if !p.respectRetryAfter {
		t.Error("RespectRetryAfter pointer-true must propagate")
	}
	// POST without idempotency → clamped to 1.
	pp := resolveRetryOverride(http.MethodPost, override, false)
	if pp.maxAttempts != 1 {
		t.Errorf("POST without idempotency must clamp to 1, got %d", pp.maxAttempts)
	}
	// POST with idempotency → budget honored.
	ppp := resolveRetryOverride(http.MethodPost, override, true)
	if ppp.maxAttempts != 5 {
		t.Errorf("POST with idempotency keeps budget, got %d", ppp.maxAttempts)
	}
}

// validateDefaults: a negative defaults timeout is rejected.
func TestValidateDefaults_NegativeTimeout(t *testing.T) {
	c := &Config{Defaults: Defaults{Timeout: Duration(-time.Second)}}
	var errs validationErrors
	c.validateDefaults(&errs)
	if !joinHas(errs, "defaults.timeout") {
		t.Errorf("expected negative timeout error, got %v", errs)
	}
}

// validateAuthProviderShape covers each provider type's required-field branches.
func TestValidateAuthProviderShape_Branches(t *testing.T) {
	cases := []struct {
		name string
		typ  string
		p    AuthProviderConfig
		want string // substring expected in at least one error
	}{
		{"header-static missing name+value", "header-static", AuthProviderConfig{}, "attach.name"},
		{"bearer-static missing token", "bearer-static", AuthProviderConfig{}, "token"},
		{"basic missing username", "basic", AuthProviderConfig{Password: "p"}, "username"},
		{"basic missing password", "basic", AuthProviderConfig{Username: "u"}, "password"},
		{"oauth2 missing endpoint", "oauth2-client-credentials", AuthProviderConfig{ClientID: "i", ClientSecret: "s", TokenCache: &TokenCacheConfig{Source: "jwt-exp"}}, "tokenEndpoint"},
		{"oauth2 bad url", "oauth2-client-credentials", AuthProviderConfig{TokenEndpoint: "not-a-url", ClientID: "i", ClientSecret: "s", TokenCache: &TokenCacheConfig{Source: "jwt-exp"}}, "absolute URL"},
		{"oauth2 missing clientid", "oauth2-client-credentials", AuthProviderConfig{TokenEndpoint: "https://idp/t", ClientSecret: "s", TokenCache: &TokenCacheConfig{Source: "jwt-exp"}}, "clientId"},
		{"oauth2 missing secret", "oauth2-client-credentials", AuthProviderConfig{TokenEndpoint: "https://idp/t", ClientID: "i", TokenCache: &TokenCacheConfig{Source: "jwt-exp"}}, "clientSecret"},
		{"credx missing endpoint", "credentials-exchange", AuthProviderConfig{RequestFields: map[string]string{"a": "b"}, ResponseTokenPath: "$.t", TokenCache: &TokenCacheConfig{Source: "jwt-exp"}}, "tokenEndpoint"},
		{"credx empty fields", "credentials-exchange", AuthProviderConfig{TokenEndpoint: "https://idp/t", ResponseTokenPath: "$.t", TokenCache: &TokenCacheConfig{Source: "jwt-exp"}}, "requestFields"},
		{"credx missing responsepath", "credentials-exchange", AuthProviderConfig{TokenEndpoint: "https://idp/t", RequestFields: map[string]string{"a": "b"}, TokenCache: &TokenCacheConfig{Source: "jwt-exp"}}, "responseTokenPath"},
		{"credx bad responsepath prefix", "credentials-exchange", AuthProviderConfig{TokenEndpoint: "https://idp/t", RequestFields: map[string]string{"a": "b"}, ResponseTokenPath: "active", TokenCache: &TokenCacheConfig{Source: "jwt-exp"}}, "must start with $"},
		{"credx bad codec", "credentials-exchange", AuthProviderConfig{TokenEndpoint: "https://idp/t", RequestFields: map[string]string{"a": "b"}, ResponseTokenPath: "$.t", RequestCodec: "xml", TokenCache: &TokenCacheConfig{Source: "jwt-exp"}}, "requestCodec"},
		{"attach bad as", "bearer-static", AuthProviderConfig{Token: "t", Attach: &AttachConfig{As: "nonsense"}}, "attach.as"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateAuthProviderShape("p", tc.typ, tc.p)
			if !joinHas(errs, tc.want) {
				t.Errorf("expected an error containing %q, got %v", tc.want, errs)
			}
		})
	}
	// A fully valid bearer-static + good attach produces no errors.
	ok := validateAuthProviderShape("p", "bearer-static", AuthProviderConfig{Token: "t", Attach: &AttachConfig{As: "header", Name: "Authorization", Value: "x"}})
	if len(ok) != 0 {
		t.Errorf("valid bearer-static should produce no errors, got %v", ok)
	}
}

func joinHas(errs []string, sub string) bool {
	return strings.Contains(strings.Join(errs, "\n"), sub)
}

// validateAuthProviders: empty type and unrecognized type branches. A provider
// type outside the supported set is rejected as unrecognized, whatever it is —
// there is no second tier of "declared but not implemented".
func TestValidateAuthProviders_Branches(t *testing.T) {
	if errs := validateAuthProviders(nil); errs != nil {
		t.Errorf("empty map should yield nil, got %v", errs)
	}
	errs := validateAuthProviders(map[string]AuthProviderConfig{
		"a": {Type: ""},                // missing type
		"b": {Type: "oauth2-password"}, // outside the supported set
		"c": {Type: "totally-bogus"},   // unrecognized
	})
	for _, want := range []string{"type: required", `"oauth2-password" is not a recognized`, `"totally-bogus" is not a recognized`} {
		if !joinHas(errs, want) {
			t.Errorf("expected an error containing %q, got %v", want, errs)
		}
	}
}

// validateServiceAuthReferences: nil auth skipped, empty provider, undeclared.
func TestValidateServiceAuthReferences_Branches(t *testing.T) {
	services := map[string]ServiceConfig{
		"noauth":   {}, // Auth nil → skipped
		"empty":    {Auth: &ServiceAuthConfig{Provider: ""}},
		"dangling": {Auth: &ServiceAuthConfig{Provider: "ghost"}},
		"ok":       {Auth: &ServiceAuthConfig{Provider: "real"}},
	}
	providers := map[string]AuthProviderConfig{"real": {Type: "none"}}
	errs := validateServiceAuthReferences(services, providers)
	if !joinHas(errs, "empty.auth.provider: required") {
		t.Errorf("expected required-provider error, got %v", errs)
	}
	if !joinHas(errs, "is not declared") {
		t.Errorf("expected undeclared-provider error, got %v", errs)
	}
	if joinHas(errs, "ok.auth.provider") {
		t.Errorf("declared provider should not error, got %v", errs)
	}
}
