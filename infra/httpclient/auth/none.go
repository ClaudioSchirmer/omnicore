package auth

import "net/http"

// NoneProvider is the no-op provider. Used when the TLS layer handles
// identity (mTLS) or when the call is intentionally anonymous.
type NoneProvider struct {
	name string
}

// NewNoneProvider builds a no-op provider with the supplied name.
func NewNoneProvider(name string) *NoneProvider {
	return &NoneProvider{name: name}
}

func (p *NoneProvider) Name() string                 { return p.name }
func (p *NoneProvider) Apply(req *http.Request) error { return nil }
