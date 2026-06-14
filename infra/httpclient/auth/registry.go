package auth

import "fmt"

// Registry holds named providers built from the YAML at boot. Lookup is by
// the same names declared in httpClient.authProviders.
type Registry struct {
	providers map[string]AuthProvider
}

// NewRegistry creates an empty registry; callers populate it via Register.
func NewRegistry() *Registry {
	return &Registry{providers: map[string]AuthProvider{}}
}

// Register adds a provider under the given name. Duplicate names overwrite
// (the validator rejects duplicates from YAML upstream, so this only fires
// in tests that synthesize providers).
func (r *Registry) Register(name string, p AuthProvider) {
	r.providers[name] = p
}

// Lookup returns the named provider or an error when absent. Used by the
// middleware (per service) and by WithAuthOverride (per call).
func (r *Registry) Lookup(name string) (AuthProvider, error) {
	if r == nil {
		return nil, fmt.Errorf("auth: registry not initialized")
	}
	p, ok := r.providers[name]
	if !ok {
		return nil, fmt.Errorf("auth: provider %q is not registered", name)
	}
	return p, nil
}

// Len reports the number of registered providers (test helper).
func (r *Registry) Len() int {
	if r == nil {
		return 0
	}
	return len(r.providers)
}
