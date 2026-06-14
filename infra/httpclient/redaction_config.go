package httpclient

import (
	"fmt"
	"net/textproto"
	"strings"
)

// frameworkRedactedQueryKeys are the default query parameters whose values
// the framework masks in the slog observation's url field. The list is the
// canonical "tokens in URLs" surface that operators expect blocked out of
// the box.
var frameworkRedactedQueryKeys = []string{
	"token",
	"api_key",
	"access_token",
	"signature",
	"code",
}

// RedactionConfig is the YAML shape for redaction: under defaults and
// services. Per-service blocks extend (do not replace) defaults; the
// framework block lists always apply as the base.
type RedactionConfig struct {
	Headers      []string `yaml:"headers"`
	BodyJSONPath []string `yaml:"bodyJSONPath"`
	QueryKeys    []string `yaml:"queryKeys"`
}

// redactionPolicy is the resolved runtime cascade consumed by the
// logging middleware. Sets are pre-built once per service so the request
// path performs O(1) lookups instead of slice scans.
type redactionPolicy struct {
	headerSet     map[string]struct{}
	bodyJSONPaths []string
	queryKeys     map[string]struct{}
}

// resolveRedactionPolicy unions the framework defaults, the defaults YAML
// block, and the per-service YAML block. Per-service entries can never
// remove a redaction — they only add more.
func resolveRedactionPolicy(defaults, service *RedactionConfig) redactionPolicy {
	policy := redactionPolicy{
		headerSet: make(map[string]struct{}, len(defaultRedactedHeaders)+8),
		queryKeys: make(map[string]struct{}, len(frameworkRedactedQueryKeys)+8),
	}
	for k := range defaultRedactedHeaders {
		policy.headerSet[k] = struct{}{}
	}
	for _, k := range frameworkRedactedQueryKeys {
		policy.queryKeys[strings.ToLower(k)] = struct{}{}
	}
	merge := func(cfg *RedactionConfig) {
		if cfg == nil {
			return
		}
		for _, h := range cfg.Headers {
			h = strings.TrimSpace(h)
			if h == "" {
				continue
			}
			policy.headerSet[textproto.CanonicalMIMEHeaderKey(h)] = struct{}{}
		}
		for _, p := range cfg.BodyJSONPath {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			policy.bodyJSONPaths = append(policy.bodyJSONPaths, p)
		}
		for _, k := range cfg.QueryKeys {
			k = strings.TrimSpace(k)
			if k == "" {
				continue
			}
			policy.queryKeys[strings.ToLower(k)] = struct{}{}
		}
	}
	merge(defaults)
	merge(service)
	return policy
}

// validateRedactionConfig runs schema checks on a redaction block.
// Catches empty entries and obviously malformed JSONPaths.
func validateRedactionConfig(prefix string, cfg *RedactionConfig) []string {
	if cfg == nil {
		return nil
	}
	var errs []string
	for i, h := range cfg.Headers {
		if strings.TrimSpace(h) == "" {
			errs = append(errs, fmt.Sprintf("%s.headers[%d]: empty string", prefix, i))
		}
	}
	for i, p := range cfg.BodyJSONPath {
		trim := strings.TrimSpace(p)
		if trim == "" {
			errs = append(errs, fmt.Sprintf("%s.bodyJSONPath[%d]: empty string", prefix, i))
			continue
		}
		if !strings.HasPrefix(trim, "$") {
			errs = append(errs, fmt.Sprintf("%s.bodyJSONPath[%d]: %q must start with $", prefix, i, p))
		}
	}
	for i, k := range cfg.QueryKeys {
		if strings.TrimSpace(k) == "" {
			errs = append(errs, fmt.Sprintf("%s.queryKeys[%d]: empty string", prefix, i))
		}
	}
	return errs
}
