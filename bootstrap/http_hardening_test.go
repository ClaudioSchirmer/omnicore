package bootstrap

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestBuildApp_HTTPHardeningWiredIntoFiber(t *testing.T) {
	d := silentDeps()
	bl, rt, it := 1048576, 15, 60
	d.Config.HTTP.BodyLimitBytes = &bl
	d.Config.HTTP.ReadTimeoutSeconds = &rt
	d.Config.HTTP.IdleTimeoutSeconds = &it
	app, err := buildApp(context.Background(), d, Wiring{})
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}
	cfg := app.Config()
	if cfg.BodyLimit != bl {
		t.Errorf("fiber BodyLimit = %d, want %d", cfg.BodyLimit, bl)
	}
	if cfg.ReadTimeout != time.Duration(rt)*time.Second {
		t.Errorf("fiber ReadTimeout = %v, want %v", cfg.ReadTimeout, time.Duration(rt)*time.Second)
	}
	if cfg.IdleTimeout != time.Duration(it)*time.Second {
		t.Errorf("fiber IdleTimeout = %v, want %v", cfg.IdleTimeout, time.Duration(it)*time.Second)
	}
}

func TestBuildApp_HTTPHardeningDefaultsWhenUnset(t *testing.T) {
	// Unset knobs must leave Fiber's own defaults: the 4 MiB body limit and no
	// transport timeouts — so upgrading changes nothing for a service that never
	// sets them.
	app, err := buildApp(context.Background(), silentDeps(), Wiring{})
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}
	cfg := app.Config()
	if cfg.BodyLimit != 4*1024*1024 {
		t.Errorf("default BodyLimit = %d, want the Fiber 4 MiB default", cfg.BodyLimit)
	}
	if cfg.ReadTimeout != 0 || cfg.IdleTimeout != 0 {
		t.Errorf("default timeouts = read %v / idle %v, want 0 / 0", cfg.ReadTimeout, cfg.IdleTimeout)
	}
}

func TestConfigValidate_HTTPHardeningRejectsNegatives(t *testing.T) {
	os.Unsetenv("DB")
	os.Unsetenv("MURI")
	os.Unsetenv("KB")
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{"bodyLimit", "http:\n  bodyLimitBytes: -5\n", "bodyLimitBytes"},
		{"readTimeout", "http:\n  readTimeoutSeconds: -1\n", "readTimeoutSeconds"},
		{"idleTimeout", "http:\n  idleTimeoutSeconds: -1\n", "idleTimeoutSeconds"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTemp(t, validYAMLAllRequired+tc.yaml)
			_, err := LoadConfigFrom(path)
			if err == nil {
				t.Fatalf("expected a validation error for negative %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q should name %q", err.Error(), tc.want)
			}
		})
	}
}

func TestConfigLoad_HTTPHardeningRoundTrips(t *testing.T) {
	os.Unsetenv("DB")
	os.Unsetenv("MURI")
	os.Unsetenv("KB")
	yaml := validYAMLAllRequired + "http:\n  bodyLimitBytes: 1048576\n  readTimeoutSeconds: 15\n  idleTimeoutSeconds: 60\n"
	path := writeTemp(t, yaml)
	cfg, err := LoadConfigFrom(path)
	if err != nil {
		t.Fatalf("LoadConfigFrom: %v", err)
	}
	if cfg.HTTP.BodyLimitBytes == nil || *cfg.HTTP.BodyLimitBytes != 1048576 {
		t.Errorf("BodyLimitBytes = %v, want 1048576", cfg.HTTP.BodyLimitBytes)
	}
	if cfg.HTTP.ReadTimeoutSeconds == nil || *cfg.HTTP.ReadTimeoutSeconds != 15 {
		t.Errorf("ReadTimeoutSeconds = %v, want 15", cfg.HTTP.ReadTimeoutSeconds)
	}
	if cfg.HTTP.IdleTimeoutSeconds == nil || *cfg.HTTP.IdleTimeoutSeconds != 60 {
		t.Errorf("IdleTimeoutSeconds = %v, want 60", cfg.HTTP.IdleTimeoutSeconds)
	}
}
