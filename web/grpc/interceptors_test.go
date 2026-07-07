package grpc

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"google.golang.org/protobuf/proto"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/internal/testpb"
	"github.com/ClaudioSchirmer/omnicore/web/authcore"
)

const testProcedure = "/omnicore.grpctest.v1.GadgetService/CreateGadget"

type probe struct {
	appCtx *configuration.AppContext
}

// newChainServer mounts one unary probe RPC through the registry's full
// interceptor chain and returns a caller against a live httptest server.
func newChainServer(t *testing.T, reg *Registry, p *probe, fail error) func(req *connect.Request[testpb.CreateGadgetRequest]) (*connect.Response[testpb.CreateGadgetResponse], error) {
	t.Helper()
	fn := func(ctx context.Context, req *connect.Request[testpb.CreateGadgetRequest]) (*connect.Response[testpb.CreateGadgetResponse], error) {
		if p != nil {
			p.appCtx = AppContextFrom(ctx)
		}
		if fail != nil {
			return nil, fail
		}
		return connect.NewResponse(&testpb.CreateGadgetResponse{Id: "ok"}), nil
	}
	h := connect.NewUnaryHandler(testProcedure, fn, reg.HandlerOptions()...)
	reg.mountProcedure(testProcedure, h)
	srv := httptest.NewServer(reg.Handler())
	t.Cleanup(srv.Close)
	client := connect.NewClient[testpb.CreateGadgetRequest, testpb.CreateGadgetResponse](srv.Client(), srv.URL+testProcedure)
	return func(req *connect.Request[testpb.CreateGadgetRequest]) (*connect.Response[testpb.CreateGadgetResponse], error) {
		return client.CallUnary(context.Background(), req)
	}
}

func emptyReq() *connect.Request[testpb.CreateGadgetRequest] {
	return connect.NewRequest(&testpb.CreateGadgetRequest{Name: proto.String("x")})
}

func TestChainAppContextIDLanguageAndEcho(t *testing.T) {
	p := &probe{}
	call := newChainServer(t, New(pipeline.New(nil)), p, nil)

	id := uuid.New()
	req := emptyReq()
	req.Header().Set("X-Request-ID", id.String())
	req.Header().Set("Accept-Language", "pt-BR")
	res, err := call(req)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if p.appCtx.ID() != id {
		t.Fatalf("X-Request-ID not honored: %v != %v", p.appCtx.ID(), id)
	}
	if p.appCtx.Language() != configuration.LangPTBR {
		t.Fatalf("Accept-Language not parsed: %v", p.appCtx.Language())
	}
	if res.Header().Get("X-Request-ID") != id.String() {
		t.Fatalf("X-Request-ID not echoed")
	}
	if _, hasDeadline := p.appCtx.Deadline(); hasDeadline {
		t.Fatalf("no timeout configured — no deadline expected")
	}
}

func TestChainThreadIDFallbackAndGeneratedID(t *testing.T) {
	p := &probe{}
	call := newChainServer(t, New(pipeline.New(nil)), p, nil)

	id := uuid.New()
	req := emptyReq()
	req.Header().Set("Threadid", id.String())
	if _, err := call(req); err != nil {
		t.Fatalf("call: %v", err)
	}
	if p.appCtx.ID() != id {
		t.Fatalf("threadID fallback not honored")
	}

	if _, err := call(emptyReq()); err != nil {
		t.Fatalf("call: %v", err)
	}
	if p.appCtx.ID() == uuid.Nil || p.appCtx.ID() == id {
		t.Fatalf("absent headers must generate a fresh id")
	}
}

func TestChainRequestTimeoutSetsDeadline(t *testing.T) {
	p := &probe{}
	reg := New(pipeline.New(nil))
	reg.SetRequestTimeout(5 * time.Second)
	call := newChainServer(t, reg, p, nil)
	if _, err := call(emptyReq()); err != nil {
		t.Fatalf("call: %v", err)
	}
	if _, ok := p.appCtx.Deadline(); !ok {
		t.Fatalf("server-side timeout must set the AppContext deadline")
	}
}

func TestChainRecoveryConvertsPanic(t *testing.T) {
	reg := New(pipeline.New(nil))
	fn := func(context.Context, *connect.Request[testpb.CreateGadgetRequest]) (*connect.Response[testpb.CreateGadgetResponse], error) {
		panic("boom: secret detail")
	}
	h := connect.NewUnaryHandler(testProcedure, fn, reg.HandlerOptions()...)
	reg.mountProcedure(testProcedure, h)
	srv := httptest.NewServer(reg.Handler())
	t.Cleanup(srv.Close)
	client := connect.NewClient[testpb.CreateGadgetRequest, testpb.CreateGadgetResponse](srv.Client(), srv.URL+testProcedure)

	_, err := client.CallUnary(context.Background(), emptyReq())
	var cerr *connect.Error
	if !errors.As(err, &cerr) || cerr.Code() != connect.CodeInternal {
		t.Fatalf("want Internal, got %v", err)
	}
	if cerr.Message() != "internal server error" {
		t.Fatalf("panic detail must not leak: %q", cerr.Message())
	}
}

func TestChainServerSpanTracingPathRuns(t *testing.T) {
	p := &probe{}
	reg := New(pipeline.New(nil))
	reg.EnableServerSpanTracing(true) // no tracer provider installed → no-op spans
	call := newChainServer(t, reg, p, nil)
	if _, err := call(emptyReq()); err != nil {
		t.Fatalf("tracing-enabled call: %v", err)
	}
	// error path exercises the span outcome recording
	regErr := New(pipeline.New(nil))
	regErr.EnableServerSpanTracing(true)
	callErr := newChainServer(t, regErr, nil, connect.NewError(connect.CodeNotFound, errors.New("nope")))
	if _, err := callErr(emptyReq()); err == nil {
		t.Fatalf("expected error")
	}
}

// --- auth chain ---

func authedRegistry(t *testing.T, policy AuthPolicy) (*Registry, func(claims jwt.MapClaims) string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	pubPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	validator, err := authcore.New(authcore.Options{
		Issuer:       "https://idp.test",
		Audience:     "grpc-tests",
		PublicKeyPEM: pubPEM,
	})
	if err != nil {
		t.Fatalf("authcore.New: %v", err)
	}
	reg := New(pipeline.New(nil))
	reg.EnableAuth(validator, policy)
	sign := func(claims jwt.MapClaims) string {
		tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
		s, err := tok.SignedString(key)
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		return s
	}
	return reg, sign
}

func baseClaims() jwt.MapClaims {
	return jwt.MapClaims{
		"sub": "user-9",
		"iss": "https://idp.test",
		"aud": "grpc-tests",
		"exp": time.Now().Add(time.Hour).Unix(),
	}
}

func assertAuthRejection(t *testing.T, err error, wantCode connect.Code, wantReason string) {
	t.Helper()
	var cerr *connect.Error
	if !errors.As(err, &cerr) || cerr.Code() != wantCode {
		t.Fatalf("want %v, got %v", wantCode, err)
	}
	reasons, _, _ := decodeDetails(t, cerr)
	if len(reasons) == 0 || reasons[0] != wantReason {
		t.Fatalf("want reason %q, got %v", wantReason, reasons)
	}
}

func TestChainAuthRejections(t *testing.T) {
	reg, sign := authedRegistry(t, AuthPolicy{})
	call := newChainServer(t, reg, nil, nil)

	_, err := call(emptyReq())
	assertAuthRejection(t, err, connect.CodeUnauthenticated, "MissingAuthorizationNotification")

	req := emptyReq()
	req.Header().Set("Authorization", "Bearer garbage")
	_, err = call(req)
	assertAuthRejection(t, err, connect.CodeUnauthenticated, "InvalidTokenNotification")

	expired := baseClaims()
	expired["exp"] = time.Now().Add(-time.Hour).Unix()
	req = emptyReq()
	req.Header().Set("Authorization", "Bearer "+sign(expired))
	_, err = call(req)
	assertAuthRejection(t, err, connect.CodeUnauthenticated, "ExpiredTokenNotification")
}

func TestChainAuthSuccessPopulatesIdentity(t *testing.T) {
	reg, sign := authedRegistry(t, AuthPolicy{})
	p := &probe{}
	call := newChainServer(t, reg, p, nil)

	req := emptyReq()
	req.Header().Set("Authorization", "Bearer "+sign(baseClaims()))
	if _, err := call(req); err != nil {
		t.Fatalf("authed call: %v", err)
	}
	identity := p.appCtx.Identity()
	if identity == nil || identity.Subject != "user-9" {
		t.Fatalf("identity not populated: %+v", identity)
	}
	if p.appCtx.BearerToken() == "" {
		t.Fatalf("bearer token not stored")
	}
}

func TestChainAuthTenantRequired(t *testing.T) {
	reg, sign := authedRegistry(t, AuthPolicy{TenantRequired: true})
	call := newChainServer(t, reg, nil, nil)

	req := emptyReq()
	req.Header().Set("Authorization", "Bearer "+sign(baseClaims()))
	_, err := call(req)
	assertAuthRejection(t, err, connect.CodePermissionDenied, "TenantMissingNotification")

	withTenant := baseClaims()
	withTenant["tenant_id"] = "acme"
	req = emptyReq()
	req.Header().Set("Authorization", "Bearer "+sign(withTenant))
	if _, err := call(req); err != nil {
		t.Fatalf("tenant-carrying call must pass: %v", err)
	}
}

func TestChainAuthPublicProcedureBypasses(t *testing.T) {
	reg, _ := authedRegistry(t, AuthPolicy{PublicProcedures: []string{testProcedure}})
	call := newChainServer(t, reg, nil, nil)
	if _, err := call(emptyReq()); err != nil {
		t.Fatalf("public procedure must bypass auth: %v", err)
	}
}

// --- registry surface ---

func TestRegistrySurface(t *testing.T) {
	pipe := pipeline.New(nil)
	reg := New(pipe)
	if reg.Pipeline() != pipe {
		t.Fatalf("Pipeline accessor")
	}
	reg.mountProcedure("/a.v1.A/Do", http.NotFoundHandler())
	reg.mountProcedure("/a.v1.A/DoMore", http.NotFoundHandler()) // same service, recorded once
	reg.mountProcedure("/b.v1.B/Do", http.NotFoundHandler())
	reg.MountRaw("/healthz", http.NotFoundHandler())
	names := reg.ServiceNames()
	if len(names) != 2 || names[0] != "a.v1.A" || names[1] != "b.v1.B" {
		t.Fatalf("ServiceNames: %v", names)
	}
	names[0] = "mutated"
	if reg.ServiceNames()[0] != "a.v1.A" {
		t.Fatalf("ServiceNames must return a copy")
	}
	if len(reg.HandlerOptions()) == 0 {
		t.Fatalf("HandlerOptions empty")
	}
}

func TestAppContextFromFallback(t *testing.T) {
	appCtx := AppContextFrom(context.Background())
	if appCtx == nil || appCtx.Language() != configuration.LangENG {
		t.Fatalf("fallback AppContext: %+v", appCtx)
	}
}

func TestChainErrorCarriesRequestIDMeta(t *testing.T) {
	call := newChainServer(t, New(pipeline.New(nil)), nil,
		connect.NewError(connect.CodeNotFound, errors.New("nope")))
	id := uuid.New()
	req := emptyReq()
	req.Header().Set("X-Request-ID", id.String())
	_, err := call(req)
	var cerr *connect.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("want connect error, got %v", err)
	}
	if cerr.Meta().Get("X-Request-ID") != id.String() {
		t.Fatalf("error meta must echo X-Request-ID: %v", cerr.Meta())
	}
}

func TestParseLanguageUnknownFallsBack(t *testing.T) {
	if got := parseLanguage("xx-ZZ,klingon"); got != configuration.LangENG {
		t.Fatalf("unknown language must fall back to ENG: %v", got)
	}
}

func TestChainAppContextFallbackHasLanguage(t *testing.T) {
	p := &probe{}
	call := newChainServer(t, New(pipeline.New(nil)), p, nil)
	req := emptyReq()
	req.Header().Set("Accept-Language", "de")
	if _, err := call(req); err != nil {
		t.Fatalf("call: %v", err)
	}
	if p.appCtx.Language() != configuration.LangDE {
		t.Fatalf("de must map to LangDEU: %v", p.appCtx.Language())
	}
}

func TestChainServerSpanBridgesCorrelationID(t *testing.T) {
	// A REAL tracer provider (SDK) produces a valid span context, so the
	// correlation-id ↔ trace-id bridge runs — the noop provider used by the
	// other tracing tests yields an invalid span and skips it.
	prev := otel.GetTracerProvider()
	tp := sdktrace.NewTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(prev)
		_ = tp.Shutdown(context.Background())
	})

	p := &probe{}
	reg := New(pipeline.New(nil))
	reg.EnableServerSpanTracing(true)
	call := newChainServer(t, reg, p, nil)
	if _, err := call(emptyReq()); err != nil {
		t.Fatalf("call: %v", err)
	}
	if p.appCtx.CorrelationID() == uuid.Nil {
		t.Fatalf("valid server span must bridge CorrelationID to the trace id")
	}
}
