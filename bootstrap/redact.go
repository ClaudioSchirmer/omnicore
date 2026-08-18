package bootstrap

import (
	"regexp"
	"strings"
)

// Secret masking for the connection strings the boot sequence logs
// ("database connected", "mongo connected", "sync engine started").
//
// A DSN is not one grammar — every engine the framework supports carries its
// credentials differently, and a masker that understands only one of them
// leaks the others verbatim into stdout:
//
//	postgres    postgres://user:PASS@host/db          URL userinfo
//	            host=h user=u password=PASS dbname=d  libpq keyword/value
//	mysql       user:PASS@tcp(host:3306)/db           driver DSN, NO scheme
//	sqlserver   sqlserver://user:PASS@host?database=d URL userinfo
//	            server=h;user id=u;password=PASS      ADO keyword/value
//	oracle      oracle://user:PASS@host:1521/SERVICE  URL userinfo
//	sqlite      file:./app.db                         no credentials at all
//	mongo/nats  mongodb://user:PASS@host/db           URL userinfo
//
// redact therefore covers three shapes: the userinfo of a scheme-ful URI, the
// userinfo of a scheme-less driver DSN (MySQL), and the value of a
// password-bearing key in the keyword/value dialects — the last one applied
// unconditionally so it also catches a secret riding a URI query string
// (`?password=…`) or an `odbc:`-prefixed connection string.

// secretKeyValueRe matches the value of a password-bearing key in the
// keyword/value dialects. The value alternatives cover ODBC brace quoting
// (`{p;w}`, which may contain the `;` separator) and libpq quoting before
// falling back to "everything up to the next separator".
var secretKeyValueRe = regexp.MustCompile(`(?i)\b(password|passwd|pwd)\s*=\s*(\{[^}]*\}|'[^']*'|"[^"]*"|[^;&\s]*)`)

// keyValueDSNHead matches a DSN that OPENS with a `key=` pair — the libpq and
// ADO keyword forms. Such a DSN has no userinfo section, so the userinfo pass
// is skipped for it and only the key=value pass runs. Everything else without
// a scheme is treated as a driver DSN (MySQL).
var keyValueDSNHead = regexp.MustCompile(`^\s*[A-Za-z][A-Za-z0-9 _]*=`)

// redact masks the credentials of a connection string for logs. It never
// returns the secret on any supported engine; an input it does not recognize
// is returned unchanged (it carries no userinfo and no password key).
//
//	postgres://user:PASS@host/db      → postgres://user:***@host/db
//	user:PASS@tcp(localhost:3306)/db  → user:***@tcp(localhost:3306)/db
//	server=h;user id=sa;password=PASS → server=h;user id=sa;password=***
func redact(dsn string) string {
	if dsn == "" {
		return ""
	}
	out := dsn
	if i := strings.Index(out, "://"); i >= 0 {
		scheme, rest := out[:i+3], out[i+3:]
		out = scheme + maskUserInfo(rest, uriAuthorityEnd(rest))
	} else if !keyValueDSNHead.MatchString(out) {
		out = maskUserInfo(out, driverAuthorityEnd(out))
	}
	return secretKeyValueRe.ReplaceAllString(out, "${1}=***")
}

// redactEach applies redact to every entry of a transport endpoint list —
// NATS accepts nats://user:PASS@host:4222, so the endpoints logged at sync
// startup are the same class of secret as a database DSN.
func redactEach(endpoints []string) []string {
	if len(endpoints) == 0 {
		return endpoints
	}
	out := make([]string, len(endpoints))
	for i, e := range endpoints {
		out[i] = redact(e)
	}
	return out
}

// uriAuthorityEnd returns where the authority of a scheme-ful URI ends —
// RFC 3986: the first '/', '?' or '#' following the scheme.
func uriAuthorityEnd(rest string) int {
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		return i
	}
	return len(rest)
}

// driverAuthorityEnd mirrors go-sql-driver/mysql's own parse: the dbname
// starts at the LAST '/', so `[user[:password]@][protocol[(address)]]` is
// everything before it. Scanning to the last slash (rather than the first)
// keeps a password containing '/' inside the searched region, and keeps the
// '/' of a `unix(/path/to.sock)` address out of the way.
func driverAuthorityEnd(dsn string) int {
	if i := strings.LastIndex(dsn, "/"); i >= 0 {
		return i
	}
	return len(dsn)
}

// maskUserInfo replaces the password of `user:password@` with *** inside
// s[:end]. The LAST '@' of the region delimits the userinfo (a password may
// contain an unescaped '@' in a driver DSN) and the FIRST ':' delimits the
// user (everything after it is secret). No ':' means no password to mask.
func maskUserInfo(s string, end int) string {
	at := strings.LastIndex(s[:end], "@")
	if at < 0 {
		return s
	}
	colon := strings.Index(s[:at], ":")
	if colon < 0 {
		return s
	}
	return s[:colon+1] + "***" + s[at:]
}
