package httpclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ClaudioSchirmer/omnicore/infra/cache"
)

// cacheMiddleware is the GET/HEAD cache layer at chain position 6. On a hit
// it reconstructs the response from the stored entry and short-circuits;
// on a miss it delegates and stores 2xx responses (plus acceptable-status
// responses when cacheAcceptable is true on the endpoint).
//
// Bypass paths (obs.CacheStatus = "bypass"): policy disabled, the backing
// cache.Cache is nil (operator omitted the top-level cache: block in YAML),
// method other than GET/HEAD, per-call CallConfig.NoCache, or
// Cache-Control: no-store on the response.
//
// The backing cache.Cache is read from the HttpClient through `cacheRef`
// — a getter, not a value — so a late SetCache by the bootstrap path
// (Wiring.Cache + cache.store: custom) reaches the middleware without
// rebuilding the chain. Errors returned by cache.Cache propagate verbatim:
// the cache backend's failMode (open / closed) decides whether transport
// errors collapse to a miss internally or bubble up here.
func cacheMiddleware(serviceName, endpointName string, cacheRef func() cache.Cache, policy cachePolicy, cacheAcceptable bool, acceptableStatus map[int]struct{}, logger *slog.Logger) roundTripper {
	return rtFunc(func(ctx context.Context, req *http.Request, obs *observation, next roundTripper) (*http.Response, error) {
		store := cacheRef()
		if store == nil || !policy.enabled || !isCacheableMethod(req.Method) {
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
		raw, ok, err := store.Get(ctx, key)
		if err != nil {
			return nil, err
		}
		if ok {
			var entry cacheEntry
			if decErr := json.Unmarshal(raw, &entry); decErr != nil {
				// Corrupt entry — treat as miss, log, and proceed to upstream.
				// Not a transport error; the next store.Set will overwrite it
				// with a fresh well-formed value.
				if logger != nil {
					logger.Warn("httpclient.cache.decode.error",
						slog.String("service", serviceName),
						slog.String("endpoint", endpointName),
						slog.String("key", key),
						slog.String("error", decErr.Error()))
				}
			} else {
				obs.CacheStatus = "hit"
				return materializeResponse(req, &entry), nil
			}
		}
		obs.CacheStatus = "miss"
		resp, err := next.RoundTrip(ctx, req, obs, nil)
		if err != nil || resp == nil {
			return resp, err
		}
		if !shouldStore(resp, policy, cacheAcceptable, acceptableStatus) {
			return resp, nil
		}
		ttl := effectiveTTL(resp, policy)
		// TTL == 0 from Cache-Control: max-age=0 means "do not store this
		// response" per HTTP semantics. The byte-cache layer treats 0 as
		// "no expiration" (which would be the opposite of what max-age=0
		// asks for), so we short-circuit here.
		if ttl <= 0 {
			return resp, nil
		}
		bodyBytes, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		entry := cacheEntry{
			Body:          bodyBytes,
			Headers:       cloneHeader(resp.Header),
			Status:        resp.StatusCode,
			ContentType:   resp.Header.Get("Content-Type"),
			ContentLength: int64(len(bodyBytes)),
			ExpiresAt:     time.Now().Add(ttl),
		}
		encoded, encErr := json.Marshal(entry)
		if encErr != nil {
			// cacheEntry only carries primitives + http.Header → json.Marshal
			// cannot fail in practice. Defensive: surface the error.
			return nil, encErr
		}
		if setErr := store.Set(ctx, key, encoded, ttl); setErr != nil {
			return nil, setErr
		}
		resp.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		resp.ContentLength = entry.ContentLength
		return resp, nil
	})
}

// cacheEntry is the wire shape the httpclient persists into the byte-
// level cache.Cache. JSON-encoded so the value round-trips through any
// backend (memory store, Redis, custom) and stays human-debuggable via
// the backend's CLI (e.g. `redis-cli GET <key>`).
//
// Field order / json tag stability matters across releases — backends
// may store entries written by an older binary that a newer binary
// reads after a deploy.
type cacheEntry struct {
	Body          []byte      `json:"body"`
	Headers       http.Header `json:"headers"`
	Status        int         `json:"status"`
	ContentType   string      `json:"contentType"`
	ContentLength int64       `json:"contentLength"`
	ExpiresAt     time.Time   `json:"expiresAt"`
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
// the configured TTL.
func effectiveTTL(resp *http.Response, policy cachePolicy) time.Duration {
	if policy.honorCacheControl {
		if d, ok := maxAgeFromCacheControl(resp.Header.Get("Cache-Control")); ok {
			return d
		}
	}
	return policy.ttl
}

// hasNoStore reports whether the response's Cache-Control header tells the
// client not to store this response.
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

func hashString(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

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

func materializeResponse(req *http.Request, entry *cacheEntry) *http.Response {
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
