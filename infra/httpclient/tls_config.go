package httpclient

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"strings"
	"time"
)

// TLSConfig is the YAML shape for tls: under defaults and services. The
// cascade merges service over defaults field by field. The only framework
// default is minVersion = TLS 1.2; cipherSuites are left to Go's stdlib
// selection unless a preset or explicit list is set — imposing the
// TLS 1.3-only "modern" preset by default would leave TLS 1.2 handshakes
// with no usable suites (Go ignores CipherSuites for TLS 1.3).
type TLSConfig struct {
	MinVersion         string   `yaml:"minVersion"`
	CipherSuites       []string `yaml:"cipherSuites"`
	InsecureSkipVerify *bool    `yaml:"insecureSkipVerify"`
	ClientCertFile     string   `yaml:"clientCertFile"`
	ClientKeyFile      string   `yaml:"clientKeyFile"`
	CABundle           string   `yaml:"caBundle"`
}

// PoolConfig is the YAML shape for pool: under defaults and services. All
// fields override per-service when set; absent fields fall back to the
// framework defaults already defined in service_client.go.
type PoolConfig struct {
	MaxIdleConnsPerHost int      `yaml:"maxIdleConnsPerHost"`
	MaxConnsPerHost     int      `yaml:"maxConnsPerHost"`
	IdleConnTimeout     Duration `yaml:"idleConnTimeout"`
	DisableKeepAlives   *bool    `yaml:"disableKeepAlives"`
}

// cipherSuitePresets are the canonical preset cipher lists. Names follow
// Mozilla's recommended SSL configurations (modern/intermediate/legacy).
// A preset applies only when explicitly selected (resolveTLSConfig leaves
// cfg.CipherSuites nil otherwise); "modern" is the TLS 1.3 AEAD suites.
var cipherSuitePresets = map[string][]uint16{
	"modern": {
		tls.TLS_AES_128_GCM_SHA256,
		tls.TLS_AES_256_GCM_SHA384,
		tls.TLS_CHACHA20_POLY1305_SHA256,
	},
	"intermediate": {
		tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,
		tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,
	},
	"legacy": {
		tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA,
		tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
		tls.TLS_RSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_RSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_RSA_WITH_AES_128_CBC_SHA,
	},
}

// explicitCipherNames maps the upper-case canonical name to the Go
// constant. Operators can list these directly when no preset fits.
var explicitCipherNames = map[string]uint16{
	"TLS_AES_128_GCM_SHA256":                  tls.TLS_AES_128_GCM_SHA256,
	"TLS_AES_256_GCM_SHA384":                  tls.TLS_AES_256_GCM_SHA384,
	"TLS_CHACHA20_POLY1305_SHA256":            tls.TLS_CHACHA20_POLY1305_SHA256,
	"TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256": tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
	"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256":   tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
	"TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384": tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
	"TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384":   tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
	"TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305":  tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,
	"TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305":    tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,
	"TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA":    tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA,
	"TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA":      tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
	"TLS_RSA_WITH_AES_128_GCM_SHA256":         tls.TLS_RSA_WITH_AES_128_GCM_SHA256,
	"TLS_RSA_WITH_AES_256_GCM_SHA384":         tls.TLS_RSA_WITH_AES_256_GCM_SHA384,
	"TLS_RSA_WITH_AES_128_CBC_SHA":            tls.TLS_RSA_WITH_AES_128_CBC_SHA,
}

// supportedTLSVersions maps the YAML scalar to the constant from
// crypto/tls. Empty input means "use the framework default" — TLS 1.2.
var supportedTLSVersions = map[string]uint16{
	"1.0": tls.VersionTLS10,
	"1.1": tls.VersionTLS11,
	"1.2": tls.VersionTLS12,
	"1.3": tls.VersionTLS13,
}

// resolveTLSConfig merges defaults + service into a runtime *tls.Config.
// Returns nil when neither input is set so the http.Transport can stick
// with the stdlib defaults. mTLS files are loaded once at boot.
func resolveTLSConfig(defaults, service *TLSConfig) (*tls.Config, error) {
	merged := mergeTLSConfig(defaults, service)
	if merged == nil {
		return nil, nil
	}
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
	}
	if merged.MinVersion != "" {
		v, ok := supportedTLSVersions[merged.MinVersion]
		if !ok {
			return nil, fmt.Errorf("tls: minVersion %q is not one of 1.0|1.1|1.2|1.3", merged.MinVersion)
		}
		cfg.MinVersion = v
	}
	if merged.InsecureSkipVerify != nil {
		cfg.InsecureSkipVerify = *merged.InsecureSkipVerify
	}
	if len(merged.CipherSuites) > 0 {
		suites, err := resolveCipherSuites(merged.CipherSuites)
		if err != nil {
			return nil, err
		}
		cfg.CipherSuites = suites
	}
	if merged.ClientCertFile != "" || merged.ClientKeyFile != "" {
		cert, err := loadCertPair(merged.ClientCertFile, merged.ClientKeyFile)
		if err != nil {
			return nil, err
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	if merged.CABundle != "" {
		pool, err := loadCABundle(merged.CABundle)
		if err != nil {
			return nil, err
		}
		cfg.RootCAs = pool
	}
	return cfg, nil
}

// mergeTLSConfig returns a fresh TLSConfig with service overriding
// defaults field by field. Returns nil when both inputs are nil.
func mergeTLSConfig(defaults, service *TLSConfig) *TLSConfig {
	if defaults == nil && service == nil {
		return nil
	}
	out := &TLSConfig{}
	if defaults != nil {
		*out = *defaults
		if defaults.CipherSuites != nil {
			out.CipherSuites = append([]string(nil), defaults.CipherSuites...)
		}
	}
	if service != nil {
		if service.MinVersion != "" {
			out.MinVersion = service.MinVersion
		}
		if service.CipherSuites != nil {
			out.CipherSuites = append([]string(nil), service.CipherSuites...)
		}
		if service.InsecureSkipVerify != nil {
			v := *service.InsecureSkipVerify
			out.InsecureSkipVerify = &v
		}
		if service.ClientCertFile != "" {
			out.ClientCertFile = service.ClientCertFile
		}
		if service.ClientKeyFile != "" {
			out.ClientKeyFile = service.ClientKeyFile
		}
		if service.CABundle != "" {
			out.CABundle = service.CABundle
		}
	}
	return out
}

// resolveCipherSuites translates either a preset name or an explicit list
// of cipher names into the []uint16 form Go's tls package expects.
func resolveCipherSuites(input []string) ([]uint16, error) {
	if len(input) == 0 {
		return nil, nil
	}
	if len(input) == 1 {
		if preset, ok := cipherSuitePresets[strings.ToLower(input[0])]; ok {
			out := make([]uint16, len(preset))
			copy(out, preset)
			return out, nil
		}
	}
	out := make([]uint16, 0, len(input))
	for _, raw := range input {
		name := strings.ToUpper(strings.TrimSpace(raw))
		v, ok := explicitCipherNames[name]
		if !ok {
			return nil, fmt.Errorf("tls: unknown cipher suite %q", raw)
		}
		out = append(out, v)
	}
	return out, nil
}

// loadCertPair loads a PEM-encoded certificate + private key pair from
// disk. Both files are required; absence of either is a config error.
func loadCertPair(certFile, keyFile string) (tls.Certificate, error) {
	if certFile == "" || keyFile == "" {
		return tls.Certificate{}, fmt.Errorf("tls: clientCertFile and clientKeyFile must both be set")
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("tls: load cert pair: %w", err)
	}
	return cert, nil
}

// loadCABundle reads a PEM-encoded CA bundle and returns a pool. The
// returned pool replaces (not merges with) the system roots — operators
// who need both should concatenate the bundles in the file.
func loadCABundle(path string) (*x509.CertPool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("tls: read caBundle %q: %w", path, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(raw) {
		return nil, fmt.Errorf("tls: caBundle %q contains no usable certificates", path)
	}
	return pool, nil
}

// resolvePoolConfig merges defaults + service into the http.Transport
// pool knobs. Returns the framework defaults when nothing is configured.
func resolvePoolConfig(defaults, service *PoolConfig) (maxIdle, maxConns int, idleTimeout time.Duration, disableKeepAlives bool) {
	maxIdle = defaultMaxIdleConnsPerHost
	maxConns = defaultMaxConnsPerHost
	idleTimeout = defaultIdleConnTimeout
	disableKeepAlives = false
	apply := func(cfg *PoolConfig) {
		if cfg == nil {
			return
		}
		if cfg.MaxIdleConnsPerHost > 0 {
			maxIdle = cfg.MaxIdleConnsPerHost
		}
		if cfg.MaxConnsPerHost > 0 {
			maxConns = cfg.MaxConnsPerHost
		}
		if cfg.IdleConnTimeout > 0 {
			idleTimeout = cfg.IdleConnTimeout.ToTime()
		}
		if cfg.DisableKeepAlives != nil {
			disableKeepAlives = *cfg.DisableKeepAlives
		}
	}
	apply(defaults)
	apply(service)
	return
}

// validateTLSConfig runs schema checks on a TLS block.
func validateTLSConfig(prefix string, cfg *TLSConfig) []string {
	if cfg == nil {
		return nil
	}
	var errs []string
	if cfg.MinVersion != "" {
		if _, ok := supportedTLSVersions[cfg.MinVersion]; !ok {
			errs = append(errs, fmt.Sprintf("%s.minVersion: %q is not one of 1.0|1.1|1.2|1.3", prefix, cfg.MinVersion))
		}
	}
	if len(cfg.CipherSuites) > 0 {
		if _, err := resolveCipherSuites(cfg.CipherSuites); err != nil {
			errs = append(errs, fmt.Sprintf("%s.cipherSuites: %v", prefix, err))
		}
	}
	if cfg.ClientCertFile != "" && cfg.ClientKeyFile == "" {
		errs = append(errs, fmt.Sprintf("%s.clientKeyFile: required when clientCertFile is set", prefix))
	}
	if cfg.ClientKeyFile != "" && cfg.ClientCertFile == "" {
		errs = append(errs, fmt.Sprintf("%s.clientCertFile: required when clientKeyFile is set", prefix))
	}
	return errs
}

// validatePoolConfig runs schema checks on a pool block.
func validatePoolConfig(prefix string, cfg *PoolConfig) []string {
	if cfg == nil {
		return nil
	}
	var errs []string
	if cfg.MaxIdleConnsPerHost < 0 {
		errs = append(errs, fmt.Sprintf("%s.maxIdleConnsPerHost: must be non-negative", prefix))
	}
	if cfg.MaxConnsPerHost < 0 {
		errs = append(errs, fmt.Sprintf("%s.maxConnsPerHost: must be non-negative", prefix))
	}
	if cfg.IdleConnTimeout < 0 {
		errs = append(errs, fmt.Sprintf("%s.idleConnTimeout: must be non-negative", prefix))
	}
	return errs
}
