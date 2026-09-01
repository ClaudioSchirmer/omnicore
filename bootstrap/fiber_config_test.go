package bootstrap

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
)

func TestBuildApp_FiberConfigHatchReachesTheLongTail(t *testing.T) {
	// The point of the hatch: a fiber.Config field the framework models
	// nowhere still reaches the app, so nobody is blocked on a release.
	app, err := buildApp(context.Background(), silentDeps(), Wiring{
		FiberConfig: func(cfg *fiber.Config) {
			cfg.StreamRequestBody = true
			cfg.ServerHeader = "omnicore-test"
			cfg.StrictRouting = true
		},
	})
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}
	cfg := app.Config()
	if !cfg.StreamRequestBody || cfg.ServerHeader != "omnicore-test" || !cfg.StrictRouting {
		t.Fatalf("hatch fields dropped: stream %v / server %q / strict %v",
			cfg.StreamRequestBody, cfg.ServerHeader, cfg.StrictRouting)
	}
}

func TestBuildApp_YAMLOutranksFiberConfigHatch(t *testing.T) {
	// Deployment posture belongs to the operator's file, so a knob set in
	// both places resolves to the YAML — never to the compiled-in value.
	d := silentDeps()
	yamlLimit := 4096
	d.Config.HTTP.BodyLimitBytes = &yamlLimit
	app, err := buildApp(context.Background(), d, Wiring{
		FiberConfig: func(cfg *fiber.Config) {
			cfg.BodyLimit = 1
			cfg.ReadBufferSize = 8192 // untouched by YAML — survives
		},
	})
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}
	cfg := app.Config()
	if cfg.BodyLimit != yamlLimit {
		t.Errorf("BodyLimit = %d, want the YAML value %d", cfg.BodyLimit, yamlLimit)
	}
	if cfg.ReadBufferSize != 8192 {
		t.Errorf("ReadBufferSize = %d, want the hatch value 8192 (no YAML spelling)", cfg.ReadBufferSize)
	}
}

func TestBuildApp_FiberConfigHatchCannotReplaceFrameworkFields(t *testing.T) {
	// AppName and ErrorHandler are written last and unconditionally: the
	// error envelope of every route is built on the framework's handler, so
	// a hatch that tries to take it over must lose.
	d := silentDeps()
	app, err := buildApp(context.Background(), d, Wiring{
		FiberConfig: func(cfg *fiber.Config) {
			cfg.AppName = "hijacked"
			cfg.ErrorHandler = func(c fiber.Ctx, _ error) error {
				return c.Status(418).SendString("HIJACKED")
			}
		},
		BeforeServe: func(app *fiber.App, _ Deps) error {
			app.Get("/boom", func(fiber.Ctx) error { return errors.New("boom") })
			return nil
		},
	})
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}
	if got := app.Config().AppName; got != d.Config.Service {
		t.Errorf("AppName = %q, want the service name %q", got, d.Config.Service)
	}
	resp, err := app.Test(httptest.NewRequest("GET", "/boom", nil))
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == 418 || strings.Contains(string(body), "HIJACKED") {
		t.Fatalf("the hatch replaced the framework ErrorHandler: %d %s", resp.StatusCode, body)
	}
}

// The trusted-proxy trio is the ONE part of fiber.Config the hatch may not
// reach: those fields are half of a mechanism whose other half is a middleware
// registered from the yaml block, so setting them here yields a service that
// trusts the forwarded header by Fiber's leftmost rule — the entry a caller can
// forge — and boots without a word. It is refused instead.
func TestBuildApp_FiberConfigHatchCannotEnableTrustProxy(t *testing.T) {
	cases := []struct {
		name  string
		mutex func(*fiber.Config)
		want  string
	}{
		{
			name:  "TrustProxy",
			mutex: func(c *fiber.Config) { c.TrustProxy = true },
			want:  "TrustProxy",
		},
		{
			name:  "TrustProxyConfig.Proxies",
			mutex: func(c *fiber.Config) { c.TrustProxyConfig = fiber.TrustProxyConfig{Proxies: []string{"10.0.0.7"}} },
			want:  "TrustProxyConfig",
		},
		{
			name:  "TrustProxyConfig range flag",
			mutex: func(c *fiber.Config) { c.TrustProxyConfig = fiber.TrustProxyConfig{Private: true} },
			want:  "TrustProxyConfig",
		},
		{
			name:  "ProxyHeader",
			mutex: func(c *fiber.Config) { c.ProxyHeader = fiber.HeaderXForwardedFor },
			want:  "ProxyHeader",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildApp(context.Background(), silentDeps(), Wiring{FiberConfig: tc.mutex})
			if err == nil {
				t.Fatal("buildApp succeeded — the hatch enabled trusted proxy behind the framework's back")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q should name %q", err.Error(), tc.want)
			}
			if !strings.Contains(err.Error(), "http.trustProxy") {
				t.Errorf("error %q should point at the yaml block", err.Error())
			}
		})
	}
}

// The refusal names every field the hatch touched, not just the first.
func TestBuildApp_FiberConfigHatchRefusalNamesEveryField(t *testing.T) {
	_, err := buildApp(context.Background(), silentDeps(), Wiring{
		FiberConfig: func(c *fiber.Config) {
			c.TrustProxy = true
			c.TrustProxyConfig = fiber.TrustProxyConfig{Private: true}
			c.ProxyHeader = fiber.HeaderXForwardedFor
		},
	})
	if err == nil {
		t.Fatal("expected a refusal")
	}
	for _, f := range []string{"TrustProxy", "TrustProxyConfig", "ProxyHeader"} {
		if !strings.Contains(err.Error(), f) {
			t.Errorf("error %q should name %q", err.Error(), f)
		}
	}
}

// The refusal must not fire on a hatch that leaves the trio alone — the whole
// long tail stays reachable.
func TestBuildApp_FiberConfigHatchUnaffectedByTheGuard(t *testing.T) {
	app, err := buildApp(context.Background(), silentDeps(), Wiring{
		FiberConfig: func(c *fiber.Config) {
			c.StreamRequestBody = true
			c.EnableIPValidation = true // adjacent, but not part of the trio
		},
	})
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}
	if !app.Config().StreamRequestBody {
		t.Error("the long tail stopped reaching the app")
	}
}
