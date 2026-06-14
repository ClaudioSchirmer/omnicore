package auth

// RevocableProvider is implemented by providers whose credential can be
// invalidated mid-session. The auth middleware type-asserts against this
// interface on 401 responses (when revocationOnUnauthorized is opted in)
// to clear cached state and re-acquire a fresh credential.
//
// Static providers (header-static, bearer-static, basic, none) do NOT
// implement this — their credential is fixed at boot and a 401 from the
// upstream means the YAML is wrong, not that the cached value is stale.
type RevocableProvider interface {
	AuthProvider
	Invalidate()
}
