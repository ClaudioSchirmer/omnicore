package httpclient

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ClaudioSchirmer/omnicore/infra/httpclient/auth"
)

// --- retry_config.go: parseBackoffName ----------------------------------

func TestParseBackoffName(t *testing.T) {
	cases := []struct {
		in   string
		want backoffStrategy
	}{
		{"constant", backoffConstant},
		{"linear", backoffLinear},
		{"exponential", backoffExponential},
		{"exponential-jitter", backoffExponentialJitter},
		{"", backoffExponentialJitter},
		{" EXPONENTIAL ", backoffExponential},
		{"unknown", backoffExponentialJitter},
	}
	for _, tc := range cases {
		if got := parseBackoffName(tc.in); got != tc.want {
			t.Errorf("parseBackoffName(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// --- signing_config.go: resolveTimestampFormat --------------------------

func TestResolveTimestampFormat(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", timestampFormatRFC1123},
		{"rfc1123", timestampFormatRFC1123},
		{"iso8601", timestampFormatISO8601},
		{"unix-seconds", timestampFormatUnixSecond},
		{" ISO8601 ", timestampFormatISO8601},
		{"weird", timestampFormatRFC1123},
	}
	for _, tc := range cases {
		if got := resolveTimestampFormat(tc.in); got != tc.want {
			t.Errorf("resolveTimestampFormat(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// --- auth_config.go: validateTokenCacheConfig ---------------------------

func TestValidateTokenCacheConfig(t *testing.T) {
	t.Run("nil → required", func(t *testing.T) {
		errs := validateTokenCacheConfig("p.tokenCache", nil)
		if len(errs) != 1 || !strings.Contains(errs[0], "required") {
			t.Fatalf("nil cfg: got %v", errs)
		}
	})
	t.Run("empty source", func(t *testing.T) {
		errs := validateTokenCacheConfig("p.tokenCache", &TokenCacheConfig{})
		if !containsBit(errs, "source") {
			t.Fatalf("expected source error; got %v", errs)
		}
	})
	t.Run("unknown source", func(t *testing.T) {
		errs := validateTokenCacheConfig("p.tokenCache", &TokenCacheConfig{Source: "wat"})
		if !containsBit(errs, "source") {
			t.Fatalf("expected source error; got %v", errs)
		}
	})
	t.Run("negative skew", func(t *testing.T) {
		errs := validateTokenCacheConfig("p.tokenCache", &TokenCacheConfig{Source: "ttl", TTL: Duration(time.Second), Skew: Duration(-time.Second)})
		if !containsBit(errs, "skew") {
			t.Fatalf("expected skew error; got %v", errs)
		}
	})
	t.Run("response-field needs jsonPath", func(t *testing.T) {
		errs := validateTokenCacheConfig("p.tokenCache", &TokenCacheConfig{Source: "response-field"})
		if !containsBit(errs, "jsonPath") {
			t.Fatalf("expected jsonPath error; got %v", errs)
		}
	})
	t.Run("response-field bad unit", func(t *testing.T) {
		errs := validateTokenCacheConfig("p.tokenCache", &TokenCacheConfig{Source: "response-field", JSONPath: "$.x", Unit: "fortnights"})
		if !containsBit(errs, "unit") {
			t.Fatalf("expected unit error; got %v", errs)
		}
	})
	t.Run("response-field good unit", func(t *testing.T) {
		errs := validateTokenCacheConfig("p.tokenCache", &TokenCacheConfig{Source: "response-field", JSONPath: "$.x", Unit: "millis"})
		if len(errs) != 0 {
			t.Fatalf("clean response-field must pass; got %v", errs)
		}
	})
	t.Run("ttl requires positive", func(t *testing.T) {
		errs := validateTokenCacheConfig("p.tokenCache", &TokenCacheConfig{Source: "ttl"})
		if !containsBit(errs, "ttl") {
			t.Fatalf("expected ttl error; got %v", errs)
		}
	})
	t.Run("jwt-exp clean", func(t *testing.T) {
		errs := validateTokenCacheConfig("p.tokenCache", &TokenCacheConfig{Source: "jwt-exp"})
		if len(errs) != 0 {
			t.Fatalf("jwt-exp must pass; got %v", errs)
		}
	})
}

// --- auth_config.go: toRuntimeTokenCacheConfig --------------------------

func TestToRuntimeTokenCacheConfig(t *testing.T) {
	t.Run("jwt-exp default single-flight", func(t *testing.T) {
		out := toRuntimeTokenCacheConfig(&TokenCacheConfig{Source: "jwt-exp"})
		if out.Source != auth.SourceJWTExp {
			t.Errorf("source = %v", out.Source)
		}
		if !out.SingleFlight {
			t.Error("single-flight should default true")
		}
	})
	t.Run("response-field millis", func(t *testing.T) {
		out := toRuntimeTokenCacheConfig(&TokenCacheConfig{Source: "response-field", JSONPath: "$.expires_in", Unit: "millis"})
		if out.Source != auth.SourceResponseField || out.Unit != auth.UnitMillis || out.JSONPath != "$.expires_in" {
			t.Errorf("unexpected runtime cfg: %+v", out)
		}
	})
	t.Run("response-field iso8601", func(t *testing.T) {
		out := toRuntimeTokenCacheConfig(&TokenCacheConfig{Source: "response-field", JSONPath: "$.x", Unit: "iso8601"})
		if out.Unit != auth.UnitISO8601 {
			t.Errorf("unit = %v, want iso8601", out.Unit)
		}
	})
	t.Run("response-field default unit seconds", func(t *testing.T) {
		out := toRuntimeTokenCacheConfig(&TokenCacheConfig{Source: "response-field", JSONPath: "$.x"})
		if out.Unit != auth.UnitSeconds {
			t.Errorf("unit = %v, want seconds", out.Unit)
		}
	})
	t.Run("ttl with explicit single-flight off", func(t *testing.T) {
		off := false
		out := toRuntimeTokenCacheConfig(&TokenCacheConfig{Source: "ttl", TTL: Duration(time.Minute), SingleFlight: &off})
		if out.Source != auth.SourceTTL {
			t.Errorf("source = %v", out.Source)
		}
		if out.SingleFlight {
			t.Error("explicit false should disable single-flight")
		}
		if out.TTL != time.Minute {
			t.Errorf("ttl = %v", out.TTL)
		}
	})
}

// --- error.go: Unwrap + causeIsRetriable --------------------------------

func TestHttpError_Unwrap_NilAndCause(t *testing.T) {
	var nilErr *HttpError
	if nilErr.Unwrap() != nil {
		t.Error("nil receiver should unwrap to nil")
	}
	cause := errors.New("root")
	he := &HttpError{Cause: cause}
	if he.Unwrap() != cause {
		t.Error("Unwrap should expose the cause")
	}
	if (&HttpError{}).Unwrap() != nil {
		t.Error("no cause should unwrap to nil")
	}
}

func TestCauseIsRetriable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"deadline exceeded", context.DeadlineExceeded, true},
		{"url.Error timeout", &url.Error{Err: fakeNetErr{timeout: true}}, true},
		{"net.Error timeout", fakeNetErr{timeout: true}, true},
		{"dns error", &net.DNSError{}, true},
		{"plain", errors.New("boom"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := causeIsRetriable(tc.err); got != tc.want {
				t.Errorf("causeIsRetriable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestIsRetriable_StatusBranches(t *testing.T) {
	if !IsRetriable(&HttpError{Status: 503}) {
		t.Error("503 should be retriable")
	}
	if IsRetriable(&HttpError{Status: 404}) {
		t.Error("404 should not be retriable")
	}
	if !IsRetriable(&HttpError{Status: 0, Cause: &net.DNSError{}}) {
		t.Error("transport DNS error should be retriable")
	}
	if IsRetriable(context.Canceled) {
		t.Error("cancellation never retriable")
	}
	if IsRetriable(nil) {
		t.Error("nil is not retriable")
	}
}
