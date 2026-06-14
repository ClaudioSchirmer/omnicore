package auth

import (
	"net/http"
	"strings"
	"testing"
)

// --- attach helpers ------------------------------------------------------

func TestParseAttachKind(t *testing.T) {
	cases := []struct {
		in   string
		want AttachKind
		err  bool
	}{
		{"", AttachHeader, false},
		{"header", AttachHeader, false},
		{"HEADER", AttachHeader, false},
		{"query", AttachQuery, false},
		{"cookie", AttachCookie, false},
		{"basic", AttachUnknown, true},
	}
	for _, tc := range cases {
		got, err := ParseAttachKind(tc.in)
		if tc.err {
			if err == nil {
				t.Errorf("%q should error", tc.in)
			}
			continue
		}
		if got != tc.want {
			t.Errorf("%q = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestRenderValue(t *testing.T) {
	if got := RenderValue("", "tok"); got != "tok" {
		t.Errorf("empty format = %q", got)
	}
	if got := RenderValue("Bearer {token}", "abc"); got != "Bearer abc" {
		t.Errorf("substitution = %q", got)
	}
}

func TestAttach_Header(t *testing.T) {
	req, _ := http.NewRequest("GET", "https://x.example.com/u", nil)
	Attach(req, AttachConfig{Kind: AttachHeader, Name: "X-K"}, "v")
	if req.Header.Get("X-K") != "v" {
		t.Errorf("header not set")
	}
}

func TestAttach_Query(t *testing.T) {
	req, _ := http.NewRequest("GET", "https://x.example.com/u", nil)
	Attach(req, AttachConfig{Kind: AttachQuery, Name: "k"}, "v")
	if req.URL.Query().Get("k") != "v" {
		t.Errorf("query not set")
	}
}

func TestAttach_Cookie(t *testing.T) {
	req, _ := http.NewRequest("GET", "https://x.example.com/u", nil)
	Attach(req, AttachConfig{Kind: AttachCookie, Name: "s"}, "v")
	c, err := req.Cookie("s")
	if err != nil {
		t.Fatalf("cookie: %v", err)
	}
	if c.Value != "v" {
		t.Errorf("cookie value = %q", c.Value)
	}
}

// --- providers -----------------------------------------------------------

func TestNoneProvider_Noop(t *testing.T) {
	p := NewNoneProvider("none")
	req, _ := http.NewRequest("GET", "https://x.example.com/u", nil)
	before := req.Header.Get("Authorization")
	if err := p.Apply(req); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if req.Header.Get("Authorization") != before {
		t.Error("noneProvider should not mutate request")
	}
}

func TestHeaderStaticProvider(t *testing.T) {
	p, err := NewHeaderStaticProvider("api", AttachConfig{Kind: AttachHeader, Name: "X-API-Key", Value: "secret"})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	req, _ := http.NewRequest("GET", "https://x.example.com/u", nil)
	_ = p.Apply(req)
	if req.Header.Get("X-API-Key") != "secret" {
		t.Errorf("header = %q", req.Header.Get("X-API-Key"))
	}
}

func TestHeaderStaticProvider_RequiresName(t *testing.T) {
	if _, err := NewHeaderStaticProvider("api", AttachConfig{Kind: AttachHeader, Value: "v"}); err == nil {
		t.Error("expected error for missing name")
	}
}

func TestHeaderStaticProvider_RequiresValue(t *testing.T) {
	if _, err := NewHeaderStaticProvider("api", AttachConfig{Kind: AttachHeader, Name: "X-K"}); err == nil {
		t.Error("expected error for missing value")
	}
}

func TestBearerStaticProvider_DefaultAttach(t *testing.T) {
	p, err := NewBearerStaticProvider("b", "tok", AttachConfig{})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	req, _ := http.NewRequest("GET", "https://x.example.com/u", nil)
	_ = p.Apply(req)
	if got := req.Header.Get("Authorization"); got != "Bearer tok" {
		t.Errorf("default attach = %q", got)
	}
}

func TestBearerStaticProvider_CustomFormat(t *testing.T) {
	p, err := NewBearerStaticProvider("b", "tok", AttachConfig{Kind: AttachHeader, Name: "X-Token", Format: "Token {token}"})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	req, _ := http.NewRequest("GET", "https://x.example.com/u", nil)
	_ = p.Apply(req)
	if got := req.Header.Get("X-Token"); got != "Token tok" {
		t.Errorf("custom = %q", got)
	}
}

func TestBearerStaticProvider_RequiresToken(t *testing.T) {
	if _, err := NewBearerStaticProvider("b", "", AttachConfig{}); err == nil {
		t.Error("expected error for missing token")
	}
}

func TestBasicProvider_Encoded(t *testing.T) {
	p, err := NewBasicProvider("b", "alice", "wonder", AttachConfig{})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	req, _ := http.NewRequest("GET", "https://x.example.com/u", nil)
	_ = p.Apply(req)
	got := req.Header.Get("Authorization")
	if !strings.HasPrefix(got, "Basic ") {
		t.Errorf("expected Basic prefix; got %q", got)
	}
	// base64("alice:wonder") = YWxpY2U6d29uZGVy
	if got != "Basic YWxpY2U6d29uZGVy" {
		t.Errorf("encoding = %q, want Basic YWxpY2U6d29uZGVy", got)
	}
}

func TestBasicProvider_RequiresCreds(t *testing.T) {
	if _, err := NewBasicProvider("b", "", "p", AttachConfig{}); err == nil {
		t.Error("expected error for missing username")
	}
	if _, err := NewBasicProvider("b", "u", "", AttachConfig{}); err == nil {
		t.Error("expected error for missing password")
	}
}

// --- Registry ------------------------------------------------------------

func TestRegistry_LookupKnown(t *testing.T) {
	r := NewRegistry()
	r.Register("a", NewNoneProvider("a"))
	p, err := r.Lookup("a")
	if err != nil || p == nil {
		t.Errorf("got (%v, %v)", p, err)
	}
}

func TestRegistry_LookupUnknown(t *testing.T) {
	r := NewRegistry()
	if _, err := r.Lookup("nope"); err == nil {
		t.Error("expected error for unknown lookup")
	}
}

func TestRegistry_NilLookup(t *testing.T) {
	var r *Registry
	if _, err := r.Lookup("x"); err == nil {
		t.Error("expected error from nil registry")
	}
}
