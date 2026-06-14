package bootstrap

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	configPathEnv = "OMNICORE_CONFIG_PATH"
	profileEnv    = "APP_PROFILE"
	profileDev    = "dev"
	profilePrd    = "prd"
)

// LoadConfig reads APP_PROFILE from the environment (required, non-empty),
// loads the matching microservice.<profile>.yaml (overridable via
// $OMNICORE_CONFIG_PATH), parses it via LoadConfigFrom, and applies
// profile-aware guards — currently: auth.mode=disabled is rejected under any
// profile other than "dev" so prd cannot ship without authentication wired.
//
// The profile name is free-form: "dev" and "prd" are the canonical pair, but
// any non-empty string is accepted. Extra variants (e.g., "prd-pem",
// "prd-external", "qa-canary") allow QA suites and ops setups to swap whole
// configurations via APP_PROFILE without competing with the canonical pair.
// "dev" remains the only profile under which auth.mode=disabled is allowed.
//
// The profile is intentionally NOT a YAML field — the same artifact must not
// be able to drift between profiles, and the file selection itself ties config
// to environment by shape (separate files per profile).
func LoadConfig() (*Config, error) {
	profile := os.Getenv(profileEnv)
	if profile == "" {
		return nil, fmt.Errorf("bootstrap: %s env var is required", profileEnv)
	}
	path := os.Getenv(configPathEnv)
	if path == "" {
		path = fmt.Sprintf("./microservice.%s.yaml", profile)
	}
	cfg, err := LoadConfigFrom(path)
	if err != nil {
		return nil, err
	}
	cfg.Profile = profile
	cfg.applyProfileDefaults(profile)
	if err := cfg.validateForProfile(profile); err != nil {
		return nil, err
	}
	return cfg, nil
}

// LoadConfigFrom parses the YAML at the given path, applies defaults, and
// runs schema validation. It does NOT set Profile and does NOT enforce
// profile-aware guards — callers that need those go through LoadConfig.
func LoadConfigFrom(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("bootstrap: read config %q: %w", path, err)
	}
	interpolated, err := interpolate(string(raw))
	if err != nil {
		return nil, fmt.Errorf("bootstrap: interpolate %q: %w", path, err)
	}
	var cfg Config
	if err := yaml.Unmarshal([]byte(interpolated), &cfg); err != nil {
		return nil, fmt.Errorf("bootstrap: parse yaml %q: %w", path, err)
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// interpolationRE captures ${NAME} or ${NAME:arg}. NAME is an env var name
// (uppercase convention) OR one of the two reserved prefixes "file" / "vault"
// — see interpolate. The arg group may contain any char except the closing
// brace, which keeps DSN strings, URLs, and absolute paths legible inline.
var interpolationRE = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::([^}]*))?\}`)

// interpolate substitutes ${...} references in the input. Three forms:
//
//   - ${VAR} / ${VAR:default} — environment variable. The value wins when set
//     and non-empty; otherwise the default after `:` is used; absent default →
//     empty string. Env misses are intentionally silent (dev defaults).
//
//   - ${file:/abs/path} — file contents read once at boot. A trailing \n or
//     \r\n is trimmed; everything else is preserved verbatim so PEM blocks
//     and similar multi-line payloads round-trip. Missing file or any I/O
//     failure aborts the boot.
//
//   - ${vault:store/path#field} — delegated to the registered SecretResolver.
//     Without a registered resolver the default returns ErrUnsupportedResolver
//     and the boot aborts. Plug a real implementation via
//     RegisterSecretResolver.
//
// The reserved names "file" and "vault" never collide with env vars in
// practice because env-var conventions are uppercase; their lowercase
// counterparts are reserved by this loader.
func interpolate(input string) (string, error) {
	var firstErr error
	result := interpolationRE.ReplaceAllStringFunc(input, func(match string) string {
		if firstErr != nil {
			return ""
		}
		sub := interpolationRE.FindStringSubmatch(match)
		name, arg := sub[1], sub[2]
		switch name {
		case "file":
			content, err := readSecretFile(arg)
			if err != nil {
				firstErr = err
				return ""
			}
			return content
		case "vault":
			content, err := currentSecretResolver().ResolveSecret(arg)
			if err != nil {
				firstErr = fmt.Errorf("resolve ${vault:%s}: %w", arg, err)
				return ""
			}
			return content
		default:
			if v, ok := os.LookupEnv(name); ok && v != "" {
				return v
			}
			return arg
		}
	})
	if firstErr != nil {
		return "", firstErr
	}
	return result, nil
}

// readSecretFile reads the file at path and returns its contents with a
// single trailing \n or \r\n trimmed. Internal newlines are preserved (PEM
// blocks, multi-line tokens) — only the optional EOF newline is dropped so
// `cat secret-file` style fixtures don't bleed whitespace into config values.
func readSecretFile(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read ${file:%s}: %w", path, err)
	}
	s := string(raw)
	s = strings.TrimSuffix(s, "\n")
	s = strings.TrimSuffix(s, "\r")
	return s, nil
}
