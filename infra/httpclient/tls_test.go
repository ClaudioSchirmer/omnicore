package httpclient

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
)

// --- cipher suite resolution ---------------------------------------------

func TestResolveCipherSuites_Presets(t *testing.T) {
	for _, name := range []string{"modern", "intermediate", "legacy"} {
		got, err := resolveCipherSuites([]string{name})
		if err != nil {
			t.Errorf("preset %q: %v", name, err)
		}
		if len(got) == 0 {
			t.Errorf("preset %q returned empty list", name)
		}
	}
}

func TestResolveCipherSuites_ExplicitList(t *testing.T) {
	got, err := resolveCipherSuites([]string{"TLS_AES_128_GCM_SHA256", "TLS_AES_256_GCM_SHA384"})
	if err != nil {
		t.Fatalf("explicit: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("got %d suites; want 2", len(got))
	}
}

func TestResolveCipherSuites_UnknownNameErrors(t *testing.T) {
	if _, err := resolveCipherSuites([]string{"TLS_FAKE_NAME"}); err == nil {
		t.Error("expected error for unknown cipher")
	}
}

func TestResolveCipherSuites_Empty(t *testing.T) {
	got, err := resolveCipherSuites(nil)
	if err != nil || got != nil {
		t.Errorf("nil input should yield nil/nil; got (%v, %v)", got, err)
	}
}

// --- validation ----------------------------------------------------------

func TestValidateTLSConfig_BadMinVersion(t *testing.T) {
	errs := validateTLSConfig("x", &TLSConfig{MinVersion: "0.9"})
	if len(errs) == 0 || !strings.Contains(errs[0], "minVersion") {
		t.Errorf("expected minVersion error; got %v", errs)
	}
}

func TestValidateTLSConfig_OnlyCertNoKey(t *testing.T) {
	errs := validateTLSConfig("x", &TLSConfig{ClientCertFile: "/cert.pem"})
	if len(errs) == 0 || !strings.Contains(errs[0], "clientKeyFile") {
		t.Errorf("expected clientKeyFile error; got %v", errs)
	}
}

func TestValidateTLSConfig_OnlyKeyNoCert(t *testing.T) {
	errs := validateTLSConfig("x", &TLSConfig{ClientKeyFile: "/key.pem"})
	if len(errs) == 0 || !strings.Contains(errs[0], "clientCertFile") {
		t.Errorf("expected clientCertFile error; got %v", errs)
	}
}

func TestValidatePoolConfig_Negative(t *testing.T) {
	cfg := &PoolConfig{MaxIdleConnsPerHost: -1, MaxConnsPerHost: -1, IdleConnTimeout: Duration(-time.Second)}
	errs := validatePoolConfig("x", cfg)
	if len(errs) != 3 {
		t.Errorf("expected 3 errors; got %v", errs)
	}
}

// --- cascade -------------------------------------------------------------

func TestResolveTLSConfig_DefaultsOverridden(t *testing.T) {
	d := &TLSConfig{MinVersion: "1.2"}
	s := &TLSConfig{MinVersion: "1.3"}
	cfg, err := resolveTLSConfig(d, s)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if cfg.MinVersion != tls.VersionTLS13 {
		t.Errorf("service should override; got %v", cfg.MinVersion)
	}
}

func TestResolvePoolConfig_DefaultsAndService(t *testing.T) {
	d := &PoolConfig{MaxIdleConnsPerHost: 10, MaxConnsPerHost: 20}
	s := &PoolConfig{MaxConnsPerHost: 5}
	maxIdle, maxConns, _, _ := resolvePoolConfig(d, s)
	if maxIdle != 10 {
		t.Errorf("MaxIdle = %d; want 10 (inherited from defaults)", maxIdle)
	}
	if maxConns != 5 {
		t.Errorf("MaxConns = %d; want 5 (overridden by service)", maxConns)
	}
}

func TestResolvePoolConfig_FrameworkDefaults(t *testing.T) {
	maxIdle, maxConns, idleTimeout, disableKA := resolvePoolConfig(nil, nil)
	if maxIdle != defaultMaxIdleConnsPerHost || maxConns != defaultMaxConnsPerHost || idleTimeout != defaultIdleConnTimeout || disableKA {
		t.Errorf("framework defaults broken: %d %d %v %v", maxIdle, maxConns, idleTimeout, disableKA)
	}
}

// --- file loaders --------------------------------------------------------

func writePEM(t *testing.T, name, contents string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(contents), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return p
}

func TestLoadCABundle_Invalid(t *testing.T) {
	p := writePEM(t, "ca.pem", "not a pem")
	if _, err := loadCABundle(p); err == nil {
		t.Error("expected error for bad PEM")
	}
}

func TestLoadCABundle_MissingFile(t *testing.T) {
	if _, err := loadCABundle("/no/such/path"); err == nil {
		t.Error("expected error for missing file")
	}
}

// --- E2E -----------------------------------------------------------------

func TestE2E_DefaultTLS_TrustedServer(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()
	// Build a client whose CA pool trusts the test server's self-signed cert.
	cfg := &Config{
		Services: map[string]ServiceConfig{
			"svc": {
				BaseURL: srv.URL,
				TLS:     &TLSConfig{},
				Endpoints: map[string]EndpointConfig{
					"call": {Method: "GET", Path: "/x"},
				},
			},
		},
	}
	c, err := New(cfg, WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Inject the server's CA via the per-service transport's TLS config so
	// the test does not need access to a real CA file.
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	c.snap().services["svc"].tlsConfig.RootCAs = pool
	c.snap().services["svc"].transport.TLSClientConfig = c.snap().services["svc"].tlsConfig

	type req struct{}
	type resp struct {
		Ok bool `json:"ok"`
	}
	got, err := Call[req, resp](configuration.NewAppContextWithRandomID(configuration.LangPTBR), c, "svc", "call", req{})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !got.Ok {
		t.Errorf("got %+v", got)
	}
}

func TestE2E_InsecureSkipVerify_Works(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	skip := true
	cfg := &Config{
		Services: map[string]ServiceConfig{
			"svc": {
				BaseURL: srv.URL,
				TLS:     &TLSConfig{InsecureSkipVerify: &skip},
				Endpoints: map[string]EndpointConfig{
					"call": {Method: "GET", Path: "/x"},
				},
			},
		},
	}
	c, err := New(cfg, WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	type req struct{}
	_, err = Call[req, struct{}](configuration.NewAppContextWithRandomID(configuration.LangPTBR), c, "svc", "call", req{})
	if err != nil {
		t.Errorf("Call with insecureSkipVerify should succeed; got %v", err)
	}
}

func TestE2E_BadTLSConfig_BootFails(t *testing.T) {
	cfg := &Config{
		Services: map[string]ServiceConfig{
			"svc": {
				BaseURL: "https://x.example.com",
				TLS:     &TLSConfig{MinVersion: "9.9"},
				Endpoints: map[string]EndpointConfig{
					"call": {Method: "GET", Path: "/x"},
				},
			},
		},
	}
	if _, err := New(cfg, WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))); err == nil {
		t.Error("expected boot to fail on bad minVersion")
	}
}

func TestE2E_PoolOverride_ReachesTransport(t *testing.T) {
	cfg := &Config{
		Services: map[string]ServiceConfig{
			"svc": {
				BaseURL: "https://x.example.com",
				Pool:    &PoolConfig{MaxConnsPerHost: 7, MaxIdleConnsPerHost: 3},
				Endpoints: map[string]EndpointConfig{
					"call": {Method: "GET", Path: "/x"},
				},
			},
		},
	}
	c, err := New(cfg, WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tr := c.snap().services["svc"].transport
	if tr.MaxConnsPerHost != 7 {
		t.Errorf("MaxConnsPerHost = %d; want 7", tr.MaxConnsPerHost)
	}
	if tr.MaxIdleConnsPerHost != 3 {
		t.Errorf("MaxIdleConnsPerHost = %d; want 3", tr.MaxIdleConnsPerHost)
	}
}
