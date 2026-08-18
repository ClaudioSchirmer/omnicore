package bootstrap

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRedact covers the credential shape of EVERY engine the framework
// supports, plus the transports. The case that used to be asserted as
// "no scheme → returned verbatim" is exactly the MySQL DSN grammar, so it is
// now asserted masked: leaving it verbatim printed the password of a MySQL
// boot to stdout on every start.
func TestRedact(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		// postgres — URL userinfo and libpq keyword/value
		{"postgres url", "postgres://user:secret@h:5433/db?sslmode=disable", "postgres://user:***@h:5433/db?sslmode=disable"},
		{"postgres keyword", "host=h port=5432 user=u password=secret dbname=d", "host=h port=5432 user=u password=*** dbname=d"},
		{"postgres quoted keyword", "host=h password='se cret' dbname=d", "host=h password=*** dbname=d"},

		// mysql — scheme-less driver DSN
		{"mysql", "omnicore:omnicore@tcp(localhost:3307)/users_db", "omnicore:***@tcp(localhost:3307)/users_db"},
		{"mysql with params", "root:root@tcp(127.0.0.1:3307)/?parseTime=true&multiStatements=true", "root:***@tcp(127.0.0.1:3307)/?parseTime=true&multiStatements=true"},
		{"mysql no password", "root@tcp(127.0.0.1:3307)/db", "root@tcp(127.0.0.1:3307)/db"},
		{"mysql password with slash", "u:pa/ss@tcp(h:3306)/db", "u:***@tcp(h:3306)/db"},
		{"mysql unix socket", "u:secret@unix(/tmp/mysql.sock)/db", "u:***@unix(/tmp/mysql.sock)/db"},

		// sqlserver — URL and ADO keyword/value (the QA bench uses the latter)
		{"sqlserver url", "sqlserver://sa:OmnicoreQA!2026@127.0.0.1:14333?database=d", "sqlserver://sa:***@127.0.0.1:14333?database=d"},
		{"sqlserver ado", "server=127.0.0.1;port=14333;user id=sa;password=OmnicoreQA!2026;encrypt=true", "server=127.0.0.1;port=14333;user id=sa;password=***;encrypt=true"},
		{"sqlserver ado braced", "server=h;password={pa;ss};encrypt=true", "server=h;password=***;encrypt=true"},
		{"sqlserver odbc prefix", "odbc:server=h;password=secret;encrypt=true", "odbc:server=h;password=***;encrypt=true"},

		// oracle
		{"oracle", "oracle://sys:OmnicoreQA!2026@127.0.0.1:15211/FREEPDB1?dba+privilege=sysdba", "oracle://sys:***@127.0.0.1:15211/FREEPDB1?dba+privilege=sysdba"},

		// sqlite — carries no credentials; must survive untouched
		{"sqlite file", "file:./app.db", "file:./app.db"},
		{"sqlite file with pragmas", "file:/var/lib/app.db?_pragma=busy_timeout(5000)", "file:/var/lib/app.db?_pragma=busy_timeout(5000)"},
		{"sqlite memory", ":memory:", ":memory:"},

		// mongo + transports
		{"mongo", "mongodb://u:p@host:27017/db", "mongodb://u:***@host:27017/db"},
		{"mongo no credentials", "mongodb://localhost:27018", "mongodb://localhost:27018"},
		{"mongo password with at", "mongodb+srv://u:p@ss@cluster/db", "mongodb+srv://u:***@cluster/db"},
		{"nats", "nats://user:secret@localhost:4222", "nats://user:***@localhost:4222"},

		// secret riding a query string rather than the userinfo
		{"password query param", "postgres://h:5433/db?user=u&password=secret&sslmode=disable", "postgres://h:5433/db?user=u&password=***&sslmode=disable"},

		// non-secrets and degenerate inputs
		{"no @", "postgres://localhost/db", "postgres://localhost/db"},
		{"no colon in userinfo", "postgres://userhost@h/d", "postgres://userhost@h/d"},
		{"key ending in password is not a match", "host=h;apppasswordless=true", "host=h;apppasswordless=true"},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redact(tc.in)
			if got != tc.want {
				t.Fatalf("redact(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestRedactEach masks every transport endpoint and leaves an empty list
// alone (the nil is forwarded to the log attr unchanged).
func TestRedactEach(t *testing.T) {
	got := redactEach([]string{"nats://u:secret@a:4222", "localhost:4222"})
	want := []string{"nats://u:***@a:4222", "localhost:4222"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("redactEach()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if redactEach(nil) != nil {
		t.Fatal("redactEach(nil) should stay nil")
	}
}

func TestBuild_MissingConfig(t *testing.T) {
	// APP_PROFILE=dev so the loader gets past env-var validation and proceeds
	// to file lookup — the failure must come from the missing file, not from
	// the missing env var.
	t.Setenv(profileEnv, profileDev)
	t.Setenv(configPathEnv, "")
	tmp := t.TempDir()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })

	// Sanity: file really does not exist in cwd.
	if _, err := os.Stat(filepath.Join(tmp, "microservice.dev.yaml")); !os.IsNotExist(err) {
		t.Fatalf("setup invalid: microservice.dev.yaml exists or unexpected err: %v", err)
	}

	deps, cfg, err := Build()
	if err == nil {
		t.Fatalf("Build() should fail when microservice.dev.yaml is missing; got deps=%+v cfg=%+v", deps, cfg)
	}
}
