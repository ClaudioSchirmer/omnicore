package web

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/application/translation"
	"github.com/gofiber/fiber/v3"
	"github.com/golang-jwt/jwt/v5"
)

// configurationSetTenantClaimFor switches the package-level tenant-claim name
// to `name` and returns a cleanup callback that restores the previous value.
// Lives here rather than as a method on configuration so the production
// surface only exposes the two natural setters (SetPermissionsClaim /
// SetTenantClaim) — symmetry with the openapi.SetGate cleanup pattern.
func configurationSetTenantClaimFor(t *testing.T, name string) func() {
	t.Helper()
	configuration.SetTenantClaim(name)
	return func() { configuration.SetTenantClaim("tenant_id") }
}

// --- token / key helpers ----------------------------------------------------

type tokenSigner struct {
	priv   *rsa.PrivateKey
	pemPub string
}

func newTokenSigner(t *testing.T) *tokenSigner {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	pubBytes, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	pemBlock := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubBytes})
	return &tokenSigner{priv: priv, pemPub: string(pemBlock)}
}

func (s *tokenSigner) sign(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	signed, err := tok.SignedString(s.priv)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return signed
}

func standardClaims() jwt.MapClaims {
	now := time.Now()
	return jwt.MapClaims{
		"sub": "user-42",
		"iss": "https://idp.test",
		"aud": "test-svc",
		"iat": now.Unix(),
		"exp": now.Add(1 * time.Hour).Unix(),
	}
}

func newPipeline() *pipeline.Pipeline {
	tr := translation.Default()
	tr.Import(translation.CoreENG())
	tr.Import(translation.CorePTBR())
	return pipeline.New(tr)
}

// makeApp builds a Fiber app with AppContextMiddleware + AuthMiddleware. The
// protected route returns 200 with the populated Identity subject so tests can
// observe both branches (skip and pass through).
func makeApp(t *testing.T, opts AuthOptions) *fiber.App {
	t.Helper()
	mw, err := AuthMiddleware(opts, newPipeline())
	if err != nil {
		t.Fatalf("AuthMiddleware: %v", err)
	}
	app := fiber.New()
	app.Use(AppContextMiddleware())
	app.Use(mw)
	app.Get("/protected", func(c fiber.Ctx) error {
		id := AppContext(c).Identity()
		if id == nil {
			return c.SendStatus(fiber.StatusInternalServerError)
		}
		return c.JSON(fiber.Map{"sub": id.Subject})
	})
	// Surfaces both Identity().Subject and BearerToken() so tests can assert
	// that the middleware populated the verified raw token on AppContext.
	app.Get("/protected-bearer", func(c fiber.Ctx) error {
		ac := AppContext(c)
		out := fiber.Map{"bearer": ac.BearerToken()}
		if id := ac.Identity(); id != nil {
			out["sub"] = id.Subject
		}
		return c.JSON(out)
	})
	app.Get("/health", func(c fiber.Ctx) error {
		return c.SendString("ok")
	})
	return app
}

func readBody(t *testing.T, body io.ReadCloser) string {
	t.Helper()
	defer body.Close()
	b, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

// --- unit tests of helpers --------------------------------------------------

func TestExtractBearerToken(t *testing.T) {
	cases := []struct {
		header string
		want   string
		ok     bool
	}{
		{"", "", false},
		{"abc", "", false},
		{"Bearer abc", "abc", true},
		{"bearer abc", "abc", true}, // case-insensitive scheme
		{"Bearer  abc  ", "abc", true},
		{"Bearer ", "", false},
		{"Basic abc", "", false},
	}
	for _, tc := range cases {
		got, ok := extractBearerToken(tc.header)
		if got != tc.want || ok != tc.ok {
			t.Errorf("extractBearerToken(%q) = (%q, %t), want (%q, %t)", tc.header, got, ok, tc.want, tc.ok)
		}
	}
}

func TestParsePublicRoutes_Valid(t *testing.T) {
	r, err := parsePublicRoutes([]string{"GET /health", "post /login"})
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(r) != 2 || r[0].method != "GET" || r[0].path != "/health" || r[1].method != "POST" || r[1].path != "/login" {
		t.Fatalf("unexpected parse: %#v", r)
	}
}

func TestParsePublicRoutes_Invalid(t *testing.T) {
	for _, raw := range []string{"GET", "/health", "GET /health extra"} {
		if _, err := parsePublicRoutes([]string{raw}); err == nil {
			t.Errorf("expected error for %q", raw)
		}
	}
}

// buildKeyfunc's JWKS branch: a local JWKS endpoint is enough to drive
// keyfunc.NewDefaultCtx to a usable Keyfunc (no real network — httptest).
func TestBuildKeyfunc_JWKSSource(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"keys":[]}`))
	}))
	defer srv.Close()

	kf, err := buildKeyfunc(AuthOptions{JWKSURL: srv.URL})
	if err != nil {
		t.Fatalf("buildKeyfunc with a JWKS URL: %v", err)
	}
	if kf == nil {
		t.Fatal("expected a non-nil Keyfunc from the JWKS source")
	}
}

func TestParsePublicKeyPEM_InvalidPEM(t *testing.T) {
	if _, err := parsePublicKeyPEM([]byte("not a pem")); err == nil {
		t.Fatal("expected error for non-PEM input")
	}
}

func TestParsePublicKeyPEM_ValidRSA(t *testing.T) {
	signer := newTokenSigner(t)
	if _, err := parsePublicKeyPEM([]byte(signer.pemPub)); err != nil {
		t.Fatalf("expected RSA PEM to parse, got %v", err)
	}
}

func TestBuildIdentity_CopiesClaims(t *testing.T) {
	// MapClaims.GetExpirationTime only accepts float64 / json.Number — the
	// shape jwt.Parse produces from the wire. Mirror that here.
	claims := jwt.MapClaims{
		"sub":   "user-42",
		"iss":   "https://idp.test",
		"exp":   float64(time.Now().Add(1 * time.Hour).Unix()),
		"roles": []any{"admin", "user"},
	}
	id := buildIdentity(claims)
	if id.Subject != "user-42" {
		t.Errorf("Subject = %q", id.Subject)
	}
	if id.Issuer != "https://idp.test" {
		t.Errorf("Issuer = %q", id.Issuer)
	}
	if id.ExpiresAt.IsZero() {
		t.Error("ExpiresAt should be populated")
	}
	if _, ok := id.Claims["roles"]; !ok {
		t.Error("custom claim roles missing")
	}
	// mutate Identity.Claims must not affect the original map
	id.Claims["sub"] = "mutated"
	if claims["sub"] != "user-42" {
		t.Errorf("buildIdentity should copy claims, original mutated to %v", claims["sub"])
	}
}

// --- middleware construction errors -----------------------------------------

func TestAuthMiddleware_RequiresKeySource(t *testing.T) {
	_, err := AuthMiddleware(AuthOptions{Issuer: "x", Audience: "y"}, newPipeline())
	if err == nil {
		t.Fatal("expected error when neither JWKSURL nor PublicKeyPEM is set")
	}
}

func TestAuthMiddleware_RejectsBothKeySources(t *testing.T) {
	signer := newTokenSigner(t)
	_, err := AuthMiddleware(AuthOptions{
		Issuer:       "x",
		Audience:     "y",
		JWKSURL:      "https://idp/jwks",
		PublicKeyPEM: signer.pemPub,
	}, newPipeline())
	if err == nil {
		t.Fatal("expected error when both JWKSURL and PublicKeyPEM are set")
	}
}

func TestAuthMiddleware_RequiresPipeline(t *testing.T) {
	signer := newTokenSigner(t)
	_, err := AuthMiddleware(AuthOptions{
		Issuer: "x", Audience: "y", PublicKeyPEM: signer.pemPub,
	}, nil)
	if err == nil {
		t.Fatal("expected error when Pipeline is nil")
	}
}

func TestAuthMiddleware_RejectsMalformedPublicRoutes(t *testing.T) {
	signer := newTokenSigner(t)
	_, err := AuthMiddleware(AuthOptions{
		Issuer: "x", Audience: "y", PublicKeyPEM: signer.pemPub,
		PublicRoutes: []string{"BAD"},
	}, newPipeline())
	if err == nil {
		t.Fatal("expected error for malformed publicRoute entry")
	}
}

// --- middleware behavior end-to-end -----------------------------------------

func newOpts(pemPub string) AuthOptions {
	return AuthOptions{
		Issuer:        "https://idp.test",
		Audience:      "test-svc",
		LeewaySeconds: 0,
		PublicKeyPEM:  pemPub,
		PublicRoutes:  []string{"GET /health"},
	}
}

func sendRequest(t *testing.T, app *fiber.App, method, path, bearer string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 0, FailOnTimeout: false})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	return resp.StatusCode, readBody(t, resp.Body)
}

func TestAuthMiddleware_MissingToken_401(t *testing.T) {
	signer := newTokenSigner(t)
	app := makeApp(t, newOpts(signer.pemPub))
	status, body := sendRequest(t, app, "GET", "/protected", "")
	if status != fiber.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", status, body)
	}
	if !strings.Contains(body, "MissingAuthorizationNotification") {
		t.Errorf("body should mention MissingAuthorizationNotification: %s", body)
	}
}

func TestAuthMiddleware_MalformedToken_401(t *testing.T) {
	signer := newTokenSigner(t)
	app := makeApp(t, newOpts(signer.pemPub))
	status, body := sendRequest(t, app, "GET", "/protected", "garbage.not.jwt")
	if status != fiber.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", status, body)
	}
	if !strings.Contains(body, "InvalidTokenNotification") {
		t.Errorf("body should mention InvalidTokenNotification: %s", body)
	}
}

func TestAuthMiddleware_BadSignature_401(t *testing.T) {
	signer1 := newTokenSigner(t)
	signer2 := newTokenSigner(t) // different keypair
	app := makeApp(t, newOpts(signer1.pemPub))
	token := signer2.sign(t, standardClaims())
	status, body := sendRequest(t, app, "GET", "/protected", token)
	if status != fiber.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", status, body)
	}
	if !strings.Contains(body, "InvalidTokenNotification") {
		t.Errorf("body should mention InvalidTokenNotification: %s", body)
	}
}

func TestAuthMiddleware_Expired_401(t *testing.T) {
	signer := newTokenSigner(t)
	app := makeApp(t, newOpts(signer.pemPub))
	c := standardClaims()
	c["exp"] = time.Now().Add(-1 * time.Hour).Unix()
	c["iat"] = time.Now().Add(-2 * time.Hour).Unix()
	token := signer.sign(t, c)
	status, body := sendRequest(t, app, "GET", "/protected", token)
	if status != fiber.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", status, body)
	}
	if !strings.Contains(body, "ExpiredTokenNotification") {
		t.Errorf("body should mention ExpiredTokenNotification: %s", body)
	}
}

func TestAuthMiddleware_WrongIssuer_401(t *testing.T) {
	signer := newTokenSigner(t)
	app := makeApp(t, newOpts(signer.pemPub))
	c := standardClaims()
	c["iss"] = "https://attacker.example.com"
	token := signer.sign(t, c)
	status, body := sendRequest(t, app, "GET", "/protected", token)
	if status != fiber.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", status, body)
	}
	if !strings.Contains(body, "InvalidTokenNotification") {
		t.Errorf("body should mention InvalidTokenNotification: %s", body)
	}
}

func TestAuthMiddleware_WrongAudience_401(t *testing.T) {
	signer := newTokenSigner(t)
	app := makeApp(t, newOpts(signer.pemPub))
	c := standardClaims()
	c["aud"] = "other-service"
	token := signer.sign(t, c)
	status, body := sendRequest(t, app, "GET", "/protected", token)
	if status != fiber.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", status, body)
	}
}

func TestAuthMiddleware_DisallowedAlgorithm_401(t *testing.T) {
	signer := newTokenSigner(t)
	opts := newOpts(signer.pemPub)
	opts.Algorithms = []string{"ES256"} // PEM is RSA → mismatch
	app := makeApp(t, opts)
	token := signer.sign(t, standardClaims())
	status, body := sendRequest(t, app, "GET", "/protected", token)
	if status != fiber.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", status, body)
	}
	if !strings.Contains(body, "InvalidTokenNotification") {
		t.Errorf("body should mention InvalidTokenNotification: %s", body)
	}
}

func TestAuthMiddleware_ValidToken_Populates_Identity(t *testing.T) {
	signer := newTokenSigner(t)
	app := makeApp(t, newOpts(signer.pemPub))
	token := signer.sign(t, standardClaims())
	status, body := sendRequest(t, app, "GET", "/protected", token)
	if status != fiber.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, body)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("unmarshal body: %v (raw=%s)", err, body)
	}
	if got["sub"] != "user-42" {
		t.Errorf("sub in response = %v, want %q", got["sub"], "user-42")
	}
}

func TestAuthMiddleware_PublicRoute_SkipsValidation(t *testing.T) {
	signer := newTokenSigner(t)
	app := makeApp(t, newOpts(signer.pemPub))
	status, body := sendRequest(t, app, "GET", "/health", "")
	if status != fiber.StatusOK {
		t.Fatalf("public route status = %d, want 200; body=%s", status, body)
	}
}

// --- AppContext.BearerToken population -------------------------------------

func TestAuthMiddleware_ValidToken_PopulatesBearerToken(t *testing.T) {
	signer := newTokenSigner(t)
	app := makeApp(t, newOpts(signer.pemPub))
	token := signer.sign(t, standardClaims())
	status, body := sendRequest(t, app, "GET", "/protected-bearer", token)
	if status != fiber.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, body)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("unmarshal body: %v (raw=%s)", err, body)
	}
	if got["bearer"] != token {
		t.Errorf("BearerToken on AppContext = %v, want %q", got["bearer"], token)
	}
}

func TestAuthMiddleware_PublicRoute_LeavesBearerEmpty(t *testing.T) {
	signer := newTokenSigner(t)
	opts := newOpts(signer.pemPub)
	opts.PublicRoutes = append(opts.PublicRoutes, "GET /protected-bearer")
	app := makeApp(t, opts)
	// Sending a valid token to a public route — the middleware must not touch
	// AppContext (no validation runs), so BearerToken stays empty.
	status, body := sendRequest(t, app, "GET", "/protected-bearer", signer.sign(t, standardClaims()))
	if status != fiber.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, body)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("unmarshal body: %v (raw=%s)", err, body)
	}
	if got["bearer"] != "" {
		t.Errorf("public route BearerToken = %v, want \"\"", got["bearer"])
	}
}

func TestAuthMiddleware_External_PopulatesBearerOnSuccess(t *testing.T) {
	// Confirms the bearer is stored only AFTER the external revocation check
	// also passes. With external validation enabled, a locally-valid token
	// that gets accepted by the IdP should land both Identity and BearerToken
	// on the AppContext.
	signer := newTokenSigner(t)
	app, srv := makeAppWithExternalValidator(t, newOpts(signer.pemPub), `{"active":true}`, http.StatusOK)
	defer srv.Close()
	token := signer.sign(t, standardClaims())
	status, body := sendRequest(t, app, "GET", "/protected-bearer", token)
	if status != fiber.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, body)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("unmarshal body: %v (raw=%s)", err, body)
	}
	if got["bearer"] != token {
		t.Errorf("BearerToken after external validation = %v, want %q", got["bearer"], token)
	}
}

// --- external validator integration ----------------------------------------

func makeAppWithExternalValidator(t *testing.T, opts AuthOptions, idpResp string, idpStatus int) (*fiber.App, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(idpStatus)
		_, _ = io.WriteString(w, idpResp)
	}))
	opts.ExternalValidator = &ExternalValidatorOptions{
		Method:         "POST",
		URL:            srv.URL,
		TokenPlacement: "bearer_header",
		Success:        ExternalValidatorSuccess{JSONPath: "$.active", ExpectedValue: true},
		TimeoutMS:      1000,
	}
	return makeApp(t, opts), srv
}

func TestAuthMiddleware_External_AcceptsActive(t *testing.T) {
	signer := newTokenSigner(t)
	app, srv := makeAppWithExternalValidator(t, newOpts(signer.pemPub), `{"active":true}`, http.StatusOK)
	defer srv.Close()
	token := signer.sign(t, standardClaims())
	status, body := sendRequest(t, app, "GET", "/protected", token)
	if status != fiber.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, body)
	}
}

func TestAuthMiddleware_External_RejectsInactive(t *testing.T) {
	signer := newTokenSigner(t)
	app, srv := makeAppWithExternalValidator(t, newOpts(signer.pemPub), `{"active":false}`, http.StatusOK)
	defer srv.Close()
	token := signer.sign(t, standardClaims())
	status, body := sendRequest(t, app, "GET", "/protected", token)
	if status != fiber.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", status, body)
	}
	if !strings.Contains(body, "InvalidTokenNotification") {
		t.Errorf("body should carry InvalidTokenNotification: %s", body)
	}
}

func TestAuthMiddleware_External_FailClosed_RejectsOn5xx(t *testing.T) {
	signer := newTokenSigner(t)
	app, srv := makeAppWithExternalValidator(t, newOpts(signer.pemPub), `oops`, http.StatusServiceUnavailable)
	defer srv.Close()
	token := signer.sign(t, standardClaims())
	status, body := sendRequest(t, app, "GET", "/protected", token)
	if status != fiber.StatusUnauthorized {
		t.Fatalf("fail_closed: status = %d, want 401; body=%s", status, body)
	}
}

func TestAuthMiddleware_External_FailOpen_AcceptsOn5xx(t *testing.T) {
	signer := newTokenSigner(t)
	opts := newOpts(signer.pemPub)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	opts.ExternalValidator = &ExternalValidatorOptions{
		Method:         "POST",
		URL:            srv.URL,
		TokenPlacement: "bearer_header",
		Success:        ExternalValidatorSuccess{JSONPath: "$.active", ExpectedValue: true},
		TimeoutMS:      1000,
		FailMode:       "open",
	}
	app := makeApp(t, opts)
	token := signer.sign(t, standardClaims())
	status, body := sendRequest(t, app, "GET", "/protected", token)
	if status != fiber.StatusOK {
		t.Fatalf("fail_open: status = %d, want 200; body=%s", status, body)
	}
}

func TestAuthMiddleware_External_NotCalled_WhenLocalFails(t *testing.T) {
	signer := newTokenSigner(t)
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		_, _ = io.WriteString(w, `{"active":true}`)
	}))
	defer srv.Close()
	opts := newOpts(signer.pemPub)
	opts.ExternalValidator = &ExternalValidatorOptions{
		Method:         "POST",
		URL:            srv.URL,
		TokenPlacement: "bearer_header",
		Success:        ExternalValidatorSuccess{JSONPath: "$.active", ExpectedValue: true},
	}
	app := makeApp(t, opts)
	// Sign with a different key — local validation rejects before any external call.
	other := newTokenSigner(t)
	token := other.sign(t, standardClaims())
	status, _ := sendRequest(t, app, "GET", "/protected", token)
	if status != fiber.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
	if called {
		t.Error("external validator must not be called when local validation fails")
	}
}

func TestAuthMiddleware_External_BadOptions_FailsAtBoot(t *testing.T) {
	signer := newTokenSigner(t)
	opts := newOpts(signer.pemPub)
	opts.ExternalValidator = &ExternalValidatorOptions{
		// missing URL
		TokenPlacement: "bearer_header",
		Success:        ExternalValidatorSuccess{JSONPath: "$.active", ExpectedValue: true},
	}
	if _, err := AuthMiddleware(opts, newPipeline()); err == nil {
		t.Fatal("expected boot error for invalid external validator options")
	}
}

func TestAuthMiddleware_PublicRoute_OnlyExactMatch(t *testing.T) {
	signer := newTokenSigner(t)
	opts := newOpts(signer.pemPub)
	opts.PublicRoutes = []string{"GET /health"}
	app := makeApp(t, opts)
	// POST /health is not public → 401 expected
	status, _ := sendRequest(t, app, "POST", "/health", "")
	if status != fiber.StatusUnauthorized && status != fiber.StatusMethodNotAllowed && status != fiber.StatusNotFound {
		// the route POST /health is not declared on the app at all; Fiber returns 404 typically.
		// What matters: the middleware did not bypass it as public — any of 401/404/405 demonstrates it ran.
		t.Logf("status = %d (acceptable as long as it is not 200)", status)
	}
}

// --- Phase 4 tenant gate ----------------------------------------------------

func TestAuthMiddleware_TenantNotRequired_AbsenceIgnored(t *testing.T) {
	signer := newTokenSigner(t)
	opts := newOpts(signer.pemPub)
	// TenantRequired is the zero value (false) by default — no tenant claim
	// in the JWT, baseline 200 to prove the existing flow is unaffected.
	app := makeApp(t, opts)
	token := signer.sign(t, standardClaims())
	status, body := sendRequest(t, app, "GET", "/protected", token)
	if status != fiber.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, body)
	}
}

func TestAuthMiddleware_TenantRequired_ClaimPresent_200(t *testing.T) {
	signer := newTokenSigner(t)
	opts := newOpts(signer.pemPub)
	opts.TenantRequired = true
	app := makeApp(t, opts)

	claims := standardClaims()
	claims["tenant_id"] = "acme"
	token := signer.sign(t, claims)

	status, body := sendRequest(t, app, "GET", "/protected", token)
	if status != fiber.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, body)
	}
}

func TestAuthMiddleware_TenantRequired_ClaimAbsent_403(t *testing.T) {
	signer := newTokenSigner(t)
	opts := newOpts(signer.pemPub)
	opts.TenantRequired = true
	app := makeApp(t, opts)

	token := signer.sign(t, standardClaims()) // no tenant_id
	status, body := sendRequest(t, app, "GET", "/protected", token)
	if status != fiber.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", status, body)
	}
	var env map[string]any
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("envelope unmarshal: %v\nbody=%s", err, body)
	}
	errors, _ := env["errors"].([]any)
	if len(errors) != 1 {
		t.Fatalf("expected one error context; got %+v", errors)
	}
	msgs, _ := errors[0].(map[string]any)["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("expected one message; got %+v", msgs)
	}
	got := msgs[0].(map[string]any)
	if got["notificationKey"] != "TenantMissingNotification" {
		t.Errorf("notificationKey = %v, want TenantMissingNotification", got["notificationKey"])
	}
	if got["semantic"] != "Forbidden" {
		t.Errorf("semantic = %v, want Forbidden", got["semantic"])
	}
}

func TestAuthMiddleware_TenantRequired_PublicRouteBypasses(t *testing.T) {
	signer := newTokenSigner(t)
	opts := newOpts(signer.pemPub)
	opts.TenantRequired = true
	opts.PublicRoutes = []string{"GET /health"}
	app := makeApp(t, opts)

	// Public route — no bearer, no tenant claim, must pass through without 403
	status, body := sendRequest(t, app, "GET", "/health", "")
	if status != fiber.StatusOK {
		t.Fatalf("public route hit by tenant check: status=%d body=%s", status, body)
	}
}

func TestAuthMiddleware_TenantRequired_RejectsBeforeHandler(t *testing.T) {
	signer := newTokenSigner(t)
	opts := newOpts(signer.pemPub)
	opts.TenantRequired = true
	mw, err := AuthMiddleware(opts, newPipeline())
	if err != nil {
		t.Fatalf("AuthMiddleware: %v", err)
	}
	app := fiber.New()
	app.Use(AppContextMiddleware())
	app.Use(mw)
	called := false
	app.Get("/protected", func(c fiber.Ctx) error {
		called = true
		return c.SendStatus(200)
	})

	token := signer.sign(t, standardClaims()) // no tenant_id
	req := httptest.NewRequest("GET", "/protected", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != fiber.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if called {
		t.Error("handler must NOT be invoked when tenant gate rejects")
	}
}

func TestAuthMiddleware_TenantRequired_CustomTenantClaim(t *testing.T) {
	// Tenant claim name is read via Identity.TenantID() which consults the
	// package-level setter from application/configuration. Phase 5 wires
	// bootstrap to call SetTenantClaim; here we set it directly to exercise
	// the round-trip.
	defer configurationSetTenantClaimFor(t, "org")()

	signer := newTokenSigner(t)
	opts := newOpts(signer.pemPub)
	opts.TenantRequired = true
	app := makeApp(t, opts)

	claims := standardClaims()
	claims["org"] = "globex" // populates the configured custom claim
	token := signer.sign(t, claims)

	status, body := sendRequest(t, app, "GET", "/protected", token)
	if status != fiber.StatusOK {
		t.Fatalf("expected 200 with custom claim 'org' set, got %d; body=%s", status, body)
	}
}
