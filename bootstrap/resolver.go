package bootstrap

import "errors"

// ErrUnsupportedResolver is returned by the default SecretResolver. Plug a
// real implementation (HashiCorp Vault, AWS Secrets Manager, etc.) via
// RegisterSecretResolver to enable ${vault:...} references in the YAML.
var ErrUnsupportedResolver = errors.New("bootstrap: vault: prefix has no resolver registered")

// SecretResolver resolves ${vault:<path>} references in microservice.<profile>.yaml.
// Implementations fetch the secret from an external store at boot. The path is
// everything after the literal "vault:" prefix; conventional shape is
// "store/path#field" (the field after '#' is the key inside the secret), but
// the exact interpretation is left to the implementation.
//
// Implementations are called once per ${vault:...} occurrence during
// LoadConfigFrom. An error aborts the boot — partial configs never reach the
// service.
type SecretResolver interface {
	ResolveSecret(path string) (string, error)
}

// defaultSecretResolver is the no-op resolver installed at process start. It
// rejects every ${vault:...} reference with ErrUnsupportedResolver so the
// failure surfaces at boot rather than silently producing an empty secret.
type defaultSecretResolver struct{}

func (defaultSecretResolver) ResolveSecret(string) (string, error) {
	return "", ErrUnsupportedResolver
}

// secretResolver holds the active resolver. Not guarded by a mutex on
// purpose: RegisterSecretResolver is meant to be called once at process init
// (before any LoadConfig), matching the singleton lifecycle of other
// framework registries.
var secretResolver SecretResolver = defaultSecretResolver{}

// RegisterSecretResolver swaps the resolver consumed by LoadConfigFrom when
// it interpolates a ${vault:...} reference. Pass nil to revert to the default
// (every ${vault:...} fails with ErrUnsupportedResolver).
//
// Process-global; call once at process init, before LoadConfig.
func RegisterSecretResolver(r SecretResolver) {
	if r == nil {
		secretResolver = defaultSecretResolver{}
		return
	}
	secretResolver = r
}

// currentSecretResolver returns the resolver in effect. Used by interpolate.
func currentSecretResolver() SecretResolver {
	return secretResolver
}
