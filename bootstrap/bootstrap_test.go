package bootstrap

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRedact(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"postgres with password", "postgres://user:secret@h/d", "postgres://user:***@h/d"},
		{"mongo with password", "mongodb://u:p@host:27017/db", "mongodb://u:***@host:27017/db"},
		{"no @", "postgres://localhost/db", "postgres://localhost/db"},
		{"no scheme", "user:secret@h/d", "user:secret@h/d"},
		{"no colon in userinfo", "postgres://userhost@h/d", "postgres://userhost@h/d"},
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
