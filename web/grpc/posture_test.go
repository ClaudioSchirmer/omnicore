package grpc

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/golang-jwt/jwt/v5"
	"google.golang.org/protobuf/proto"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/internal/testpb"
)

// internalRegistry builds a registry in the given posture with an
// attribution validator sharing the keypair of the signer.
func internalRegistry(t *testing.T, posture AuthPosture) (*Registry, func(claims jwt.MapClaims) string) {
	t.Helper()
	reg, sign := authedRegistry(t, AuthPolicy{}) // arms the INHERIT validator (unused in internal branch)
	reg.SetAuthPosture(posture, reg.auth)        // reuse the same keypair-backed validator for attribution
	return reg, sign
}

func TestInternalAnonymousPassesEvenWithPermissionGate(t *testing.T) {
	reg, _ := internalRegistry(t, PostureInternal)
	reg.EnableAuthorization(true)
	call := registerServer(t, reg, RequirePermission("gadgets:write"))
	// no bearer at all — trusted plane, gates pass (flow designer's call)
	res, err := call(connect.NewRequest(&testpb.CreateGadgetRequest{Name: proto.String("x")}))
	if err != nil || res.Msg.GetId() != "g-1" {
		t.Fatalf("anonymous internal must pass gate: res=%v err=%v", res, err)
	}
}

func TestInternalForwardedValidUserIsEvaluated(t *testing.T) {
	reg, sign := internalRegistry(t, PostureInternal)
	reg.EnableAuthorization(true)
	p := &probe{}
	callP := newChainServerWithGate(t, reg, p, "gadgets:write")

	// user WITH the permission → passes, identity populated
	granted := baseClaims()
	granted["permissions"] = []string{"gadgets:write"}
	req := emptyReq()
	req.Header().Set("Authorization", "Bearer "+sign(granted))
	if _, err := callP(req); err != nil {
		t.Fatalf("granted forwarded user: %v", err)
	}
	if p.appCtx.Identity() == nil || p.appCtx.Identity().Subject != "user-9" {
		t.Fatalf("forwarded identity missing: %+v", p.appCtx.Identity())
	}

	// user WITHOUT the permission → gate denies (forwarded user is evaluated)
	denied := baseClaims()
	denied["permissions"] = []string{"gadgets:read"}
	req = emptyReq()
	req.Header().Set("Authorization", "Bearer "+sign(denied))
	_, err := callP(req)
	var cerr *connect.Error
	if !errors.As(err, &cerr) || cerr.Code() != connect.CodePermissionDenied {
		t.Fatalf("forwarded user without permission must be denied: %v", err)
	}
}

func TestInternalExpiredAuthenticPassesWithStaleMark(t *testing.T) {
	reg, sign := internalRegistry(t, PostureInternal)
	p := &probe{}
	call := newChainServerWithGate(t, reg, p, "")

	expired := baseClaims()
	expired["exp"] = time.Now().Add(-2 * time.Hour).Unix()
	req := emptyReq()
	req.Header().Set("Authorization", "Bearer "+sign(expired))
	if _, err := call(req); err != nil {
		t.Fatalf("expired-authentic must pass on the internal plane: %v", err)
	}
	id := p.appCtx.Identity()
	if id == nil || id.Subject != "user-9" {
		t.Fatalf("attribution identity missing: %+v", id)
	}
	if id.Claims[attributionStaleClaim] != attributionStaleValue {
		t.Fatalf("stale mark missing: %v", id.Claims)
	}
}

func TestInternalForgedRejected(t *testing.T) {
	reg, _ := internalRegistry(t, PostureInternal)
	call := newChainServerWithGate(t, reg, nil, "")
	req := emptyReq()
	req.Header().Set("Authorization", "Bearer forged.garbage.token")
	_, err := call(req)
	var cerr *connect.Error
	if !errors.As(err, &cerr) || cerr.Code() != connect.CodeUnauthenticated {
		t.Fatalf("forged attribution must reject: %v", err)
	}
}

// TestSideBySideExpiredToken proves the structural guarantee: the SAME
// expired token rejects at the main door (inherit) and passes with
// attribution on the internal plane.
func TestSideBySideExpiredToken(t *testing.T) {
	expired := baseClaims()
	expired["exp"] = time.Now().Add(-time.Hour).Unix()

	// main door (inherit): the expired token rejects
	edgeReg, edgeSign := authedRegistry(t, AuthPolicy{})
	edgeCall := newChainServer(t, edgeReg, nil, nil)
	req := emptyReq()
	req.Header().Set("Authorization", "Bearer "+edgeSign(expired))
	_, err := edgeCall(req)
	assertAuthRejection(t, err, connect.CodeUnauthenticated, "ExpiredTokenNotification")

	// internal plane: the same claims, signed by that plane's IdP keypair,
	// pass with attribution (each test registry gets its own keypair)
	internalReg, internalSign := internalRegistry(t, PostureInternal)
	internalCall := newChainServerWithGate(t, internalReg, nil, "")
	req = emptyReq()
	req.Header().Set("Authorization", "Bearer "+internalSign(expired))
	if _, err := internalCall(req); err != nil {
		t.Fatalf("internal plane must accept the expired-authentic token: %v", err)
	}
}

func TestInternalWithoutAttributionValidatorIsAnonymous(t *testing.T) {
	// global auth disabled (dev): no JWT material — a bearer cannot be
	// verified, so the call proceeds anonymous instead of failing.
	reg := New(pipeline.New(nil))
	reg.SetAuthPosture(PostureInternal, nil)
	p := &probe{}
	call := newChainServerWithGate(t, reg, p, "")
	req := emptyReq()
	req.Header().Set("Authorization", "Bearer whatever")
	if _, err := call(req); err != nil {
		t.Fatalf("dev internal must pass: %v", err)
	}
	if p.appCtx.Identity() != nil {
		t.Fatalf("nothing verifiable must not attribute")
	}
}

func TestMTLSAnonymousCarriesCertIdentity(t *testing.T) {
	reg := New(pipeline.New(nil))
	reg.SetAuthPosture(PostureMTLS, nil)
	p := &probe{}

	fn := func(ctx context.Context, req *connect.Request[testpb.CreateGadgetRequest]) (*connect.Response[testpb.CreateGadgetResponse], error) {
		p.appCtx = AppContextFrom(ctx)
		return connect.NewResponse(&testpb.CreateGadgetResponse{Id: "ok"}), nil
	}
	h := connect.NewUnaryHandler(testProcedure, fn, reg.HandlerOptions()...)
	reg.mountProcedure(testProcedure, h)

	// simulate the TLS layer: inject a verified peer certificate the way
	// bootstrap's WithClientCertIdentity wrapper sees it
	wrapped := WithClientCertIdentity(reg.Handler())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{{
			Subject:  pkix.Name{CommonName: "fallback-cn"},
			DNSNames: []string{"service-a"},
		}}}
		wrapped.ServeHTTP(w, req)
	}))
	t.Cleanup(srv.Close)

	client := connect.NewClient[testpb.CreateGadgetRequest, testpb.CreateGadgetResponse](srv.Client(), srv.URL+testProcedure)
	if _, err := client.CallUnary(context.Background(), emptyReq()); err != nil {
		t.Fatalf("mtls anonymous call: %v", err)
	}
	id := p.appCtx.Identity()
	if id == nil || id.Subject != "service-a" {
		t.Fatalf("cert identity missing: %+v", id)
	}
	if id.Claims[attributionStaleClaim] != attributionMTLSValue {
		t.Fatalf("mtls attribution mark missing: %v", id.Claims)
	}
}

// probeCmdHandler records the AppContext the wrapper dispatched with, then
// delegates — so posture tests can assert Identity/claims end to end.
type probeCmdHandler struct {
	p     *probe
	inner *createGadgetHandler
}

func (h *probeCmdHandler) Handle(ctx *configuration.AppContext, cmd *createGadgetCommand) (*gadgetResult, error) {
	if h.p != nil {
		h.p.appCtx = ctx
	}
	return h.inner.Handle(ctx, cmd)
}

// newChainServerWithGate mounts the probe RPC with an optional permission
// through the full chain — the posture-aware sibling of newChainServer.
func newChainServerWithGate(t *testing.T, reg *Registry, p *probe, permission string) func(req *connect.Request[testpb.CreateGadgetRequest]) (*connect.Response[testpb.CreateGadgetResponse], error) {
	t.Helper()
	var opts []ProcedureOption
	if permission != "" {
		opts = append(opts, RequirePermission(permission))
	}
	reg.Register(CommandWithBody(testProcedure,
		toCreateCommand,
		fromGadgetResult,
		&probeCmdHandler{p: p, inner: &createGadgetHandler{}},
		opts...,
	))
	srv := httptest.NewServer(reg.Handler())
	t.Cleanup(srv.Close)
	client := connect.NewClient[testpb.CreateGadgetRequest, testpb.CreateGadgetResponse](srv.Client(), srv.URL+testProcedure)
	return func(req *connect.Request[testpb.CreateGadgetRequest]) (*connect.Response[testpb.CreateGadgetResponse], error) {
		return client.CallUnary(context.Background(), req)
	}
}

func TestInternalMalformedAuthorizationSchemeRejected(t *testing.T) {
	reg, _ := internalRegistry(t, PostureInternal)
	call := newChainServerWithGate(t, reg, nil, "")
	req := emptyReq()
	req.Header().Set("Authorization", "Basic dXNlcjpwYXNz") // wrong scheme = present but unusable
	_, err := call(req)
	var cerr *connect.Error
	if !errors.As(err, &cerr) || cerr.Code() != connect.CodeUnauthenticated {
		t.Fatalf("malformed scheme on internal plane must reject: %v", err)
	}
}

func TestMTLSAnonymousWithoutCertificateStaysAnonymous(t *testing.T) {
	// mtls posture but the ctx carries no cert (e.g. h2c probe in tests):
	// the call passes as plain anonymous — no synthetic identity invented.
	reg := New(pipeline.New(nil))
	reg.SetAuthPosture(PostureMTLS, nil)
	p := &probe{}
	call := newChainServerWithGate(t, reg, p, "")
	if _, err := call(emptyReq()); err != nil {
		t.Fatalf("certless mtls-posture call in tests must pass: %v", err)
	}
	if p.appCtx.Identity() != nil {
		t.Fatalf("no certificate → no synthetic identity")
	}
}

func TestMTLSCertIdentityPassesPermissionGate(t *testing.T) {
	// The certificate identity is ATTRIBUTION, not an authorization
	// subject: under authz-enabled, a cert-attributed anonymous call passes
	// RequirePermission exactly like the nil-identity anonymous call.
	reg := New(pipeline.New(nil))
	reg.SetAuthPosture(PostureMTLS, nil)
	reg.EnableAuthorization(true)
	p := &probe{}

	reg.Register(CommandWithBody(testProcedure,
		toCreateCommand,
		fromGadgetResult,
		&probeCmdHandler{p: p, inner: &createGadgetHandler{}},
		RequirePermission("gadgets:write"),
	))
	wrapped := WithClientCertIdentity(reg.Handler())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{{
			DNSNames: []string{"service-a"},
		}}}
		wrapped.ServeHTTP(w, req)
	}))
	t.Cleanup(srv.Close)
	client := connect.NewClient[testpb.CreateGadgetRequest, testpb.CreateGadgetResponse](srv.Client(), srv.URL+testProcedure)
	if _, err := client.CallUnary(context.Background(), emptyReq()); err != nil {
		t.Fatalf("cert-attributed call must pass the gate: %v", err)
	}
	if id := p.appCtx.Identity(); id == nil || id.Subject != "service-a" {
		t.Fatalf("attribution identity must still flow to the handler: %+v", p.appCtx.Identity())
	}
}
