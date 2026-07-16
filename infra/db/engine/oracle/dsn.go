//go:build oracle

package oracle

import "net/url"

// ensureLobFetch forces go-ora's `lob fetch=post` connection option into a
// URL-form DSN when the consumer did not set it. Without the option go-ora
// truncates the READ of a native JSON column at 32 KiB (the write side stores
// the full value — proven against a live 23ai: a 40 KB payload came back
// 32768 bytes, and full with the option), silently corrupting large payloads
// on the composer/read path. A consumer-supplied `lob fetch` value wins — the
// guard only fills the gap. A DSN that does not parse as go-ora's oracle://
// URL form passes through untouched: the driver, not this guard, owns its
// rejection.
func ensureLobFetch(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil || u.Scheme != "oracle" {
		return dsn
	}
	q := u.Query()
	if q.Get("lob fetch") != "" {
		return dsn
	}
	q.Set("lob fetch", "post")
	u.RawQuery = q.Encode()
	return u.String()
}
