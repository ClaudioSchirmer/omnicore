package auth

import (
	"encoding/base64"
	"fmt"
	"net/http"
)

// BasicProvider implements RFC 7617 Basic. The base64-encoded credential
// is computed once at boot — the request path only applies the rendered
// value via Attach.
type BasicProvider struct {
	name    string
	encoded string
	attach  AttachConfig
}

// NewBasicProvider validates username + password and computes the
// "Basic <b64(user:pass)>" value. attach defaults to header Authorization
// with no format override (the encoded value already contains the scheme).
func NewBasicProvider(name, username, password string, attach AttachConfig) (*BasicProvider, error) {
	if username == "" || password == "" {
		return nil, fmt.Errorf("auth: basic %q requires username and password", name)
	}
	if attach.Kind == AttachUnknown {
		attach.Kind = AttachHeader
	}
	if attach.Name == "" {
		attach.Name = "Authorization"
	}
	encoded := "Basic " + base64.StdEncoding.EncodeToString([]byte(username+":"+password))
	return &BasicProvider{name: name, encoded: encoded, attach: attach}, nil
}

func (p *BasicProvider) Name() string { return p.name }

func (p *BasicProvider) Apply(req *http.Request) error {
	Attach(req, p.attach, p.encoded)
	return nil
}
