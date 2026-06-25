package httpclient

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- validate.go: rejectReserved ----------------------------------------

func TestRejectReserved(t *testing.T) {
	var errs validationErrors
	rejectReserved(&errs, "svc", map[string]reservedField{
		"zeta":  {value: "set", phase: "Phase 9"},
		"alpha": {value: 42, phase: "Phase 9"},
		"gamma": {value: nil, phase: "Phase 9"}, // nil → skipped
	})
	if len(errs) != 2 {
		t.Fatalf("expected 2 errors (nil skipped); got %d: %v", len(errs), errs)
	}
	// Keys are sorted: alpha before zeta.
	if !strings.Contains(errs[0], "svc.alpha") || !strings.Contains(errs[0], "Phase 9") {
		t.Errorf("unexpected first error: %q", errs[0])
	}
	if !strings.Contains(errs[1], "svc.zeta") {
		t.Errorf("unexpected second error: %q", errs[1])
	}
}

func TestRejectReserved_AllNil(t *testing.T) {
	var errs validationErrors
	rejectReserved(&errs, "svc", map[string]reservedField{
		"a": {value: nil, phase: "P"},
	})
	if len(errs) != 0 {
		t.Fatalf("all-nil reserved fields must produce no errors; got %v", errs)
	}
}

// --- retry.go: network error classifiers --------------------------------

type fakeNetErr struct {
	timeout bool
}

func (e fakeNetErr) Error() string   { return "fake net error" }
func (e fakeNetErr) Timeout() bool   { return e.timeout }
func (e fakeNetErr) Temporary() bool { return false }

func TestIsTimeout(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain", errors.New("boom"), false},
		{"net.Error timeout", fakeNetErr{timeout: true}, true},
		{"net.Error not timeout", fakeNetErr{timeout: false}, false},
		{"url.Error wrapping timeout", &url.Error{Op: "Get", URL: "x", Err: fakeNetErr{timeout: true}}, true},
		{"url.Error wrapping non-timeout", &url.Error{Op: "Get", URL: "x", Err: errors.New("nope")}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTimeout(tc.err); got != tc.want {
				t.Errorf("isTimeout(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestIsDNSError(t *testing.T) {
	if !isDNSError(&net.DNSError{Name: "nope.invalid"}) {
		t.Error("expected DNSError to be classified as DNS")
	}
	if isDNSError(errors.New("boom")) {
		t.Error("plain error must not be DNS")
	}
	if isDNSError(&url.Error{Err: &net.DNSError{}}) == false {
		t.Error("wrapped DNSError must be detected")
	}
}

func TestIsNetworkError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"dns", &net.DNSError{}, true},
		{"opError", &net.OpError{Op: "dial", Err: errors.New("refused")}, true},
		{"netError", fakeNetErr{}, true},
		{"plain", errors.New("boom"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isNetworkError(tc.err); got != tc.want {
				t.Errorf("isNetworkError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// --- retry.go: computeBackoff curves ------------------------------------

func TestComputeBackoff_Curves(t *testing.T) {
	base := 10 * time.Millisecond
	maxD := 1 * time.Second
	t.Run("zero initial → 0", func(t *testing.T) {
		p := retryPolicy{backoff: backoffConstant, initialDelay: 0, maxDelay: maxD}
		if d := computeBackoff(p, 3); d != 0 {
			t.Errorf("want 0; got %v", d)
		}
	})
	t.Run("constant", func(t *testing.T) {
		p := retryPolicy{backoff: backoffConstant, initialDelay: base, maxDelay: maxD}
		if d := computeBackoff(p, 5); d != base {
			t.Errorf("constant want %v; got %v", base, d)
		}
	})
	t.Run("linear", func(t *testing.T) {
		p := retryPolicy{backoff: backoffLinear, initialDelay: base, maxDelay: maxD}
		if d := computeBackoff(p, 3); d != 3*base {
			t.Errorf("linear want %v; got %v", 3*base, d)
		}
	})
	t.Run("exponential", func(t *testing.T) {
		p := retryPolicy{backoff: backoffExponential, initialDelay: base, maxDelay: maxD}
		if d := computeBackoff(p, 3); d != base<<2 {
			t.Errorf("exponential want %v; got %v", base<<2, d)
		}
	})
	t.Run("exponential capped at maxDelay", func(t *testing.T) {
		p := retryPolicy{backoff: backoffExponential, initialDelay: base, maxDelay: 25 * time.Millisecond}
		if d := computeBackoff(p, 10); d != 25*time.Millisecond {
			t.Errorf("expected cap at maxDelay; got %v", d)
		}
	})
	t.Run("exponential-jitter within ceiling", func(t *testing.T) {
		p := retryPolicy{backoff: backoffExponentialJitter, initialDelay: base, maxDelay: maxD}
		for i := 0; i < 50; i++ {
			d := computeBackoff(p, 4)
			if d < 0 || d > maxD {
				t.Fatalf("jitter out of range: %v", d)
			}
		}
	})
	t.Run("attempt < 1 normalized", func(t *testing.T) {
		p := retryPolicy{backoff: backoffLinear, initialDelay: base, maxDelay: maxD}
		if d := computeBackoff(p, 0); d != base {
			t.Errorf("attempt<1 should normalize to 1; got %v", d)
		}
	})
	t.Run("unknown backoff defaults to initial", func(t *testing.T) {
		p := retryPolicy{backoff: backoffStrategy(99), initialDelay: base, maxDelay: maxD}
		if d := computeBackoff(p, 2); d != base {
			t.Errorf("default branch want %v; got %v", base, d)
		}
	})
}

func TestComputeWait_RespectRetryAfter(t *testing.T) {
	p := retryPolicy{
		respectRetryAfter: true,
		backoff:           backoffConstant,
		initialDelay:      time.Second,
		maxDelay:          2 * time.Second,
	}
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("Retry-After", "5") // 5s > maxDelay 2s → capped
	if d := computeWait(p, 1, resp); d != 2*time.Second {
		t.Errorf("Retry-After should cap at maxDelay; got %v", d)
	}
	// No Retry-After header → falls back to backoff curve.
	if d := computeWait(p, 1, &http.Response{Header: http.Header{}}); d != time.Second {
		t.Errorf("fallback to backoff; got %v", d)
	}
}

func TestParseRetryAfter(t *testing.T) {
	if d, ok := parseRetryAfter("3"); !ok || d != 3*time.Second {
		t.Errorf("seconds form: got %v ok=%v", d, ok)
	}
	if _, ok := parseRetryAfter(""); ok {
		t.Error("empty must fail")
	}
	if _, ok := parseRetryAfter("-1"); ok {
		t.Error("negative seconds must fail")
	}
	if _, ok := parseRetryAfter("garbage"); ok {
		t.Error("garbage must fail")
	}
	// HTTP-date in the past → not honored.
	past := time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)
	if _, ok := parseRetryAfter(past); ok {
		t.Error("past date must fail")
	}
	// HTTP-date in the future → honored.
	future := time.Now().Add(time.Hour).UTC().Format(http.TimeFormat)
	if d, ok := parseRetryAfter(future); !ok || d <= 0 {
		t.Errorf("future date: got %v ok=%v", d, ok)
	}
}

func TestSleepCtx_Canceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// Zero duration + canceled context → returns false (abort).
	if sleepCtx(ctx, 0) {
		t.Error("canceled ctx with zero duration should return false")
	}
	// Positive duration + canceled context → returns false immediately.
	if sleepCtx(ctx, time.Hour) {
		t.Error("canceled ctx should abort the sleep")
	}
	// Healthy context completes the timer.
	if !sleepCtx(context.Background(), time.Millisecond) {
		t.Error("healthy ctx should complete the sleep")
	}
	// Zero duration + healthy context → true.
	if !sleepCtx(context.Background(), 0) {
		t.Error("zero duration healthy ctx should return true")
	}
}

// --- signing.go: canonical helpers --------------------------------------

func TestCanonicalPath(t *testing.T) {
	if got := canonicalPath(nil); got != "/" {
		t.Errorf("nil URL → %q, want /", got)
	}
	u, _ := url.Parse("http://x/")
	u.Path = ""
	if got := canonicalPath(u); got != "/" {
		t.Errorf("empty path → %q, want /", got)
	}
	u2, _ := url.Parse("http://x/a/b")
	if got := canonicalPath(u2); got != "/a/b" {
		t.Errorf("got %q, want /a/b", got)
	}
}

func TestHeaderCanonicalValue(t *testing.T) {
	req, _ := http.NewRequest("GET", "http://api.example.com/x", nil)

	// host pseudo-header from req.Host.
	req.Host = "explicit.example.com"
	if got := headerCanonicalValue(req, "host"); got != "explicit.example.com" {
		t.Errorf("host from req.Host = %q", got)
	}

	// host falls back to URL.Host when req.Host empty.
	req.Host = ""
	if got := headerCanonicalValue(req, "host"); got != "api.example.com" {
		t.Errorf("host from URL.Host = %q", got)
	}

	// host with nil URL and empty req.Host → "".
	bare := &http.Request{Header: http.Header{}}
	if got := headerCanonicalValue(bare, "host"); got != "" {
		t.Errorf("bare host = %q, want empty", got)
	}

	// missing header → "".
	if got := headerCanonicalValue(req, "x-absent"); got != "" {
		t.Errorf("absent header = %q, want empty", got)
	}

	// single value trimmed.
	req.Header.Set("X-Trim", "  spaced  ")
	if got := headerCanonicalValue(req, "x-trim"); got != "spaced" {
		t.Errorf("trim = %q", got)
	}

	// multi-value joined by comma, each trimmed.
	req.Header.Add("X-Multi", " a ")
	req.Header.Add("X-Multi", " b ")
	if got := headerCanonicalValue(req, "x-multi"); got != "a,b" {
		t.Errorf("multi = %q, want a,b", got)
	}
}

func TestBodyForSigning(t *testing.T) {
	// obs carries the buffered body → returned directly.
	obs := &observation{RequestBody: []byte("buffered")}
	req, _ := http.NewRequest("POST", "http://x", strings.NewReader("ignored"))
	if got := bodyForSigning(req, obs); string(got) != "buffered" {
		t.Errorf("obs body = %q", got)
	}

	// obs nil, req.Body nil → nil.
	noBody, _ := http.NewRequest("GET", "http://x", nil)
	if got := bodyForSigning(noBody, nil); got != nil {
		t.Errorf("no body should be nil; got %q", got)
	}

	// obs nil, req.Body present → defensive read + reset.
	withBody, _ := http.NewRequest("POST", "http://x", strings.NewReader("defensive"))
	if got := bodyForSigning(withBody, nil); string(got) != "defensive" {
		t.Errorf("defensive read = %q", got)
	}
	// Body must be re-readable after the defensive read.
	again, _ := io.ReadAll(withBody.Body)
	if string(again) != "defensive" {
		t.Errorf("body not reset; second read = %q", again)
	}

	// obs present but empty body falls through to req.Body.
	emptyObs := &observation{RequestBody: nil}
	withBody2, _ := http.NewRequest("POST", "http://x", strings.NewReader("fallback"))
	if got := bodyForSigning(withBody2, emptyObs); string(got) != "fallback" {
		t.Errorf("fallback read = %q", got)
	}
}

// --- options.go: WithClientCert + codecs --------------------------------

func TestWithClientCert_Option(t *testing.T) {
	cert := tls.Certificate{Certificate: [][]byte{{0x01}}}
	cfg := applyInvokeOptions([]InvokeOption{WithClientCert(cert)})
	if cfg.clientCert == nil {
		t.Fatal("WithClientCert should set clientCert")
	}
	if len(cfg.clientCert.Certificate) != 1 {
		t.Errorf("cert not copied through")
	}
}

func TestEffectiveCodecs(t *testing.T) {
	empty := &invokeConfig{}
	if empty.effectiveRequestCodec("json") != "json" {
		t.Error("empty override should fall back to yaml request codec")
	}
	if empty.effectiveResponseCodec("xml") != "xml" {
		t.Error("empty override should fall back to yaml response codec")
	}
	set := &invokeConfig{requestCodecOverride: "form", responseCodecOverride: "xml"}
	if set.effectiveRequestCodec("json") != "form" {
		t.Error("request override should win")
	}
	if set.effectiveResponseCodec("json") != "xml" {
		t.Error("response override should win")
	}
	var nilCfg *invokeConfig
	if nilCfg.effectiveRequestCodec("json") != "json" || nilCfg.effectiveResponseCodec("json") != "json" {
		t.Error("nil receiver should fall back to yaml")
	}
}

// --- tls_config.go: loadCertPair + resolveTLSConfig with assets ---------

// genCertPairFiles writes a self-signed ECDSA cert + key pair to temp files
// and returns their paths along with the parsed tls.Certificate.
func genCertPairFiles(t *testing.T) (certPath, keyPath string, cert tls.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	dir := t.TempDir()
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	parsed, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("parse pair: %v", err)
	}
	return certPath, keyPath, parsed
}

func TestLoadCertPair(t *testing.T) {
	certPath, keyPath, _ := genCertPairFiles(t)

	cert, err := loadCertPair(certPath, keyPath)
	if err != nil {
		t.Fatalf("loadCertPair valid: %v", err)
	}
	if len(cert.Certificate) == 0 {
		t.Error("expected a populated certificate")
	}

	if _, err := loadCertPair("", keyPath); err == nil {
		t.Error("missing cert path should error")
	}
	if _, err := loadCertPair(certPath, ""); err == nil {
		t.Error("missing key path should error")
	}
	if _, err := loadCertPair("/no/cert.pem", "/no/key.pem"); err == nil {
		t.Error("nonexistent files should error")
	}
}

func TestResolveTLSConfig_LoadsCertAndCA(t *testing.T) {
	certPath, keyPath, _ := genCertPairFiles(t)
	caPath, _, _ := genCertPairFiles(t)

	cfg, err := resolveTLSConfig(nil, &TLSConfig{
		MinVersion:     "1.3",
		CipherSuites:   []string{"modern"},
		ClientCertFile: certPath,
		ClientKeyFile:  keyPath,
		CABundle:       caPath,
	})
	if err != nil {
		t.Fatalf("resolveTLSConfig: %v", err)
	}
	if cfg.MinVersion != tls.VersionTLS13 {
		t.Errorf("minVersion = %v, want 1.3", cfg.MinVersion)
	}
	if len(cfg.Certificates) != 1 {
		t.Errorf("expected loaded client cert; got %d", len(cfg.Certificates))
	}
	if cfg.RootCAs == nil {
		t.Error("expected CA pool loaded")
	}
	if len(cfg.CipherSuites) == 0 {
		t.Error("expected resolved cipher suites")
	}
}

func TestResolveTLSConfig_BothNil(t *testing.T) {
	cfg, err := resolveTLSConfig(nil, nil)
	if err != nil || cfg != nil {
		t.Errorf("both-nil should yield nil config; got (%v, %v)", cfg, err)
	}
}

func TestResolveTLSConfig_BadCipher(t *testing.T) {
	if _, err := resolveTLSConfig(nil, &TLSConfig{CipherSuites: []string{"TLS_NOPE"}}); err == nil {
		t.Error("bad cipher must error")
	}
}

func TestResolveTLSConfig_BadCAPath(t *testing.T) {
	if _, err := resolveTLSConfig(nil, &TLSConfig{CABundle: "/no/such/ca.pem"}); err == nil {
		t.Error("missing CA bundle must error")
	}
}

func TestResolveTLSConfig_BadCertPath(t *testing.T) {
	if _, err := resolveTLSConfig(nil, &TLSConfig{ClientCertFile: "/no/c.pem", ClientKeyFile: "/no/k.pem"}); err == nil {
		t.Error("missing cert pair must error")
	}
}

func TestResolveTLSConfig_InsecureSkipVerify(t *testing.T) {
	skip := true
	cfg, err := resolveTLSConfig(nil, &TLSConfig{InsecureSkipVerify: &skip})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !cfg.InsecureSkipVerify {
		t.Error("InsecureSkipVerify should propagate")
	}
}

// --- service_client.go: cloneServiceWithClientCert ----------------------

func TestCloneServiceWithClientCert_RealService(t *testing.T) {
	cfg := &Config{Services: map[string]ServiceConfig{
		"svc": validService("https://svc.example.com"),
	}}
	c, err := New(cfg, WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	svc := c.snap().services["svc"]

	_, _, cert := genCertPairFiles(t)
	clone, err := cloneServiceWithClientCert(svc, cert)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	if clone.transport == svc.transport {
		t.Error("clone must have its own transport")
	}
	if clone.tlsConfig == nil || len(clone.tlsConfig.Certificates) != 1 {
		t.Error("clone tlsConfig should carry the injected cert")
	}
	if clone.httpClient == svc.httpClient {
		t.Error("clone must have its own http.Client")
	}
	// Original untouched.
	if svc.tlsConfig != nil {
		t.Error("validService has no TLS block; original tlsConfig should remain nil")
	}
}

func TestCloneServiceWithClientCert_PreservesExistingTLS(t *testing.T) {
	skip := true
	cfg := &Config{Services: map[string]ServiceConfig{
		"svc": {
			BaseURL: "https://svc.example.com",
			TLS:     &TLSConfig{MinVersion: "1.3", InsecureSkipVerify: &skip},
			Endpoints: map[string]EndpointConfig{
				"getX": {Method: "GET", Path: "/x"},
			},
		},
	}}
	c, err := New(cfg, WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	svc := c.snap().services["svc"]
	_, _, cert := genCertPairFiles(t)
	clone, err := cloneServiceWithClientCert(svc, cert)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	if clone.tlsConfig.MinVersion != tls.VersionTLS13 {
		t.Error("clone should inherit the source MinVersion")
	}
	if !clone.tlsConfig.InsecureSkipVerify {
		t.Error("clone should inherit InsecureSkipVerify")
	}
	if len(clone.tlsConfig.Certificates) != 1 {
		t.Error("clone should carry the injected cert")
	}
}

func TestCloneServiceWithClientCert_Skeleton(t *testing.T) {
	skeleton := &serviceClient{name: "bare"}
	if _, err := cloneServiceWithClientCert(skeleton, tls.Certificate{}); err == nil {
		t.Error("skeleton client without transport should error")
	}
}
