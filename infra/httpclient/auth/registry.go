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

// Has reports whether a provider is registered under the given name.
func (r *Registry) Has(name string) bool {
	if r == nil {
		return false
	}
	_, ok := r.providers[name]
	return ok
}

// Remove deletes the provider registered under name, if any. Used by the
// httpclient's Unregister to drop a runtime-registered provider once its
// owning service is gone.
func (r *Registry) Remove(name string) {
	if r == nil {
		return
	}
	delete(r.providers, name)
}

// Clone returns a new registry holding the same provider instances under the
// same names. Used for the httpclient's copy-on-write registry swaps: the
// clone can be mutated (Register / Remove) without disturbing the published
// snapshot. Provider instances are shared, so warm token caches survive.
func (r *Registry) Clone() *Registry {
	if r == nil {
		return &Registry{providers: map[string]AuthProvider{}}
	}
	nr := &Registry{providers: make(map[string]AuthProvider, len(r.providers)+1)}
	for k, v := range r.providers {
		nr.providers[k] = v
	}
	return nr
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
