package bootstrap

import (
	"fmt"
	"net"
	"strings"

	"github.com/gofiber/fiber/v3"
)

// TrustProxyConfig is the http.trustProxy YAML block — the deployment's
// reverse-proxy topology, declared where the rest of the deployment posture
// lives rather than compiled into the service.
//
// It exists because the request's origin is only knowable from the forwarded
// headers when the peer that set them is trusted. The framework reads the
// origin in three places — the access log's ip field, the server span's
// client.address/server.address/url.scheme, and anything a handler asks
// c.IP()/c.Host()/c.Scheme() for — and all three are wrong in the same
// direction behind an unconfigured load balancer: they describe the balancer,
// not the caller.
//
// The allowlist is what separates this from a header-spoofing hole. Fiber
// consults it per request: a peer inside it gets its X-Forwarded-* honored,
// a peer outside it is read from the socket exactly as if the block were
// absent. An attacker reaching the service directly therefore cannot forge
// an origin, whatever headers it sends.
type TrustProxyConfig struct {
	// Enabled turns the block on. Every other field is inert without it, and
	// declaring one while this is false is rejected at boot rather than
	// silently ignored — a proxies list that does nothing is the kind of
	// configuration that reads as protection and isn't.
	Enabled bool `yaml:"enabled"`

	// Proxies is the explicit allowlist: bare IPs ("10.0.0.7") and/or CIDR
	// ranges ("10.0.0.0/8"), v4 or v6. This is the tight option — name the
	// balancer, trust nothing else.
	Proxies []string `yaml:"proxies"`

	// Private trusts the RFC1918 / ULA ranges (10/8, 172.16/12, 192.168/16,
	// fc00::/7). The usual choice inside a cluster, where the ingress IP is
	// assigned by the platform and not known when the YAML is written.
	Private bool `yaml:"private"`

	// Loopback trusts 127.0.0.0/8 and ::1/128 — a sidecar proxy on the same
	// network namespace.
	Loopback bool `yaml:"loopback"`

	// LinkLocal trusts 169.254.0.0/16 and fe80::/10.
	LinkLocal bool `yaml:"linkLocal"`

	// UnixSocket trusts requests arriving over a Unix domain socket.
	UnixSocket bool `yaml:"unixSocket"`

	// Header names the request header carrying the client IP. Empty →
	// X-Forwarded-For. Set it to X-Real-IP, CF-Connecting-IP, True-Client-IP
	// or whatever the edge in front actually writes; the value is read only
	// from trusted peers, so naming the wrong header degrades to the socket
	// IP rather than to a forgeable one.
	Header string `yaml:"header"`
}

// trustSources lists the trust-granting fields that are set, in YAML spelling.
// Used both by validate (to demand at least one) and by its error message.
func (t *TrustProxyConfig) trustSources() []string {
	var on []string
	if len(t.Proxies) > 0 {
		on = append(on, "proxies")
	}
	if t.Private {
		on = append(on, "private")
	}
	if t.Loopback {
		on = append(on, "loopback")
	}
	if t.LinkLocal {
		on = append(on, "linkLocal")
	}
	if t.UnixSocket {
		on = append(on, "unixSocket")
	}
	return on
}

// validate enforces the two rules that keep the block honest, plus the
// syntax of the allowlist. nil (block absent) is valid — that is the
// socket-only default.
func (t *TrustProxyConfig) validate() error {
	if t == nil {
		return nil
	}
	sources := t.trustSources()
	if !t.Enabled {
		// Declaring topology without enabling it is a misconfiguration that
		// looks like a working one, so it fails the boot instead.
		declared := sources
		if t.Header != "" {
			declared = append(declared, "header")
		}
		if len(declared) > 0 {
			return fmt.Errorf(
				"http.trustProxy declares %s but enabled is false — set enabled: true or drop the block",
				strings.Join(declared, ", "))
		}
		return nil
	}
	if len(sources) == 0 {
		// Fiber skips the allowlist entirely when it is empty, which means
		// trusting every peer's X-Forwarded-For — the exact spoofing hole
		// the block is supposed to close. Refuse it; a deployment that
		// genuinely wants it says so with proxies: ["0.0.0.0/0"].
		return fmt.Errorf(
			"http.trustProxy.enabled is true but no peer is trusted — set proxies and/or private, loopback, linkLocal, unixSocket")
	}
	for _, p := range t.Proxies {
		if strings.Contains(p, "/") {
			if _, _, err := net.ParseCIDR(p); err != nil {
				return fmt.Errorf("http.trustProxy.proxies: %q is not a valid CIDR range", p)
			}
			continue
		}
		if net.ParseIP(p) == nil {
			return fmt.Errorf("http.trustProxy.proxies: %q is not a valid IP address or CIDR range", p)
		}
	}
	return nil
}

// apply writes the block onto the fiber.Config being built. No-op when the
// block is absent or disabled, so the socket-only default costs nothing.
func (t *TrustProxyConfig) apply(cfg *fiber.Config) {
	if t == nil || !t.Enabled {
		return
	}
	cfg.TrustProxy = true
	cfg.TrustProxyConfig = fiber.TrustProxyConfig{
		Proxies:    t.Proxies,
		Private:    t.Private,
		Loopback:   t.Loopback,
		LinkLocal:  t.LinkLocal,
		UnixSocket: t.UnixSocket,
	}
	// Fiber reads the client address from ProxyHeader, and its rule is
	// "leftmost valid entry" — which is the entry the CALLER wrote on any
	// edge that appends (nginx's proxy_add_x_forwarded_for, the default in
	// most guides). TrustProxy gates WHETHER the header is read, never WHICH
	// entry is believed, so delegating would reopen the very spoof this block
	// exists to close. So the framework resolves the address itself, in
	// clientIPMiddleware, and points Fiber at a private header that only that
	// middleware ever writes — leaving the operator's header untouched for
	// c.IPs() and for any handler that wants the full chain.
	cfg.ProxyHeader = canonicalClientIPHeader
	// Load-bearing, and not for the reason its name suggests. The canonical
	// header already carries a single parsed address, so there is nothing left
	// to validate — but Fiber only falls back to the socket peer INSIDE this
	// branch (extractIPFromHeader). Left false it returns the raw header
	// value, which is the EMPTY STRING on every request where the resolution
	// deliberately deposits nothing: a chain that is entirely trusted, one
	// with a malformed entry, or an absent header. Those are exactly the cases
	// that must degrade to the peer, so turning this off makes c.IP() return
	// "" instead. It also makes c.IPs() — which always reads the raw
	// X-Forwarded-For — drop malformed entries.
	cfg.EnableIPValidation = true
}

// rejectHatchProxyConfig refuses a Wiring.FiberConfig that set any of Fiber's
// trusted-proxy fields.
//
// Those three fields are only HALF of request-origin resolution: the other half
// is clientIPMiddleware, which walks the forwarded chain rightmost-untrusted and
// is registered from the yaml block. A hatch can reach the fiber.Config half and
// nothing else, so setting it there produces a service that trusts the header
// with Fiber's own leftmost rule and no validation — c.IP() then returns the raw
// comma-separated header, or, if the hatch also sets EnableIPValidation, the
// entry the CALLER wrote. Both are worse than the socket-peer default, and both
// boot silently. So this is a boot failure with the yaml block named, rather
// than a silent overwrite: the developer who wrote that code is the one who
// needs to know it did not take effect.
func rejectHatchProxyConfig(cfg *fiber.Config) error {
	var set []string
	if cfg.TrustProxy {
		set = append(set, "TrustProxy")
	}
	// TrustProxyConfig holds a map, so it is not comparable — check the
	// declared fields rather than the struct.
	tp := cfg.TrustProxyConfig
	if len(tp.Proxies) > 0 || tp.Private || tp.Loopback || tp.LinkLocal || tp.UnixSocket {
		set = append(set, "TrustProxyConfig")
	}
	if cfg.ProxyHeader != "" {
		set = append(set, "ProxyHeader")
	}
	if len(set) == 0 {
		return nil
	}
	return fmt.Errorf(
		"bootstrap: Wiring.FiberConfig set %s, but request-origin resolution is owned by the framework "+
			"(it needs a middleware a fiber.Config cannot carry, and the Fiber fields alone trust the "+
			"forwarded header by its leftmost entry — the one a caller can forge). "+
			"Declare the topology under http.trustProxy in the yaml instead",
		strings.Join(set, " + "))
}

// headerName is the operator's forwarded header, defaulted.
func (t *TrustProxyConfig) headerName() string {
	if t.Header != "" {
		return t.Header
	}
	return fiber.HeaderXForwardedFor
}

// canonicalClientIPHeader is where the framework deposits the resolved client
// address for Fiber to read back through c.IP(). It is stripped from every
// inbound request before anything else runs, so a caller cannot supply it.
const canonicalClientIPHeader = "X-Omnicore-Client-IP"

// trustedSet is the compiled allowlist — built once at boot, consulted per
// forwarded entry. It answers the same question as Fiber's IsProxyTrusted,
// but about an address INSIDE the header rather than about the socket peer.
type trustedSet struct {
	ips                          map[string]struct{}
	ranges                       []*net.IPNet
	private, loopback, linkLocal bool
}

func (t *TrustProxyConfig) compile() *trustedSet {
	s := &trustedSet{
		ips:       make(map[string]struct{}, len(t.Proxies)),
		private:   t.Private,
		loopback:  t.Loopback,
		linkLocal: t.LinkLocal,
	}
	for _, p := range t.Proxies {
		if strings.Contains(p, "/") {
			// validate() already rejected malformed entries at boot.
			if _, n, err := net.ParseCIDR(p); err == nil {
				s.ranges = append(s.ranges, n)
			}
			continue
		}
		if ip := net.ParseIP(p); ip != nil {
			s.ips[ip.String()] = struct{}{}
		}
	}
	return s
}

func (s *trustedSet) contains(ip net.IP) bool {
	if _, ok := s.ips[ip.String()]; ok {
		return true
	}
	if s.loopback && ip.IsLoopback() {
		return true
	}
	if s.private && ip.IsPrivate() {
		return true
	}
	if s.linkLocal && (ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast()) {
		return true
	}
	for _, n := range s.ranges {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// parseForwardedEntry reads one comma-separated element. Bare addresses are
// the norm; some edges write host:port, and IPv6 then arrives bracketed.
func parseForwardedEntry(raw string) net.IP {
	e := strings.TrimSpace(raw)
	if e == "" {
		return nil
	}
	if ip := net.ParseIP(e); ip != nil {
		return ip
	}
	if host, _, err := net.SplitHostPort(e); err == nil {
		return net.ParseIP(strings.Trim(host, "[]"))
	}
	return nil
}

// resolveClientIP walks the chain RIGHT to left and returns the first address
// that is not a trusted proxy — the last hop the trusted infrastructure can
// actually vouch for. Everything to its left was written by something outside
// the trusted set and is therefore forgeable.
//
// Returns "" when the chain is empty, unparseable, or entirely trusted; the
// caller then leaves the canonical header unset and Fiber falls back to the
// socket peer. That fallback is the conservative answer, not a failure: if
// every hop is trusted there is no untrusted origin to report.
func resolveClientIP(chain string, s *trustedSet) string {
	if chain == "" {
		return ""
	}
	parts := strings.Split(chain, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		ip := parseForwardedEntry(parts[i])
		if ip == nil {
			// A malformed entry breaks the chain of custody: everything
			// further left is vouched for by something unreadable.
			return ""
		}
		if !s.contains(ip) {
			return ip.String()
		}
	}
	return ""
}

// clientIPMiddleware resolves the request origin before anything reads it.
// Registered first, ahead of the access log and the server span, so all three
// (and every handler calling c.IP()) see the same address.
func clientIPMiddleware(header string, s *trustedSet) fiber.Handler {
	return func(c fiber.Ctx) error {
		// Unconditionally: a caller must never be able to hand us the answer.
		c.Request().Header.Del(canonicalClientIPHeader)
		// Only a trusted peer's forwarded header is worth reading at all —
		// same gate Fiber applies, applied before the walk.
		if c.IsProxyTrusted() {
			if ip := resolveClientIP(string(c.Request().Header.Peek(header)), s); ip != "" {
				c.Request().Header.Set(canonicalClientIPHeader, ip)
			}
		}
		return c.Next()
	}
}
