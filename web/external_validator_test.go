package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

// --- newExternalValidator: schema invariants --------------------------------

func TestNewExternalValidator_RequiresURL(t *testing.T) {
	_, err := newExternalValidator(ExternalValidatorOptions{
		TokenPlacement: "bearer_header",
		Success:        ExternalValidatorSuccess{JSONPath: "$.active", ExpectedValue: true},
	})
	if err == nil || !strings.Contains(err.Error(), "URL") {
		t.Fatalf("expected URL-required error, got %v", err)
	}
}

func TestNewExternalValidator_InvalidMethod(t *testing.T) {
	_, err := newExternalValidator(ExternalValidatorOptions{
		Method:         "PATCH",
		URL:            "https://idp/introspect",
		TokenPlacement: "bearer_header",
		Success:        ExternalValidatorSuccess{JSONPath: "$.active", ExpectedValue: true},
	})
	if err == nil || !strings.Contains(err.Error(), "method") {
		t.Fatalf("expected method error, got %v", err)
	}
}

func TestNewExternalValidator_DefaultsMethodAndTimeoutAndFailMode(t *testing.T) {
	v, err := newExternalValidator(ExternalValidatorOptions{
		URL:            "https://idp/introspect",
		TokenPlacement: "bearer_header",
		Success:        ExternalValidatorSuccess{JSONPath: "$.active", ExpectedValue: true},
	})
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if v.method != http.MethodPost {
		t.Errorf("method default = %q, want POST", v.method)
	}
	if v.client.Timeout != 2*time.Second {
		t.Errorf("timeout default = %v, want 2s", v.client.Timeout)
	}
	if v.failOpen {
		t.Error("failMode default should be closed (failOpen=false)")
	}
}

func TestNewExternalValidator_FormPlacementRequiresTokenField(t *testing.T) {
	_, err := newExternalValidator(ExternalValidatorOptions{
		URL:            "https://idp/introspect",
		TokenPlacement: "form_field",
		Success:        ExternalValidatorSuccess{JSONPath: "$.active", ExpectedValue: true},
	})
	if err == nil || !strings.Contains(err.Error(), "tokenField") {
		t.Fatalf("expected tokenField-required error, got %v", err)
	}
}

func TestNewExternalValidator_InvalidPlacement(t *testing.T) {
	_, err := newExternalValidator(ExternalValidatorOptions{
		URL:            "https://idp/introspect",
		TokenPlacement: "cookie",
		Success:        ExternalValidatorSuccess{JSONPath: "$.active", ExpectedValue: true},
	})
	if err == nil || !strings.Contains(err.Error(), "tokenPlacement") {
		t.Fatalf("expected placement error, got %v", err)
	}
}

func TestNewExternalValidator_InvalidFailMode(t *testing.T) {
	_, err := newExternalValidator(ExternalValidatorOptions{
		URL:            "https://idp/introspect",
		TokenPlacement: "bearer_header",
		Success:        ExternalValidatorSuccess{JSONPath: "$.active", ExpectedValue: true},
		FailMode:       "silent",
	})
	if err == nil || !strings.Contains(err.Error(), "failMode") {
		t.Fatalf("expected failMode error, got %v", err)
	}
}

func TestNewExternalValidator_MissingJSONPath(t *testing.T) {
	_, err := newExternalValidator(ExternalValidatorOptions{
		URL:            "https://idp/introspect",
		TokenPlacement: "bearer_header",
		Success:        ExternalValidatorSuccess{ExpectedValue: true},
	})
	if err == nil || !strings.Contains(err.Error(), "jsonPath") {
		t.Fatalf("expected jsonPath error, got %v", err)
	}
}

func TestNewExternalValidator_MissingExpectedValue(t *testing.T) {
	_, err := newExternalValidator(ExternalValidatorOptions{
		URL:            "https://idp/introspect",
		TokenPlacement: "bearer_header",
		Success:        ExternalValidatorSuccess{JSONPath: "$.active"},
	})
	if err == nil || !strings.Contains(err.Error(), "expectedValue") {
		t.Fatalf("expected expectedValue error, got %v", err)
	}
}

// --- parseJSONPath / lookupJSONPath -----------------------------------------

func TestParseJSONPath(t *testing.T) {
	cases := []struct {
		in   string
		want []string
		err  bool
	}{
		{"$", nil, false},
		{"$.active", []string{"active"}, false},
		{"$.data.is_active", []string{"data", "is_active"}, false},
		{"active", nil, true},     // missing $
		{"$active", nil, true},    // missing . after $
		{"$..active", nil, true},  // empty segment
	}
	for _, tc := range cases {
		got, err := parseJSONPath(tc.in)
		if tc.err {
			if err == nil {
				t.Errorf("parseJSONPath(%q): expected error", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseJSONPath(%q): unexpected error %v", tc.in, err)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("parseJSONPath(%q) = %#v, want %#v", tc.in, got, tc.want)
		}
	}
}

func TestLookupJSONPath(t *testing.T) {
	payload := map[string]any{
		"active": true,
		"data": map[string]any{
			"is_active": false,
			"role":      "admin",
		},
	}
	cases := []struct {
		segs []string
		ok   bool
		want any
	}{
		{[]string{"active"}, true, true},
		{[]string{"data", "is_active"}, true, false},
		{[]string{"data", "role"}, true, "admin"},
		{[]string{"missing"}, false, nil},
		{[]string{"data", "missing"}, false, nil},
		{[]string{"active", "deeper"}, false, nil}, // can't traverse through non-map
	}
	for _, tc := range cases {
		got, ok := lookupJSONPath(payload, tc.segs)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("lookup(%v) = (%v, %v), want (%v, %v)", tc.segs, got, ok, tc.want, tc.ok)
		}
	}
}

// --- Validate end-to-end via httptest ---------------------------------------

func makeValidatorServer(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	return httptest.NewServer(h)
}

func validateOpts(url, placement, tokenField string) ExternalValidatorOptions {
	return ExternalValidatorOptions{
		Method:         "POST",
		URL:            url,
		TokenPlacement: placement,
		TokenField:     tokenField,
		Success:        ExternalValidatorSuccess{JSONPath: "$.active", ExpectedValue: true},
	}
}

func TestValidate_BearerHeader(t *testing.T) {
	var seenAuth string
	srv := makeValidatorServer(t, func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"active":true}`))
	})
	defer srv.Close()
	v, err := newExternalValidator(validateOpts(srv.URL, "bearer_header", ""))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := v.Validate(context.Background(), "tok-abc"); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if seenAuth != "Bearer tok-abc" {
		t.Errorf("server saw Authorization=%q, want %q", seenAuth, "Bearer tok-abc")
	}
}

func TestValidate_FormField(t *testing.T) {
	var seenForm string
	var seenCT string
	srv := makeValidatorServer(t, func(w http.ResponseWriter, r *http.Request) {
		seenCT = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		seenForm = string(body)
		_, _ = w.Write([]byte(`{"active":true}`))
	})
	defer srv.Close()
	v, err := newExternalValidator(validateOpts(srv.URL, "form_field", "token"))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := v.Validate(context.Background(), "tok-abc"); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if seenCT != "application/x-www-form-urlencoded" {
		t.Errorf("Content-Type = %q, want form-urlencoded", seenCT)
	}
	if seenForm != "token=tok-abc" {
		t.Errorf("form body = %q, want %q", seenForm, "token=tok-abc")
	}
}

func TestValidate_JSONBody(t *testing.T) {
	var seenBody map[string]any
	var seenCT string
	srv := makeValidatorServer(t, func(w http.ResponseWriter, r *http.Request) {
		seenCT = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&seenBody)
		_, _ = w.Write([]byte(`{"active":true}`))
	})
	defer srv.Close()
	v, err := newExternalValidator(validateOpts(srv.URL, "json_body", "token"))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := v.Validate(context.Background(), "tok-abc"); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if seenCT != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", seenCT)
	}
	if seenBody["token"] != "tok-abc" {
		t.Errorf("json body[token] = %v, want %q", seenBody["token"], "tok-abc")
	}
}

func TestValidate_QueryParam(t *testing.T) {
	var seenQuery string
	srv := makeValidatorServer(t, func(w http.ResponseWriter, r *http.Request) {
		seenQuery = r.URL.Query().Get("access_token")
		_, _ = w.Write([]byte(`{"active":true}`))
	})
	defer srv.Close()
	v, err := newExternalValidator(validateOpts(srv.URL, "query_param", "access_token"))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := v.Validate(context.Background(), "tok-abc"); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if seenQuery != "tok-abc" {
		t.Errorf("query.access_token = %q, want %q", seenQuery, "tok-abc")
	}
}

func TestValidate_ExtraHeaders(t *testing.T) {
	var seenAuth string
	srv := makeValidatorServer(t, func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"active":true}`))
	})
	defer srv.Close()
	opts := validateOpts(srv.URL, "form_field", "token")
	opts.ExtraHeaders = map[string]string{"Authorization": "Basic abc=="}
	v, _ := newExternalValidator(opts)
	if err := v.Validate(context.Background(), "tok"); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if seenAuth != "Basic abc==" {
		t.Errorf("extraHeader Authorization = %q, want %q", seenAuth, "Basic abc==")
	}
}

// --- positive vs negative answers -------------------------------------------

func TestValidate_RejectsWhenActiveFalse(t *testing.T) {
	srv := makeValidatorServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"active":false}`))
	})
	defer srv.Close()
	v, _ := newExternalValidator(validateOpts(srv.URL, "bearer_header", ""))
	err := v.Validate(context.Background(), "tok")
	if err == nil {
		t.Fatal("expected rejection when active=false")
	}
}

func TestValidate_RejectsWhenPathMissing(t *testing.T) {
	srv := makeValidatorServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"other":true}`))
	})
	defer srv.Close()
	v, _ := newExternalValidator(validateOpts(srv.URL, "bearer_header", ""))
	err := v.Validate(context.Background(), "tok")
	if err == nil {
		t.Fatal("expected rejection when configured path is absent")
	}
}

func TestValidate_AcceptsCustomExpectedValue(t *testing.T) {
	srv := makeValidatorServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"active"}`))
	})
	defer srv.Close()
	v, _ := newExternalValidator(ExternalValidatorOptions{
		URL:            srv.URL,
		TokenPlacement: "bearer_header",
		Success:        ExternalValidatorSuccess{JSONPath: "$.status", ExpectedValue: "active"},
	})
	if err := v.Validate(context.Background(), "tok"); err != nil {
		t.Fatalf("expected pass with string match, got %v", err)
	}
}

// --- fail mode --------------------------------------------------------------

func TestValidate_FailClosed_Rejects_OnNon2xx(t *testing.T) {
	srv := makeValidatorServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	defer srv.Close()
	v, _ := newExternalValidator(validateOpts(srv.URL, "bearer_header", ""))
	err := v.Validate(context.Background(), "tok")
	if err == nil {
		t.Fatal("fail_closed: expected rejection on 500")
	}
}

func TestValidate_FailOpen_AcceptsOnNon2xx(t *testing.T) {
	srv := makeValidatorServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})
	defer srv.Close()
	opts := validateOpts(srv.URL, "bearer_header", "")
	opts.FailMode = "open"
	v, _ := newExternalValidator(opts)
	if err := v.Validate(context.Background(), "tok"); err != nil {
		t.Fatalf("fail_open should accept on 502, got %v", err)
	}
}

func TestValidate_FailOpen_StillRejectsExplicitInactive(t *testing.T) {
	// fail_open only suppresses transport-level errors. A successful
	// response saying "inactive" still rejects.
	srv := makeValidatorServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"active":false}`))
	})
	defer srv.Close()
	opts := validateOpts(srv.URL, "bearer_header", "")
	opts.FailMode = "open"
	v, _ := newExternalValidator(opts)
	if err := v.Validate(context.Background(), "tok"); err == nil {
		t.Fatal("fail_open must still reject an explicit inactive answer")
	}
}

func TestValidate_FailClosed_RejectsOnTimeout(t *testing.T) {
	srv := makeValidatorServer(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_, _ = w.Write([]byte(`{"active":true}`))
	})
	defer srv.Close()
	opts := validateOpts(srv.URL, "bearer_header", "")
	opts.TimeoutMS = 50
	v, _ := newExternalValidator(opts)
	err := v.Validate(context.Background(), "tok")
	if err == nil {
		t.Fatal("expected timeout rejection")
	}
}

func TestValidate_FailOpen_AcceptsOnTimeout(t *testing.T) {
	srv := makeValidatorServer(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_, _ = w.Write([]byte(`{"active":true}`))
	})
	defer srv.Close()
	opts := validateOpts(srv.URL, "bearer_header", "")
	opts.TimeoutMS = 50
	opts.FailMode = "open"
	v, _ := newExternalValidator(opts)
	if err := v.Validate(context.Background(), "tok"); err != nil {
		t.Fatalf("fail_open should accept on timeout, got %v", err)
	}
}

// --- cache ------------------------------------------------------------------

func TestNewExternalValidator_RejectsNegativeCacheTTL(t *testing.T) {
	_, err := newExternalValidator(ExternalValidatorOptions{
		URL:             "https://idp/introspect",
		TokenPlacement:  "bearer_header",
		Success:         ExternalValidatorSuccess{JSONPath: "$.active", ExpectedValue: true},
		CacheTTLSeconds: -1,
	})
	if err == nil || !strings.Contains(err.Error(), "cacheTtlSeconds") {
		t.Fatalf("expected error for negative TTL, got %v", err)
	}
}

func TestValidate_NoCache_WhenTTLZero(t *testing.T) {
	hits := 0
	srv := makeValidatorServer(t, func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write([]byte(`{"active":true}`))
	})
	defer srv.Close()
	v, err := newExternalValidator(validateOpts(srv.URL, "bearer_header", ""))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := v.Validate(context.Background(), "same-token"); err != nil {
			t.Fatalf("Validate(%d): %v", i, err)
		}
	}
	if hits != 3 {
		t.Errorf("hits = %d, want 3 (cache disabled → every call must reach IdP)", hits)
	}
}

func TestValidate_Cache_PositiveAnswerMemoized(t *testing.T) {
	hits := 0
	srv := makeValidatorServer(t, func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write([]byte(`{"active":true}`))
	})
	defer srv.Close()
	opts := validateOpts(srv.URL, "bearer_header", "")
	opts.CacheTTLSeconds = 60
	v, err := newExternalValidator(opts)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	for i := 0; i < 5; i++ {
		if err := v.Validate(context.Background(), "same-token"); err != nil {
			t.Fatalf("Validate(%d): %v", i, err)
		}
	}
	if hits != 1 {
		t.Errorf("hits = %d, want 1 (4 subsequent calls should hit the cache)", hits)
	}
}

func TestValidate_Cache_KeyedPerToken(t *testing.T) {
	hits := 0
	srv := makeValidatorServer(t, func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write([]byte(`{"active":true}`))
	})
	defer srv.Close()
	opts := validateOpts(srv.URL, "bearer_header", "")
	opts.CacheTTLSeconds = 60
	v, _ := newExternalValidator(opts)
	_ = v.Validate(context.Background(), "token-a")
	_ = v.Validate(context.Background(), "token-b")
	_ = v.Validate(context.Background(), "token-a")
	_ = v.Validate(context.Background(), "token-b")
	if hits != 2 {
		t.Errorf("hits = %d, want 2 (token-a + token-b each hit once, then cache)", hits)
	}
}

func TestValidate_Cache_NegativeAnswerNotMemoized(t *testing.T) {
	hits := 0
	srv := makeValidatorServer(t, func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write([]byte(`{"active":false}`))
	})
	defer srv.Close()
	opts := validateOpts(srv.URL, "bearer_header", "")
	opts.CacheTTLSeconds = 60
	v, _ := newExternalValidator(opts)
	// Each call should reach the IdP because we never cache rejections —
	// revocation is honored immediately even with cache enabled.
	for i := 0; i < 3; i++ {
		if err := v.Validate(context.Background(), "revoked-token"); err == nil {
			t.Fatalf("Validate(%d): expected rejection", i)
		}
	}
	if hits != 3 {
		t.Errorf("hits = %d, want 3 (negative answers must not be cached)", hits)
	}
}

func TestValidate_Cache_TransportErrorNotMemoized(t *testing.T) {
	hits := 0
	srv := makeValidatorServer(t, func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusInternalServerError)
	})
	defer srv.Close()
	opts := validateOpts(srv.URL, "bearer_header", "")
	opts.CacheTTLSeconds = 60
	v, _ := newExternalValidator(opts)
	for i := 0; i < 3; i++ {
		if err := v.Validate(context.Background(), "tok"); err == nil {
			t.Fatalf("Validate(%d): expected error", i)
		}
	}
	if hits != 3 {
		t.Errorf("hits = %d, want 3 (transport errors must not be cached)", hits)
	}
}

func TestValidate_Cache_ExpiresAfterTTL(t *testing.T) {
	hits := 0
	srv := makeValidatorServer(t, func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write([]byte(`{"active":true}`))
	})
	defer srv.Close()
	opts := validateOpts(srv.URL, "bearer_header", "")
	opts.CacheTTLSeconds = 1
	v, _ := newExternalValidator(opts)
	_ = v.Validate(context.Background(), "tok")
	_ = v.Validate(context.Background(), "tok")
	if hits != 1 {
		t.Fatalf("hits = %d, want 1 (second call within TTL must be cached)", hits)
	}
	time.Sleep(1100 * time.Millisecond)
	_ = v.Validate(context.Background(), "tok")
	if hits != 2 {
		t.Errorf("hits = %d, want 2 (third call after TTL must reach IdP again)", hits)
	}
}

func TestValidate_ContextCancellationRespected(t *testing.T) {
	srv := makeValidatorServer(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		_, _ = w.Write([]byte(`{"active":true}`))
	})
	defer srv.Close()
	v, _ := newExternalValidator(validateOpts(srv.URL, "bearer_header", ""))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := v.Validate(ctx, "tok")
	if err == nil || !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("expected context.Canceled error, got %v", err)
	}
}
