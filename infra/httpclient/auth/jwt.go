package auth

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// decodeJWTExp returns the exp claim of a JWT as a time.Time. The token
// is treated as "header.payload.signature"; only the payload is decoded.
// No signature verification — the upstream IdP signed the token, and
// the framework's job here is to consume it for TTL accounting, not to
// re-verify cryptographic integrity.
//
// Errors:
//   - token does not have three parts → invalid format
//   - base64 / json decode fails → malformed payload
//   - exp claim missing or not a number → no expiration
func decodeJWTExp(token string) (time.Time, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, fmt.Errorf("jwt: expected 3 dot-separated parts, got %d", len(parts))
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// Try standard encoding too — some IdPs pad their JWT payloads
		// even though the spec requires raw URL-safe encoding.
		raw, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return time.Time{}, fmt.Errorf("jwt: decode payload: %w", err)
		}
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		return time.Time{}, fmt.Errorf("jwt: parse payload: %w", err)
	}
	exp, ok := claims["exp"]
	if !ok {
		return time.Time{}, fmt.Errorf("jwt: no exp claim")
	}
	var sec int64
	switch v := exp.(type) {
	case float64:
		sec = int64(v)
	case int64:
		sec = v
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			return time.Time{}, fmt.Errorf("jwt: exp is not an integer: %w", err)
		}
		sec = n
	default:
		return time.Time{}, fmt.Errorf("jwt: exp has unsupported type %T", exp)
	}
	return time.Unix(sec, 0), nil
}
