package auth

import (
	"fmt"
	"net/http"
)

// ForwardBearerProvider propagates the inbound JWT from the AppContext to
// the downstream call. The token is read from the request's context, which
// the call surface populates from the AppContext passed into Call. When
// the AppContext carries no bearer (public route, auth disabled,
// background job), Apply returns an error and the auth middleware wraps
// it in ErrTokenAcquire.
//
// The token is intentionally never cached — it IS the inbound user's
// credential, and caching would require keying by a hash of the token,
// risking cross-user contamination. The design Section 6 explicitly
// states: "Forward-bearer: never cached."
type ForwardBearerProvider struct {
	name   string
	attach AttachConfig
}

// NewForwardBearerProvider builds the provider applying the design
// defaults to attach: header Authorization with format "Bearer {token}".
func NewForwardBearerProvider(name string, attach AttachConfig) *ForwardBearerProvider {
	if attach.Kind == AttachUnknown {
		attach.Kind = AttachHeader
	}
	if attach.Name == "" {
		attach.Name = "Authorization"
	}
	if attach.Format == "" {
		attach.Format = "Bearer {token}"
	}
	return &ForwardBearerProvider{name: name, attach: attach}
}

func (p *ForwardBearerProvider) Name() string { return p.name }

func (p *ForwardBearerProvider) Apply(req *http.Request) error {
	carrier, ok := req.Context().(bearerCarrier)
	if !ok {
		return fmt.Errorf("auth: forward-bearer requires AppContext (got %T)", req.Context())
	}
	token := carrier.BearerToken()
	if token == "" {
		return fmt.Errorf("auth: forward-bearer: no bearer on AppContext (public route / auth disabled / background job)")
	}
	Attach(req, p.attach, RenderValue(p.attach.Format, token))
	return nil
}

// bearerCarrier is the minimal interface forward-bearer reads from the
// request context. AppContext implements it via BearerToken() (added by
// the AppContext.BearerToken phase).
type bearerCarrier interface {
	BearerToken() string
}
