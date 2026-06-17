package httpclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// cacheMiddleware is the GET/HEAD cache layer at chain position 6. On a hit
// it reconstructs the response from the stored entry and short-circuits;
// on a miss it delegates and stores 2xx responses (plus acceptable-status
// responses when cacheAcceptable is true on the endpoint).
//
// Bypass paths (obs.CacheStatus = "bypass"): policy disabled, method other
// than GET/HEAD, per-call CallConfig.NoCache, or Cache-Control: no-store
// on the response.
//
// Backend errors are surfaced verbatim. The middleware does not translate
// them or swallow them — failure policy (open / closed) lives on the
// backend. Backends that opt into fail-open swallow transport errors
// internally and return (nil, false, nil) on Get (treated as miss) /
// nil on Set (treated as no-op write). Backends that opt into
// fail-closed return the error and the call aborts at this layer.
func cacheMiddleware(serviceName, endpointName string, store Cache, policy cachePolicy, cacheAcceptable bool, acceptableStatus map[int]struct{}) roundTripper {
	return rtFunc(func(ctx context.Context, req *http.Request, obs *observation, next roundTripper) (*http.Response, error) {
		if !policy.enabled || !isCacheableMethod(req.Method) {
			obs.CacheStatus = "bypass"
			return next.RoundTrip(ctx, req, obs, nil)
		}
		if obs.noCache {
			obs.CacheStatus = "bypass"
			return next.RoundTrip(ctx, req, obs, nil)
		}
		key := obs.cacheKey
		if key == "" {
			key = buildCacheKey(serviceName, endpointName, req, policy)
		}
		entry, ok, err := store.Get(ctx, key)
		if err != nil {
			return nil, err
		}
		if ok {
			obs.CacheStatus = "hit"
			return materializeResponse(req, entry), nil
		}
		obs.CacheStatus = "miss"
		resp, err := next.RoundTrip(ctx, req, obs, nil)
		if err != nil || resp == nil {
			return resp, err
		}
		if !shouldStore(resp, policy, cacheAcceptable, acceptableStatus) {
			return resp, nil
		}
		bodyBytes, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		ttl := effectiveTTL(resp, policy)
		newEntry := &CacheEntry{
			Body:          bodyBytes,
			Headers:       cloneHeader(resp.Header),
			Status:        resp.StatusCode,
			ContentType:   resp.Header.Get("Content-Type"),
			ContentLength: int64(len(bodyBytes)),
			ExpiresAt:     time.Now().Add(ttl),
		}
		if setErr := store.Set(ctx, key, newEntry); setErr != nil {
			return nil, setErr
		}
		resp.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		resp.ContentLength = newEntry.ContentLength
		return resp, nil
	})
}

// isCacheableMethod reports whether the HTTP method is one the cache layer
// considers for storage. The set is intentionally minimal — GET and HEAD —
// matching the design and HTTP semantics.
func isCacheableMethod(m string) bool {
	switch strings.ToUpper(m) {
	case "GET", "HEAD":
		return true
	}
	return false
}

// shouldStore decides whether to persist the response. 2xx is always
// storable; acceptable-status codes (e.g. 404 on a presence-check) are
// storable only when the endpoint opted in via cacheAcceptable: true.
// no-store on the response is honored when policy.honorCacheControl is on.
func shouldStore(resp *http.Response, policy cachePolicy, cacheAcceptable bool, acceptable map[int]struct{}) bool {
	if policy.honorCacheControl && hasNoStore(resp) {
		return false
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return true
	}
	if !cacheAcceptable {
		return false
	}
	_, ok := acceptable[resp.StatusCode]
	return ok
}

// effectiveTTL picks the TTL for the entry. When honorCacheControl is on
// and the response carries Cache-Control: max-age=N, N seconds wins over
// the configured TTL (capped by frameworkCacheDefaultTTL only if N is
// nonsensically large — we trust the upstream).
func effectiveTTL(resp *http.Response, policy cachePolicy) time.Duration {
	if policy.honorCacheControl {
		if d, ok := maxAgeFromCacheControl(resp.Header.Get("Cache-Control")); ok {
			return d
		}
	}
	return policy.ttl
}

// hasNoStore reports whether the response's Cache-Control header tells the
// client not to store this response (no-store) or revalidate every time
// (no-cache, treated as no-store in the current phase — full revalidation
// arrives with the redaction expansion phase).
func hasNoStore(resp *http.Response) bool {
	v := resp.Header.Get("Cache-Control")
	if v == "" {
		return false
	}
	for _, p := range strings.Split(v, ",") {
		p = strings.TrimSpace(strings.ToLower(p))
		if p == "no-store" || p == "no-cache" {
			return true
		}
	}
	return false
}

// maxAgeFromCacheControl parses the max-age=<seconds> directive when present.
func maxAgeFromCacheControl(v string) (time.Duration, bool) {
	if v == "" {
		return 0, false
	}
	for _, p := range strings.Split(v, ",") {
		p = strings.TrimSpace(strings.ToLower(p))
		if !strings.HasPrefix(p, "max-age=") {
			continue
		}
		raw := strings.TrimPrefix(p, "max-age=")
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			return 0, false
		}
		return time.Duration(n) * time.Second, true
	}
	return 0, false
}

// buildCacheKey composes the deterministic key from service, endpoint and
// the request shape, optionally varied by selected header / query values.
// The format is a flat pipe-delimited string; consumers should treat it as
// opaque (it is internal to the cache subsystem).
func buildCacheKey(service, endpoint string, req *http.Request, policy cachePolicy) string {
	var b strings.Builder
	b.WriteString(service)
	b.WriteString("|")
	b.WriteString(endpoint)
	b.WriteString("|")
	b.WriteString(strings.ToUpper(req.Method))
	b.WriteString("|")
	if req.URL != nil {
		b.WriteString(req.URL.Path)
	}
	b.WriteString("|")
	b.WriteString(sortedQuery(req.URL))
	for _, h := range policy.varyHeaders {
		b.WriteString("|h:")
		b.WriteString(hashString(req.Header.Get(h)))
	}
	for _, q := range policy.varyQueries {
		v := ""
		if req.URL != nil {
			v = req.URL.Query().Get(q)
		}
		b.WriteString("|q:")
		b.WriteString(hashString(v))
	}
	return b.String()
}

// sortedQuery returns the query string with parameters in lexicographic
// order so a=1&b=2 and b=2&a=1 hash to the same key.
func sortedQuery(u *url.URL) string {
	if u == nil {
		return ""
	}
	q := u.Query()
	if len(q) == 0 {
		return ""
	}
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	first := true
	for _, k := range keys {
		vals := q[k]
		sort.Strings(vals)
		for _, v := range vals {
			if !first {
				b.WriteString("&")
			}
			b.WriteString(url.QueryEscape(k))
			b.WriteString("=")
			b.WriteString(url.QueryEscape(v))
			first = false
		}
	}
	return b.String()
}

// hashString produces the SHA-256 hex of s, used so varyOn values don't
// leak verbatim into the key (which may end up in metrics or debug logs).
func hashString(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// cloneHeader produces a deep copy so the cache entry is decoupled from
// the original response.
func cloneHeader(h http.Header) http.Header {
	if len(h) == 0 {
		return http.Header{}
	}
	out := make(http.Header, len(h))
	for k, vs := range h {
		copied := make([]string, len(vs))
		copy(copied, vs)
		out[k] = copied
	}
	return out
}

// materializeResponse rebuilds an *http.Response from a cached entry. The
// body is wrapped in a fresh NopCloser so multiple hits each get a
// rewindable reader.
func materializeResponse(req *http.Request, entry *CacheEntry) *http.Response {
	return &http.Response{
		Status:        http.StatusText(entry.Status),
		StatusCode:    entry.Status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        cloneHeader(entry.Headers),
		Body:          io.NopCloser(bytes.NewReader(entry.Body)),
		ContentLength: entry.ContentLength,
		Request:       req,
	}
}
