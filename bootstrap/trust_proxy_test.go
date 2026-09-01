package bootstrap

import (
	"context"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
)

// app.Test dials from 0.0.0.0, so that is "the proxy" in the cases below:
// trusting it is what makes the forwarded header readable at all.
const testPeer = "0.0.0.0"

// echoIP mounts a route returning c.IP(), the accessor the access log, the
// server span and every handler read the request origin from.
func echoIP(app *fiber.App, _ Deps) error {
	app.Get("/whoami", func(c fiber.Ctx) error { return c.SendString(c.IP()) })
	return nil
}

func TestBuildApp_TrustProxyWiredIntoFiber(t *testing.T) {
	d := silentDeps()
	d.Config.HTTP.TrustProxy = &TrustProxyConfig{
		Enabled:    true,
		Proxies:    []string{"10.0.0.7", "192.168.0.0/16"},
		Private:    true,
		Loopback:   true,
		LinkLocal:  true,
		UnixSocket: true,
		Header:     "X-Real-IP",
	}
	app, err := buildApp(context.Background(), d, Wiring{})
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}
	cfg := app.Config()
	if !cfg.TrustProxy {
		t.Fatal("fiber TrustProxy = false, want true")
	}
	// Fiber reads the framework's private header — the framework, not Fiber,
	// decides which entry of the operator's header is believed.
	if cfg.ProxyHeader != canonicalClientIPHeader {
		t.Errorf("fiber ProxyHeader = %q, want %q", cfg.ProxyHeader, canonicalClientIPHeader)
	}
	if got := d.Config.HTTP.TrustProxy.headerName(); got != "X-Real-IP" {
		t.Errorf("headerName() = %q, want the declared X-Real-IP", got)
	}
	tp := cfg.TrustProxyConfig
	if len(tp.Proxies) != 2 || tp.Proxies[0] != "10.0.0.7" {
		t.Errorf("Proxies = %v, want the two declared entries", tp.Proxies)
	}
	if !tp.Private || !tp.Loopback || !tp.LinkLocal || !tp.UnixSocket {
		t.Errorf("range flags = private %v / loopback %v / linkLocal %v / unixSocket %v, want all true",
			tp.Private, tp.Loopback, tp.LinkLocal, tp.UnixSocket)
	}
}

func TestBuildApp_TrustProxyHeaderDefault(t *testing.T) {
	d := silentDeps()
	d.Config.HTTP.TrustProxy = &TrustProxyConfig{Enabled: true, Private: true}
	if got := d.Config.HTTP.TrustProxy.headerName(); got != fiber.HeaderXForwardedFor {
		t.Errorf("headerName() = %q, want the X-Forwarded-For default", got)
	}
}

func TestBuildApp_TrustProxyAbsentKeepsSocketOrigin(t *testing.T) {
	// The default has to stay spoof-proof: no trust, no proxy header, so
	// c.IP() is the peer whatever headers arrive.
	app, err := buildApp(context.Background(), silentDeps(), Wiring{})
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}
	cfg := app.Config()
	if cfg.TrustProxy {
		t.Error("TrustProxy = true with no http.trustProxy block, want false")
	}
	if cfg.ProxyHeader != "" {
		t.Errorf("ProxyHeader = %q with no block, want empty", cfg.ProxyHeader)
	}
}

func TestBuildApp_TrustProxyResolvesTheClientAddress(t *testing.T) {
	cases := []struct {
		name  string
		block *TrustProxyConfig
		xff   string
		want  string
	}{
		{
			name:  "absent — header ignored",
			block: nil,
			xff:   "198.51.100.23",
			want:  testPeer,
		},
		{
			name:  "trusted peer — client resolved from the header",
			block: &TrustProxyConfig{Enabled: true, Proxies: []string{testPeer}},
			xff:   "198.51.100.23",
			want:  "198.51.100.23",
		},
		{
			name:  "untrusted peer — header ignored, never widens trust",
			block: &TrustProxyConfig{Enabled: true, Proxies: []string{"203.0.113.9"}},
			xff:   "198.51.100.23",
			want:  testPeer,
		},
		{
			// An edge running nginx's default proxy_add_x_forwarded_for
			// APPENDS, so the leftmost entry is whatever the caller typed.
			// Walking from the right past the trusted hops lands on the real
			// one and leaves the forgery behind.
			name:  "appending edge — the caller's forged entry is ignored",
			block: &TrustProxyConfig{Enabled: true, Proxies: []string{testPeer}, Private: true},
			xff:   "1.2.3.4, 198.51.100.23, 10.0.0.4",
			want:  "198.51.100.23",
		},
		{
			name:  "a single hop is still resolved",
			block: &TrustProxyConfig{Enabled: true, Proxies: []string{testPeer}, Private: true},
			xff:   "203.0.113.7, 10.0.0.4",
			want:  "203.0.113.7",
		},
		{
			name:  "whole chain trusted — falls back to the peer",
			block: &TrustProxyConfig{Enabled: true, Proxies: []string{testPeer}, Private: true},
			xff:   "10.0.0.4, 192.168.1.9",
			want:  testPeer,
		},
		{
			name:  "malformed entry breaks the chain of custody",
			block: &TrustProxyConfig{Enabled: true, Proxies: []string{testPeer}, Private: true},
			xff:   "198.51.100.23, garbage, 10.0.0.4",
			want:  testPeer,
		},
		{
			name:  "IPv6 with port, bracketed",
			block: &TrustProxyConfig{Enabled: true, Proxies: []string{testPeer}, Private: true},
			xff:   "[2001:db8::1]:4711, 10.0.0.4",
			want:  "2001:db8::1",
		},
		{
			name:  "empty header — peer",
			block: &TrustProxyConfig{Enabled: true, Proxies: []string{testPeer}},
			xff:   "",
			want:  testPeer,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := silentDeps()
			d.Config.HTTP.TrustProxy = tc.block
			app, err := buildApp(context.Background(), d, Wiring{BeforeServe: echoIP})
			if err != nil {
				t.Fatalf("buildApp: %v", err)
			}
			req := httptest.NewRequest("GET", "/whoami", nil)
			if tc.xff != "" {
				req.Header.Set(fiber.HeaderXForwardedFor, tc.xff)
			}
			resp, err := app.Test(req)
			if err != nil {
				t.Fatalf("Test: %v", err)
			}
			body, _ := io.ReadAll(resp.Body)
			got := strings.TrimSpace(string(body))
			if got != tc.want {
				t.Fatalf("c.IP() = %q, want %q", got, tc.want)
			}
			if net.ParseIP(got) == nil {
				t.Fatalf("c.IP() = %q, want a single parseable address", got)
			}
		})
	}
}

// A caller must never be able to hand the framework its own answer by writing
// the private header the resolution deposits into.
func TestBuildApp_TrustProxyStripsInboundCanonicalHeader(t *testing.T) {
	d := silentDeps()
	d.Config.HTTP.TrustProxy = &TrustProxyConfig{Enabled: true, Proxies: []string{testPeer}}
	app, err := buildApp(context.Background(), d, Wiring{BeforeServe: echoIP})
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}
	req := httptest.NewRequest("GET", "/whoami", nil)
	req.Header.Set(canonicalClientIPHeader, "9.9.9.9")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	if got := strings.TrimSpace(string(body)); got == "9.9.9.9" {
		t.Fatalf("c.IP() = %q — an inbound %s was honored", got, canonicalClientIPHeader)
	}
}

// The operator's header choice is the one the resolution reads — and only it.
func TestBuildApp_TrustProxyHonorsCustomHeader(t *testing.T) {
	d := silentDeps()
	d.Config.HTTP.TrustProxy = &TrustProxyConfig{
		Enabled: true, Proxies: []string{testPeer}, Header: "CF-Connecting-IP",
	}
	app, err := buildApp(context.Background(), d, Wiring{BeforeServe: echoIP})
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}
	req := httptest.NewRequest("GET", "/whoami", nil)
	req.Header.Set("CF-Connecting-IP", "198.51.100.23")
	req.Header.Set(fiber.HeaderXForwardedFor, "1.2.3.4") // the header NOT declared
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	if got := strings.TrimSpace(string(body)); got != "198.51.100.23" {
		t.Fatalf("c.IP() = %q, want the declared header's value", got)
	}
}

// EnableIPValidation stays on so c.IPs() — which always reads the raw
// X-Forwarded-For — drops malformed entries instead of returning them.
func TestBuildApp_TrustProxyEnablesIPValidation(t *testing.T) {
	d := silentDeps()
	d.Config.HTTP.TrustProxy = &TrustProxyConfig{Enabled: true, Proxies: []string{testPeer}}
	app, err := buildApp(context.Background(), d, Wiring{})
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}
	if !app.Config().EnableIPValidation {
		t.Fatal("EnableIPValidation = false")
	}
}

func TestResolveClientIP(t *testing.T) {
	set := (&TrustProxyConfig{
		Proxies:   []string{"203.0.113.9", "198.18.0.0/15"},
		Private:   true,
		Loopback:  true,
		LinkLocal: true,
	}).compile()
	cases := []struct{ chain, want string }{
		{"", ""},
		{"1.2.3.4", "1.2.3.4"},
		{"  1.2.3.4  ", "1.2.3.4"},
		{"1.2.3.4, 10.0.0.1", "1.2.3.4"},
		{"1.2.3.4, 203.0.113.9", "1.2.3.4"},       // named proxy skipped
		{"1.2.3.4, 198.19.7.7", "1.2.3.4"},        // CIDR skipped
		{"1.2.3.4, 127.0.0.1", "1.2.3.4"},         // loopback skipped
		{"1.2.3.4, 169.254.1.1", "1.2.3.4"},       // link-local skipped
		{"1.2.3.4, fc00::1", "1.2.3.4"},           // IPv6 private skipped
		{"9.9.9.9, 1.2.3.4, 10.0.0.1", "1.2.3.4"}, // rightmost untrusted wins
		{"10.0.0.1, 192.168.0.1", ""},             // all trusted
		{"1.2.3.4, nonsense, 10.0.0.1", ""},       // custody broken
		{"1.2.3.4, 10.0.0.1, nonsense", ""},       // custody broken at the right edge
	}
	for _, tc := range cases {
		if got := resolveClientIP(tc.chain, set); got != tc.want {
			t.Errorf("resolveClientIP(%q) = %q, want %q", tc.chain, got, tc.want)
		}
	}
}

func TestConfigValidate_TrustProxyRules(t *testing.T) {
	os.Unsetenv("DB")
	os.Unsetenv("MURI")
	os.Unsetenv("KB")
	cases := []struct {
		name    string
		yaml    string
		wantErr string // empty → must load cleanly
	}{
		{
			name:    "enabled with no trusted peer",
			yaml:    "http:\n  trustProxy:\n    enabled: true\n",
			wantErr: "no peer is trusted",
		},
		{
			name:    "topology declared but not enabled",
			yaml:    "http:\n  trustProxy:\n    private: true\n",
			wantErr: "enabled is false",
		},
		{
			name:    "header declared but not enabled",
			yaml:    "http:\n  trustProxy:\n    header: X-Real-IP\n",
			wantErr: "enabled is false",
		},
		{
			name:    "malformed CIDR",
			yaml:    "http:\n  trustProxy:\n    enabled: true\n    proxies: [\"10.0.0.0/99\"]\n",
			wantErr: "not a valid CIDR range",
		},
		{
			name:    "malformed IP",
			yaml:    "http:\n  trustProxy:\n    enabled: true\n    proxies: [\"not-an-ip\"]\n",
			wantErr: "not a valid IP address",
		},
		{
			name: "valid block round-trips",
			yaml: "http:\n  trustProxy:\n    enabled: true\n    private: true\n    proxies: [\"10.0.0.7\", \"fc00::/7\"]\n    header: X-Real-IP\n",
		},
		{
			name: "absent block is valid",
			yaml: "http:\n  bodyLimitBytes: 4096\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := LoadConfigFrom(writeTemp(t, validYAMLAllRequired+tc.yaml))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("LoadConfigFrom: %v", err)
				}
				if strings.Contains(tc.yaml, "trustProxy") {
					tp := cfg.HTTP.TrustProxy
					if tp == nil || !tp.Enabled || !tp.Private || tp.Header != "X-Real-IP" || len(tp.Proxies) != 2 {
						t.Fatalf("trustProxy round-trip = %+v", tp)
					}
				} else if cfg.HTTP.TrustProxy != nil {
					t.Fatalf("trustProxy = %+v with no block declared, want nil", cfg.HTTP.TrustProxy)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected a validation error mentioning %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q should mention %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestTrustSources(t *testing.T) {
	cases := []struct {
		name  string
		block TrustProxyConfig
		want  string
	}{
		{"none", TrustProxyConfig{}, ""},
		{"proxies", TrustProxyConfig{Proxies: []string{"10.0.0.1"}}, "proxies"},
		{"private", TrustProxyConfig{Private: true}, "private"},
		{"loopback", TrustProxyConfig{Loopback: true}, "loopback"},
		{"linkLocal", TrustProxyConfig{LinkLocal: true}, "linkLocal"},
		{"unixSocket", TrustProxyConfig{UnixSocket: true}, "unixSocket"},
		{
			"all, in declaration order",
			TrustProxyConfig{Proxies: []string{"10.0.0.1"}, Private: true, Loopback: true, LinkLocal: true, UnixSocket: true},
			"proxies, private, loopback, linkLocal, unixSocket",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := strings.Join(tc.block.trustSources(), ", "); got != tc.want {
				t.Errorf("trustSources() = %q, want %q", got, tc.want)
			}
		})
	}
}

// The not-enabled refusal has to NAME what was declared, or the operator is
// left guessing which key to drop.
func TestTrustProxyValidate_NotEnabledNamesEveryDeclaredKey(t *testing.T) {
	err := (&TrustProxyConfig{LinkLocal: true, UnixSocket: true, Header: "X-Real-IP"}).validate()
	if err == nil {
		t.Fatal("expected a refusal")
	}
	for _, key := range []string{"linkLocal", "unixSocket", "header"} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error %q should name %q", err.Error(), key)
		}
	}
}

func TestParseForwardedEntry(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"   ", ""},
		{"1.2.3.4", "1.2.3.4"},
		{"  1.2.3.4  ", "1.2.3.4"},
		{"1.2.3.4:8080", "1.2.3.4"},
		{"2001:db8::1", "2001:db8::1"},
		{"[2001:db8::1]:8080", "2001:db8::1"},
		{"garbage", ""},
		{"garbage:8080", ""},
		{"_", ""},
	}
	for _, tc := range cases {
		got := ""
		if ip := parseForwardedEntry(tc.in); ip != nil {
			got = ip.String()
		}
		if got != tc.want {
			t.Errorf("parseForwardedEntry(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
