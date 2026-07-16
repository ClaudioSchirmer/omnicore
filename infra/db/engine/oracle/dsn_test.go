//go:build oracle

package oracle

import (
	"net/url"
	"strings"
	"testing"
)

// TestEnsureLobFetch locks the one DSN guard this engine applies: go-ora's
// `lob fetch=post` option is forced in when absent — without it the driver
// truncates native-JSON READS at 32 KiB (proven against a live 23ai) — while a
// consumer-supplied value and a non-URL DSN pass through untouched.
func TestEnsureLobFetch(t *testing.T) {
	t.Run("absent → post is added", func(t *testing.T) {
		got := ensureLobFetch("oracle://app:pw@db:1521/FREEPDB1")
		u, err := url.Parse(got)
		if err != nil {
			t.Fatalf("result does not parse: %v", err)
		}
		if v := u.Query().Get("lob fetch"); v != "post" {
			t.Fatalf("lob fetch = %q, want post (in %q)", v, got)
		}
	})

	t.Run("existing options are preserved alongside", func(t *testing.T) {
		got := ensureLobFetch("oracle://app:pw@db:1521/FREEPDB1?ssl=true")
		u, _ := url.Parse(got)
		if u.Query().Get("ssl") != "true" || u.Query().Get("lob fetch") != "post" {
			t.Fatalf("options lost: %q", got)
		}
	})

	t.Run("consumer-supplied lob fetch wins", func(t *testing.T) {
		dsn := "oracle://app:pw@db:1521/FREEPDB1?lob+fetch=pre"
		if got := ensureLobFetch(dsn); got != dsn {
			t.Fatalf("consumer value overridden: %q → %q", dsn, got)
		}
	})

	t.Run("non-oracle-URL DSN passes through untouched", func(t *testing.T) {
		for _, dsn := range []string{"server=x;user id=y", "postgres://x/y", ""} {
			if got := ensureLobFetch(dsn); got != dsn {
				t.Errorf("ensureLobFetch(%q) = %q, want it untouched", dsn, got)
			}
		}
	})

	t.Run("credentials and target survive the rewrite", func(t *testing.T) {
		got := ensureLobFetch("oracle://app:pw@db:1521/FREEPDB1")
		for _, part := range []string{"oracle://", "app:pw@db:1521", "/FREEPDB1"} {
			if !strings.Contains(got, part) {
				t.Errorf("rewritten DSN lost %q: %q", part, got)
			}
		}
	})
}
