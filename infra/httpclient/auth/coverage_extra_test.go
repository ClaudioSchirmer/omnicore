package auth

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// (makeJWT is defined in oauth2_test.go within the same package.)

// --- Name() accessors ---------------------------------------------------

func TestProviderNames(t *testing.T) {
	none := NewNoneProvider("none-p")
	if none.Name() != "none-p" {
		t.Errorf("none Name = %q", none.Name())
	}

	header, err := NewHeaderStaticProvider("hdr-p", AttachConfig{Kind: AttachHeader, Name: "X-K", Value: "v"})
	if err != nil {
		t.Fatalf("header ctor: %v", err)
	}
	if header.Name() != "hdr-p" {
		t.Errorf("header Name = %q", header.Name())
	}

	bearer, err := NewBearerStaticProvider("bearer-p", "tok", AttachConfig{})
	if err != nil {
		t.Fatalf("bearer ctor: %v", err)
	}
	if bearer.Name() != "bearer-p" {
		t.Errorf("bearer Name = %q", bearer.Name())
	}

	basic, err := NewBasicProvider("basic-p", "u", "p", AttachConfig{})
	if err != nil {
		t.Fatalf("basic ctor: %v", err)
	}
	if basic.Name() != "basic-p" {
		t.Errorf("basic Name = %q", basic.Name())
	}

	forward := NewForwardBearerProvider("fwd-p", AttachConfig{})
	if forward.Name() != "fwd-p" {
		t.Errorf("forward Name = %q", forward.Name())
	}

	oauth, err := NewOAuth2ClientCredentialsProvider(OAuth2Options{
		Name: "oauth-p", TokenEndpoint: "https://idp.example.com/token",
		ClientID: "id", ClientSecret: "secret",
		Cache: TokenCacheConfig{Source: SourceTTL, TTL: time.Hour},
	})
	if err != nil {
		t.Fatalf("oauth ctor: %v", err)
	}
	if oauth.Name() != "oauth-p" {
		t.Errorf("oauth Name = %q", oauth.Name())
	}

	creds, err := NewCredentialsExchangeProvider(CredentialsExchangeOptions{
		Name: "creds-p", TokenEndpoint: "https://idp.example.com/token",
		RequestCodec: "json", RequestFields: map[string]string{"a": "1"},
		ResponseTokenPath: "$.access_token",
		Cache:             TokenCacheConfig{Source: SourceTTL, TTL: time.Hour},
	})
	if err != nil {
		t.Fatalf("creds ctor: %v", err)
	}
	if creds.Name() != "creds-p" {
		t.Errorf("creds Name = %q", creds.Name())
	}
}

// --- Registry.Len -------------------------------------------------------

func TestRegistry_Len(t *testing.T) {
	var nilReg *Registry
	if nilReg.Len() != 0 {
		t.Errorf("nil registry Len = %d, want 0", nilReg.Len())
	}
	r := NewRegistry()
	if r.Len() != 0 {
		t.Errorf("empty registry Len = %d, want 0", r.Len())
	}
	r.Register("a", NewNoneProvider("a"))
	r.Register("b", NewNoneProvider("b"))
	if r.Len() != 2 {
		t.Errorf("registry Len = %d, want 2", r.Len())
	}
}

// --- decodeJWTExp -------------------------------------------------------

func TestDecodeJWTExp_RemainingBranches(t *testing.T) {
	t.Run("bad base64 payload", func(t *testing.T) {
		if _, err := decodeJWTExp("h.!!!notbase64!!!.s"); err == nil {
			t.Error("expected base64 decode error")
		}
	})

	t.Run("payload not json", func(t *testing.T) {
		bad := base64.RawURLEncoding.EncodeToString([]byte("not-json"))
		if _, err := decodeJWTExp("h." + bad + ".s"); err == nil {
			t.Error("expected json parse error")
		}
	})

	t.Run("exp wrong type", func(t *testing.T) {
		tok := makeJWT(map[string]any{"exp": "not-a-number"})
		if _, err := decodeJWTExp(tok); err == nil {
			t.Error("expected error for string exp")
		}
	})

	t.Run("standard base64 fallback", func(t *testing.T) {
		// Payload base64-encoded with the standard (padded) alphabet rather
		// than raw URL-safe — decodeJWTExp falls back to URLEncoding.
		payload := []byte(`{"exp":1700000000}`)
		std := base64.URLEncoding.EncodeToString(payload) // padded
		if !strings.Contains(std, "=") {
			t.Skip("payload did not require padding; fallback path not exercised")
		}
		exp, err := decodeJWTExp("h." + std + ".s")
		if err != nil {
			t.Fatalf("fallback decode: %v", err)
		}
		if exp.Unix() != 1700000000 {
			t.Errorf("exp = %d", exp.Unix())
		}
	})
}

// --- numberOf -----------------------------------------------------------

func TestNumberOf(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want int64
		err  bool
	}{
		{"float64", float64(42), 42, false},
		{"int64", int64(7), 7, false},
		{"int", 9, 9, false},
		{"json.Number", json.Number("123"), 123, false},
		{"string", "55", 55, false},
		{"bad string", "abc", 0, true},
		{"unsupported", []int{1}, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := numberOf(tc.in)
			if tc.err {
				if err == nil {
					t.Errorf("expected error for %v", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("numberOf(%v): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("numberOf(%v) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// --- parseResponseFieldExpiry -------------------------------------------

func TestParseResponseFieldExpiry(t *testing.T) {
	now := time.Now()

	t.Run("seconds", func(t *testing.T) {
		exp, err := parseResponseFieldExpiry(float64(3600), UnitSeconds)
		if err != nil {
			t.Fatalf("seconds: %v", err)
		}
		if d := exp.Sub(now); d < 59*time.Minute || d > 61*time.Minute {
			t.Errorf("seconds delta = %v", d)
		}
	})

	t.Run("unknown unit defaults to seconds", func(t *testing.T) {
		exp, err := parseResponseFieldExpiry(float64(60), UnitUnknown)
		if err != nil {
			t.Fatalf("unknown: %v", err)
		}
		if exp.Before(now) {
			t.Error("expiry should be in the future")
		}
	})

	t.Run("seconds bad value", func(t *testing.T) {
		if _, err := parseResponseFieldExpiry("nope", UnitSeconds); err == nil {
			t.Error("expected error")
		}
	})

	t.Run("millis", func(t *testing.T) {
		exp, err := parseResponseFieldExpiry(float64(5000), UnitMillis)
		if err != nil {
			t.Fatalf("millis: %v", err)
		}
		if d := exp.Sub(now); d < 4*time.Second || d > 6*time.Second {
			t.Errorf("millis delta = %v", d)
		}
	})

	t.Run("millis bad value", func(t *testing.T) {
		if _, err := parseResponseFieldExpiry(struct{}{}, UnitMillis); err == nil {
			t.Error("expected error")
		}
	})

	t.Run("iso8601", func(t *testing.T) {
		want := "2030-01-02T15:04:05Z"
		exp, err := parseResponseFieldExpiry(want, UnitISO8601)
		if err != nil {
			t.Fatalf("iso8601: %v", err)
		}
		if exp.UTC().Format(time.RFC3339) != want {
			t.Errorf("iso8601 = %v", exp)
		}
	})

	t.Run("iso8601 non-string", func(t *testing.T) {
		if _, err := parseResponseFieldExpiry(123, UnitISO8601); err == nil {
			t.Error("expected error for non-string")
		}
	})

	t.Run("iso8601 bad format", func(t *testing.T) {
		if _, err := parseResponseFieldExpiry("not-a-time", UnitISO8601); err == nil {
			t.Error("expected error for malformed timestamp")
		}
	})

	t.Run("unsupported unit", func(t *testing.T) {
		if _, err := parseResponseFieldExpiry("x", ResponseFieldUnit(99)); err == nil {
			t.Error("expected error for unsupported unit")
		}
	})
}

// --- truncate -----------------------------------------------------------

func TestTruncate(t *testing.T) {
	if got := truncate("short", 10); got != "short" {
		t.Errorf("under-limit = %q", got)
	}
	if got := truncate("exactly10!", 10); got != "exactly10!" {
		t.Errorf("at-limit = %q", got)
	}
	long := strings.Repeat("a", 20)
	got := truncate(long, 5)
	if got != "aaaaa…" {
		t.Errorf("over-limit = %q", got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("expected ellipsis suffix; got %q", got)
	}
}

// --- ctxValueToString ---------------------------------------------------

type stringerVal struct{ s string }

func (v stringerVal) String() string { return v.s }

func TestCtxValueToString(t *testing.T) {
	str := "hello"
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"string", "abc", "abc"},
		{"ptr string", &str, "hello"},
		{"nil ptr string", (*string)(nil), ""},
		{"stringer", stringerVal{"sv"}, "sv"},
		{"bool true", true, "true"},
		{"bool false", false, "false"},
		{"int", 42, "42"},
		{"int64", int64(64), "64"},
		{"uint", uint(7), "7"},
		{"float64", float64(3), "3"},
		{"nil", nil, ""},
		{"fallback", []int{1, 2}, "[1 2]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ctxValueToString(tc.in)
			if err != nil {
				t.Fatalf("ctxValueToString(%v): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("ctxValueToString(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// --- CredentialsExchangeProvider.computeExpiry --------------------------

func newCredsForExpiry(t *testing.T, cache TokenCacheConfig) *CredentialsExchangeProvider {
	t.Helper()
	p, err := NewCredentialsExchangeProvider(CredentialsExchangeOptions{
		Name: "exp", TokenEndpoint: "https://idp.example.com/token",
		RequestCodec: "json", RequestFields: map[string]string{"a": "1"},
		ResponseTokenPath: "$.access_token",
		Cache:             cache,
	})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	return p
}

func TestCredentialsExchange_ComputeExpiry(t *testing.T) {
	t.Run("jwt-exp", func(t *testing.T) {
		p := newCredsForExpiry(t, TokenCacheConfig{Source: SourceJWTExp})
		tok := makeJWT(map[string]any{"exp": 1900000000})
		exp, err := p.computeExpiry(tok, nil)
		if err != nil {
			t.Fatalf("computeExpiry: %v", err)
		}
		if exp.Unix() != 1900000000 {
			t.Errorf("exp = %d", exp.Unix())
		}
	})

	t.Run("jwt-exp bad token", func(t *testing.T) {
		p := newCredsForExpiry(t, TokenCacheConfig{Source: SourceJWTExp})
		if _, err := p.computeExpiry("garbage", nil); err == nil {
			t.Error("expected error for non-JWT token")
		}
	})

	t.Run("response-field", func(t *testing.T) {
		p := newCredsForExpiry(t, TokenCacheConfig{
			Source: SourceResponseField, JSONPath: "$.expires_in", Unit: UnitSeconds,
		})
		payload := map[string]any{"expires_in": float64(120)}
		exp, err := p.computeExpiry("tok", payload)
		if err != nil {
			t.Fatalf("computeExpiry: %v", err)
		}
		if exp.Before(time.Now()) {
			t.Error("expiry should be in the future")
		}
	})

	t.Run("response-field bad path", func(t *testing.T) {
		p := newCredsForExpiry(t, TokenCacheConfig{
			Source: SourceResponseField, JSONPath: "$.missing", Unit: UnitSeconds,
		})
		if _, err := p.computeExpiry("tok", map[string]any{"other": 1}); err == nil {
			t.Error("expected error for missing JSONPath field")
		}
	})

	t.Run("ttl", func(t *testing.T) {
		p := newCredsForExpiry(t, TokenCacheConfig{Source: SourceTTL, TTL: time.Hour})
		exp, err := p.computeExpiry("tok", nil)
		if err != nil {
			t.Fatalf("computeExpiry: %v", err)
		}
		if exp.Before(time.Now()) {
			t.Error("ttl expiry should be in the future")
		}
	})

	t.Run("no source configured", func(t *testing.T) {
		p := newCredsForExpiry(t, TokenCacheConfig{Source: SourceUnknown})
		if _, err := p.computeExpiry("tok", nil); err == nil {
			t.Error("expected error for unconfigured source")
		}
	})
}
