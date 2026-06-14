package auth

import (
	"fmt"
	"strings"
)

// walkJSONPath walks a "$.foo.bar" expression against root and returns
// the leaf value. Only dot notation is supported — no slices, no filters.
// The leading "$" is optional.
//
// Used by the OAuth2 provider's response-field source to extract the
// expiry value (or a token field) from the IdP response. JSONPath is a
// well-known mini-language; this implementation is deliberately the
// minimum that the design schema requires.
func walkJSONPath(path string, root any) (any, error) {
	p := strings.TrimPrefix(strings.TrimSpace(path), "$")
	p = strings.TrimPrefix(p, ".")
	if p == "" {
		return root, nil
	}
	parts := strings.Split(p, ".")
	cur := root
	for i, key := range parts {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("jsonpath: %q is not an object at segment %d (%q)", reprPath(parts, i), i, key)
		}
		v, ok := m[key]
		if !ok {
			return nil, fmt.Errorf("jsonpath: %q is missing key %q", reprPath(parts, i), key)
		}
		cur = v
	}
	return cur, nil
}

func reprPath(parts []string, upTo int) string {
	return "$." + strings.Join(parts[:upTo], ".")
}
