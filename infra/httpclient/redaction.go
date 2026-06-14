package httpclient

import (
	"encoding/json"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
)

// redactedPlaceholder replaces sensitive header values in slog observations.
// The literal is matched by operators/grep so changes here ripple to dashboards.
const redactedPlaceholder = "[REDACTED]"

// defaultRedactedHeaders are the header names whose values are replaced with
// redactedPlaceholder in slog observations. The set follows the design's
// Section 13 default block list — the operator-visible defaults that an SRE
// expects out of the box.
//
// Per-service overrides + body JSON-path redaction + query-key redaction
// arrive in the dedicated redaction expansion phase.
var defaultRedactedHeaders = map[string]struct{}{
	textproto.CanonicalMIMEHeaderKey("Authorization"):       {},
	textproto.CanonicalMIMEHeaderKey("Proxy-Authorization"): {},
	textproto.CanonicalMIMEHeaderKey("Cookie"):              {},
	textproto.CanonicalMIMEHeaderKey("Set-Cookie"):          {},
	textproto.CanonicalMIMEHeaderKey("X-API-Key"):           {},
}

// redactHeaders returns a copy of h with sensitive values replaced by
// redactedPlaceholder. Header matching is case-insensitive (CanonicalMIMEHeaderKey).
// Falls back to the framework default block list when policy is nil.
func redactHeaders(h http.Header, policy *redactionPolicy) http.Header {
	if len(h) == 0 {
		return h
	}
	set := defaultRedactedHeaders
	if policy != nil {
		set = policy.headerSet
	}
	out := make(http.Header, len(h))
	for k, vs := range h {
		ck := textproto.CanonicalMIMEHeaderKey(k)
		if _, blocked := set[ck]; blocked {
			out[k] = []string{redactedPlaceholder}
			continue
		}
		copyVs := make([]string, len(vs))
		copy(copyVs, vs)
		out[k] = copyVs
	}
	return out
}

// redactBody attempts to parse body as JSON and mask values at each
// configured JSONPath. Non-JSON bodies are returned verbatim. Re-encodes
// to JSON when redaction succeeds; on any error returns the original body
// so the slog line never breaks because of a malformed payload.
func redactBody(body []byte, paths []string) []byte {
	if len(body) == 0 || len(paths) == 0 {
		return body
	}
	var root any
	if err := json.Unmarshal(body, &root); err != nil {
		return body
	}
	mutated := false
	for _, path := range paths {
		if redactPath(root, path) {
			mutated = true
		}
	}
	if !mutated {
		return body
	}
	out, err := json.Marshal(root)
	if err != nil {
		return body
	}
	return out
}

// redactPath walks root via dot-notation JSONPath (e.g. "$.user.password")
// and replaces the leaf with redactedPlaceholder. Returns true when a
// substitution actually happened.
func redactPath(root any, path string) bool {
	p := strings.TrimPrefix(strings.TrimSpace(path), "$")
	p = strings.TrimPrefix(p, ".")
	if p == "" {
		return false
	}
	parts := strings.Split(p, ".")
	cur := root
	for i, key := range parts[:len(parts)-1] {
		m, ok := cur.(map[string]any)
		if !ok {
			return false
		}
		next, ok := m[key]
		if !ok {
			return false
		}
		cur = next
		_ = i
	}
	leaf := parts[len(parts)-1]
	m, ok := cur.(map[string]any)
	if !ok {
		return false
	}
	if _, present := m[leaf]; !present {
		return false
	}
	m[leaf] = redactedPlaceholder
	return true
}

// redactURL parses the URL string and masks any query parameter whose
// key (case-insensitive) appears in the policy's queryKeys set. The path
// and unlisted parameters survive verbatim. Bad URLs are returned as-is.
func redactURL(raw string, queryKeys map[string]struct{}) string {
	if raw == "" || len(queryKeys) == 0 {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.RawQuery == "" {
		return raw
	}
	q := u.Query()
	mutated := false
	for k, vs := range q {
		if _, blocked := queryKeys[strings.ToLower(k)]; !blocked {
			continue
		}
		for i := range vs {
			vs[i] = redactedPlaceholder
		}
		q[k] = vs
		mutated = true
	}
	if !mutated {
		return raw
	}
	u.RawQuery = q.Encode()
	return u.String()
}
