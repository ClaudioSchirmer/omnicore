package auth

import (
	"fmt"
	"net/http"
)

// HeaderStaticProvider attaches a raw header (or query/cookie) value from
// configuration. Typical use: API keys (X-API-Key: secret). The value is
// pre-rendered at boot from the YAML (no per-request work beyond Attach).
type HeaderStaticProvider struct {
	name   string
	attach AttachConfig
}

// NewHeaderStaticProvider validates the attach block (name + value
// required) and returns a ready provider.
func NewHeaderStaticProvider(name string, attach AttachConfig) (*HeaderStaticProvider, error) {
	if attach.Name == "" {
		return nil, fmt.Errorf("auth: header-static %q requires attach.name", name)
	}
	if attach.Value == "" {
		return nil, fmt.Errorf("auth: header-static %q requires attach.value", name)
	}
	return &HeaderStaticProvider{name: name, attach: attach}, nil
}

func (p *HeaderStaticProvider) Name() string { return p.name }

func (p *HeaderStaticProvider) Apply(req *http.Request) error {
	Attach(req, p.attach, p.attach.Value)
	return nil
}

// BearerStaticProvider attaches a fixed bearer token via the configured
// attach. The default attach is Authorization: Bearer {token}; operators
// can override (e.g. emit the token raw without the "Bearer " prefix).
type BearerStaticProvider struct {
	name   string
	token  string
	attach AttachConfig
}

// NewBearerStaticProvider validates the token and applies the design
// defaults to attach when the YAML omits them: header Authorization with
// format "Bearer {token}".
func NewBearerStaticProvider(name, token string, attach AttachConfig) (*BearerStaticProvider, error) {
	if token == "" {
		return nil, fmt.Errorf("auth: bearer-static %q requires token", name)
	}
	if attach.Kind == AttachUnknown {
		attach.Kind = AttachHeader
	}
	if attach.Name == "" {
		attach.Name = "Authorization"
	}
	if attach.Format == "" {
		attach.Format = "Bearer {token}"
	}
	return &BearerStaticProvider{name: name, token: token, attach: attach}, nil
}

func (p *BearerStaticProvider) Name() string { return p.name }

func (p *BearerStaticProvider) Apply(req *http.Request) error {
	Attach(req, p.attach, RenderValue(p.attach.Format, p.token))
	return nil
}
