package httpclient

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// signingClock is injected for testing so the middleware's notion of "now"
// can be pinned deterministically. Production paths leave it at the
// time.Now default.
type signingClock func() time.Time

var defaultSigningClock signingClock = func() time.Time { return time.Now().UTC() }

// signingMiddleware injects the timestamp + content-sha256 headers, builds
// the AWS SigV4-lite canonical string, computes HMAC-SHA256(secret,
// canonical), and attaches the signature header.
//
// Sits at chain position 8 — inner of retry / breaker, outer of transport.
// Reasoning: every retry attempt must produce a fresh signature because
// the injected timestamp advances per attempt; cache hits never reach
// signing because they short-circuit at position 5 (correct — a cached
// response did not dial); breaker rejections never reach signing because
// they short-circuit at position 7 (correct — no dial happens).
//
// Header sources available to signedHeaders at this position:
//
//   - host (always present)
//   - authorization (set by auth middleware at position 3, when configured)
//   - x-idempotency-key (set by idempotency middleware at position 4, when configured)
//   - content-type and any headers from the defaults/service/endpoint cascade
//   - WithExtraHeader / CallConfig override headers
//   - timestampHeader + contentSHA256Header (set by THIS middleware)
//
// Headers added by downstream middleware (retry, breaker, transport) do
// not exist when signing runs — that's fine because none of them set
// outbound headers.
func signingMiddleware(policy signingPolicy, clock signingClock) roundTripper {
	if clock == nil {
		clock = defaultSigningClock
	}
	return rtFunc(func(ctx context.Context, req *http.Request, obs *observation, next roundTripper) (*http.Response, error) {
		if policy.disabled() {
			return next.RoundTrip(ctx, req, obs, nil)
		}
		body := bodyForSigning(req, obs)
		injectSigningHeaders(req, policy, clock(), body)
		signature := computeHMACSHA256(policy.secret, buildCanonicalString(req, policy, body))
		req.Header.Set(policy.signatureHeader, policy.signaturePrefix+signature)
		if policy.keyId != "" && policy.keyIdHeader != "" {
			req.Header.Set(policy.keyIdHeader, policy.keyId)
		}
		return next.RoundTrip(ctx, req, obs, nil)
	})
}

// bodyForSigning recovers the request body bytes from the observation
// buffered by loggingMiddleware (chain position 2). When the request has
// no body, returns nil so SHA256 of an empty body is computed correctly.
func bodyForSigning(req *http.Request, obs *observation) []byte {
	if obs != nil && len(obs.RequestBody) > 0 {
		return obs.RequestBody
	}
	if req.Body == nil {
		return nil
	}
	// Defensive read path — runs only when logging did not capture the
	// body for some reason (e.g., obs is nil in a synthesized test).
	data, err := io.ReadAll(req.Body)
	if err != nil {
		return nil
	}
	req.Body = io.NopCloser(bytes.NewReader(data))
	return data
}

// injectSigningHeaders writes the timestamp + content-sha256 headers to
// the request. Overwrites prior values so retries get a fresh timestamp.
func injectSigningHeaders(req *http.Request, policy signingPolicy, now time.Time, body []byte) {
	req.Header.Set(policy.timestampHeader, formatTimestamp(now, policy.timestampFormat))
	if policy.contentSHA256Header != "" {
		req.Header.Set(policy.contentSHA256Header, hexSHA256(body))
	}
}

// formatTimestamp renders the time using the configured format. RFC 1123
// matches the HTTP-Date convention so it slots into HTTP headers without
// further escaping. ISO 8601 uses the basic AWS SigV4 layout
// (YYYYMMDDTHHMMSSZ) so upstreams that follow AWS conventions accept it.
// unix-seconds writes the decimal integer count, used by GitHub/Twilio.
func formatTimestamp(t time.Time, format string) string {
	switch format {
	case timestampFormatISO8601:
		return t.UTC().Format("20060102T150405Z")
	case timestampFormatUnixSecond:
		return strconv.FormatInt(t.UTC().Unix(), 10)
	default:
		return t.UTC().Format(http.TimeFormat)
	}
}

// buildCanonicalString assembles the AWS SigV4-lite canonical string:
//
//	METHOD\nPATH\nQUERY_CANONICAL\nHEADERS_CANONICAL\nSIGNED_HEADERS_LIST\nSHA256_HEX(BODY)
//
// PATH is the URL path component (no query). QUERY_CANONICAL is the
// query parameters sorted by key, each key=value percent-encoded.
// HEADERS_CANONICAL is each signed header in the order from policy
// (already lowercase + sorted), rendered as "name:trimmed_value\n".
// SIGNED_HEADERS_LIST is the same names joined by ";".
// SHA256_HEX(BODY) is hex(SHA256(body)); SHA256 of the empty byte slice
// when there is no body.
func buildCanonicalString(req *http.Request, policy signingPolicy, body []byte) string {
	var sb strings.Builder
	sb.WriteString(strings.ToUpper(req.Method))
	sb.WriteByte('\n')
	sb.WriteString(canonicalPath(req.URL))
	sb.WriteByte('\n')
	sb.WriteString(canonicalQuery(req.URL))
	sb.WriteByte('\n')
	sb.WriteString(canonicalHeaders(req, policy.signedHeaders))
	sb.WriteByte('\n')
	sb.WriteString(strings.Join(policy.signedHeaders, ";"))
	sb.WriteByte('\n')
	sb.WriteString(hexSHA256(body))
	return sb.String()
}

// canonicalPath returns the URL's path component as-is when it is not
// empty, "/" otherwise. The framework leaves percent-encoding to the
// URL builder upstream — the path that arrives here is what will be
// dialed, so signing it byte-for-byte matches what the upstream sees.
func canonicalPath(u *url.URL) string {
	if u == nil || u.Path == "" {
		return "/"
	}
	return u.Path
}

// canonicalQuery sorts query parameters by key and joins them with "&".
// Each value is URL-encoded via url.QueryEscape so signing matches the
// wire byte sequence. Repeated keys are sorted by value to keep the
// canonical form deterministic.
func canonicalQuery(u *url.URL) string {
	if u == nil {
		return ""
	}
	values := u.Query()
	if len(values) == 0 {
		return ""
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sortStrings(keys)
	var parts []string
	for _, k := range keys {
		vs := append([]string(nil), values[k]...)
		sortStrings(vs)
		for _, v := range vs {
			parts = append(parts, url.QueryEscape(k)+"="+url.QueryEscape(v))
		}
	}
	return strings.Join(parts, "&")
}

// canonicalHeaders renders the signed-header block. For each header in
// signedHeaders (already lowercase + sorted) emits "name:trimmed_value\n".
// Multiple values are joined by ",". Missing headers contribute an empty
// value — operators that need strict presence should validate at the
// upstream side.
//
// Special case: the "host" pseudo-header is resolved from req.URL.Host
// rather than req.Header because the http stdlib only sets Host on the
// request struct (not in the Header map) for outbound requests.
func canonicalHeaders(req *http.Request, signedHeaders []string) string {
	var sb strings.Builder
	for _, name := range signedHeaders {
		sb.WriteString(name)
		sb.WriteByte(':')
		sb.WriteString(headerCanonicalValue(req, name))
		sb.WriteByte('\n')
	}
	return sb.String()
}

// headerCanonicalValue resolves the value for a given header name,
// handling the "host" special case and multi-value headers.
func headerCanonicalValue(req *http.Request, name string) string {
	if name == "host" {
		if req.Host != "" {
			return strings.TrimSpace(req.Host)
		}
		if req.URL != nil {
			return strings.TrimSpace(req.URL.Host)
		}
		return ""
	}
	// http.Header keys are canonicalized to MIME form (X-Foo-Bar). The
	// stdlib Get does the right canonicalization automatically.
	v := req.Header.Values(http.CanonicalHeaderKey(name))
	if len(v) == 0 {
		return ""
	}
	if len(v) == 1 {
		return strings.TrimSpace(v[0])
	}
	parts := make([]string, len(v))
	for i, s := range v {
		parts[i] = strings.TrimSpace(s)
	}
	return strings.Join(parts, ",")
}

// hexSHA256 returns the lowercase hex of SHA256(data). nil and empty
// slices produce the well-known SHA256 of the empty input.
func hexSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// computeHMACSHA256 returns hex(HMAC_SHA256(key, msg)). hex output is
// lowercase, matching the convention used by AWS SigV4 and the majority
// of HMAC-signing APIs.
func computeHMACSHA256(key []byte, msg string) string {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(msg))
	return hex.EncodeToString(mac.Sum(nil))
}
