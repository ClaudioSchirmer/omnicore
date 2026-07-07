package authcore

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	testIssuer   = "https://idp.test"
	testAudience = "omnicore-tests"
)

func newKeypair(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatalf("marshal pub: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	return key, string(pemBytes)
}

func signToken(t *testing.T, key *rsa.PrivateKey, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return s
}

func validClaims() jwt.MapClaims {
	return jwt.MapClaims{
		"sub": "user-1",
		"iss": testIssuer,
		"aud": testAudience,
		"exp": time.Now().Add(time.Hour).Unix(),
	}
}

func newValidator(t *testing.T, pemKey string, external TokenChecker) *Validator {
	t.Helper()
	v, err := New(Options{
		Issuer:       testIssuer,
		Audience:     testAudience,
		PublicKeyPEM: pemKey,
		External:     external,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return v
}

func TestValidateAuthorizationHappyPath(t *testing.T) {
	key, pub := newKeypair(t)
	v := newValidator(t, pub, nil)
	raw := signToken(t, key, validClaims())

	identity, token, verr := v.ValidateAuthorization(context.Background(), "Bearer "+raw)
	if verr != nil {
		t.Fatalf("unexpected failure: %v", verr)
	}
	if token != raw {
		t.Fatalf("raw token not returned")
	}
	if identity.Subject != "user-1" || identity.Issuer != testIssuer {
		t.Fatalf("identity mismatch: %+v", identity)
	}
	if identity.ExpiresAt.IsZero() {
		t.Fatalf("ExpiresAt not populated")
	}
	if identity.Claims["aud"] != testAudience {
		t.Fatalf("raw claims not carried: %v", identity.Claims)
	}
}

func TestValidateAuthorizationMissingToken(t *testing.T) {
	_, pub := newKeypair(t)
	v := newValidator(t, pub, nil)
	for _, header := range []string{"", "Basic abc", "Bearer ", "Bearer"} {
		_, _, verr := v.ValidateAuthorization(context.Background(), header)
		if verr == nil || verr.Failure != FailureMissingToken {
			t.Fatalf("header %q: expected FailureMissingToken, got %v", header, verr)
		}
	}
}

func TestValidateTokenExpired(t *testing.T) {
	key, pub := newKeypair(t)
	v := newValidator(t, pub, nil)
	claims := validClaims()
	claims["exp"] = time.Now().Add(-time.Hour).Unix()
	_, verr := v.ValidateToken(context.Background(), signToken(t, key, claims))
	if verr == nil || verr.Failure != FailureExpiredToken {
		t.Fatalf("expected FailureExpiredToken, got %v", verr)
	}
	if verr.Unwrap() == nil {
		t.Fatalf("expected wrapped cause")
	}
}

func TestValidateTokenWrongIssuer(t *testing.T) {
	key, pub := newKeypair(t)
	v := newValidator(t, pub, nil)
	claims := validClaims()
	claims["iss"] = "https://evil.test"
	_, verr := v.ValidateToken(context.Background(), signToken(t, key, claims))
	if verr == nil || verr.Failure != FailureInvalidToken {
		t.Fatalf("expected FailureInvalidToken, got %v", verr)
	}
}

func TestValidateTokenWrongKey(t *testing.T) {
	key, _ := newKeypair(t)
	_, otherPub := newKeypair(t)
	v := newValidator(t, otherPub, nil)
	_, verr := v.ValidateToken(context.Background(), signToken(t, key, validClaims()))
	if verr == nil || verr.Failure != FailureInvalidToken {
		t.Fatalf("expected FailureInvalidToken, got %v", verr)
	}
}

func TestValidateTokenGarbage(t *testing.T) {
	_, pub := newKeypair(t)
	v := newValidator(t, pub, nil)
	_, verr := v.ValidateToken(context.Background(), "not-a-jwt")
	if verr == nil || verr.Failure != FailureInvalidToken {
		t.Fatalf("expected FailureInvalidToken, got %v", verr)
	}
}

type stubChecker struct{ err error }

func (s stubChecker) Validate(context.Context, string) error { return s.err }

func TestExternalCheckerRevokes(t *testing.T) {
	key, pub := newKeypair(t)
	v := newValidator(t, pub, stubChecker{err: errors.New("revoked")})
	_, verr := v.ValidateToken(context.Background(), signToken(t, key, validClaims()))
	if verr == nil || verr.Failure != FailureInvalidToken {
		t.Fatalf("expected FailureInvalidToken from revocation, got %v", verr)
	}
}

func TestExternalCheckerAccepts(t *testing.T) {
	key, pub := newKeypair(t)
	v := newValidator(t, pub, stubChecker{})
	if _, verr := v.ValidateToken(context.Background(), signToken(t, key, validClaims())); verr != nil {
		t.Fatalf("unexpected failure: %v", verr)
	}
}

func TestBuildKeyfuncExclusivity(t *testing.T) {
	if _, err := BuildKeyfunc(Options{}); err == nil {
		t.Fatalf("neither source: expected error")
	}
	if _, err := BuildKeyfunc(Options{JWKSURL: "https://idp.test/jwks", PublicKeyPEM: "x"}); err == nil {
		t.Fatalf("both sources: expected error")
	}
}

func TestParsePublicKeyPEMErrors(t *testing.T) {
	if _, err := ParsePublicKeyPEM([]byte("garbage")); err == nil {
		t.Fatalf("no PEM block: expected error")
	}
	bad := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: []byte("nope")})
	if _, err := ParsePublicKeyPEM(bad); err == nil {
		t.Fatalf("bad DER: expected error")
	}
}

func TestNewInvalidPEM(t *testing.T) {
	if _, err := New(Options{PublicKeyPEM: "garbage"}); err == nil {
		t.Fatalf("expected construction error")
	}
}

func TestExtractBearerToken(t *testing.T) {
	cases := []struct {
		header string
		want   string
		ok     bool
	}{
		{"Bearer abc", "abc", true},
		{"bearer abc", "abc", true}, // case-insensitive scheme
		{"Bearer   abc  ", "abc", true},
		{"", "", false},
		{"Bearer", "", false},
		{"Bearer ", "", false},
		{"Basic abc", "", false},
	}
	for _, tc := range cases {
		got, ok := ExtractBearerToken(tc.header)
		if ok != tc.ok || got != tc.want {
			t.Errorf("ExtractBearerToken(%q) = (%q,%t), want (%q,%t)", tc.header, got, ok, tc.want, tc.ok)
		}
	}
}

func TestErrorStrings(t *testing.T) {
	cases := map[Failure]string{
		FailureMissingToken: "authcore: missing bearer token",
		FailureExpiredToken: "authcore: token expired",
		FailureInvalidToken: "authcore: invalid token",
	}
	for f, want := range cases {
		if got := (&Error{Failure: f}).Error(); got != want {
			t.Errorf("Failure %d: want %q, got %q", f, want, got)
		}
	}
}

func TestAlgorithmAllowlistDefault(t *testing.T) {
	// HS256 (symmetric) must be rejected even unconfigured — the default
	// allowlist is asymmetric-only.
	_, pub := newKeypair(t)
	v := newValidator(t, pub, nil)
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, validClaims())
	s, err := tok.SignedString([]byte("secret"))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	_, verr := v.ValidateToken(context.Background(), s)
	if verr == nil || verr.Failure != FailureInvalidToken {
		t.Fatalf("HS256 must be rejected, got %v", verr)
	}
}

func TestValidateAuthorizationInvalidToken(t *testing.T) {
	_, pub := newKeypair(t)
	v := newValidator(t, pub, nil)
	_, _, verr := v.ValidateAuthorization(context.Background(), "Bearer not-a-jwt")
	if verr == nil || verr.Failure != FailureInvalidToken {
		t.Fatalf("expected FailureInvalidToken, got %v", verr)
	}
}

func TestExtractBearerTokenShortHeader(t *testing.T) {
	if _, ok := ExtractBearerToken("Bear"); ok {
		t.Fatalf("short header must not parse")
	}
}

func TestBuildKeyfuncJWKS(t *testing.T) {
	key, _ := newKeypair(t)
	jwks := fmt.Sprintf(`{"keys":[{"kty":"RSA","kid":"k1","alg":"RS256","use":"sig","n":%q,"e":%q}]}`,
		base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes()),
		base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.PublicKey.E)).Bytes()))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(jwks))
	}))
	defer srv.Close()

	v, err := New(Options{Issuer: testIssuer, Audience: testAudience, JWKSURL: srv.URL})
	if err != nil {
		t.Fatalf("New with JWKS: %v", err)
	}
	claims := validClaims()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = "k1"
	signed, err := tok.SignedString(key)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	identity, verr := v.ValidateToken(context.Background(), signed)
	if verr != nil {
		t.Fatalf("JWKS-backed validation failed: %v", verr)
	}
	if identity.Subject != "user-1" {
		t.Fatalf("identity mismatch: %+v", identity)
	}
}

func TestBuildKeyfuncJWKSMalformedURL(t *testing.T) {
	if _, err := BuildKeyfunc(Options{JWKSURL: "://not-a-url"}); err == nil {
		t.Fatalf("malformed JWKS URL must fail construction")
	}
}

func TestValidateAttributionValidToken(t *testing.T) {
	key, pub := newKeypair(t)
	v := newValidator(t, pub, nil)
	res, verr := v.ValidateAttribution(context.Background(), signToken(t, key, validClaims()))
	if verr != nil || res.Stale || res.Identity.Subject != "user-1" {
		t.Fatalf("valid token: res=%+v err=%v", res, verr)
	}
}

func TestValidateAttributionExpiredAuthenticPasses(t *testing.T) {
	key, pub := newKeypair(t)
	v := newValidator(t, pub, nil)
	claims := validClaims()
	claims["exp"] = time.Now().Add(-2 * time.Hour).Unix()
	res, verr := v.ValidateAttribution(context.Background(), signToken(t, key, claims))
	if verr != nil {
		t.Fatalf("expired-authentic must pass for attribution: %v", verr)
	}
	if !res.Stale {
		t.Fatalf("must be marked stale")
	}
	if res.Identity == nil || res.Identity.Subject != "user-1" {
		t.Fatalf("identity must survive expiry: %+v", res.Identity)
	}
}

func TestValidateAttributionForgedRejected(t *testing.T) {
	key, _ := newKeypair(t)
	_, otherPub := newKeypair(t)
	v := newValidator(t, otherPub, nil)
	// authentic-looking but signed by the wrong key — and ALSO expired, so
	// the expiry tolerance must not mask the signature failure
	claims := validClaims()
	claims["exp"] = time.Now().Add(-time.Hour).Unix()
	if _, verr := v.ValidateAttribution(context.Background(), signToken(t, key, claims)); verr == nil {
		t.Fatalf("forged token must be rejected even when expired")
	}
	if _, verr := v.ValidateAttribution(context.Background(), "not-a-jwt"); verr == nil {
		t.Fatalf("garbage must be rejected")
	}
}

func TestValidateAttributionExpiredWrongIssuerRejected(t *testing.T) {
	key, pub := newKeypair(t)
	v := newValidator(t, pub, nil)
	claims := validClaims()
	claims["exp"] = time.Now().Add(-time.Hour).Unix()
	claims["iss"] = "https://evil.test"
	if _, verr := v.ValidateAttribution(context.Background(), signToken(t, key, claims)); verr == nil {
		t.Fatalf("expired + wrong issuer must be rejected — expiry tolerance must not mask claim violations")
	}
}

func TestValidateAttributionNeverCallsExternal(t *testing.T) {
	key, pub := newKeypair(t)
	v := newValidator(t, pub, stubChecker{err: errors.New("would revoke")})
	// even with a revoking External configured, attribution ignores it
	res, verr := v.ValidateAttribution(context.Background(), signToken(t, key, validClaims()))
	if verr != nil || res.Identity == nil {
		t.Fatalf("attribution must not consult External: %v", verr)
	}
}

func TestExtractBearerTokenSpacesOnly(t *testing.T) {
	if _, ok := ExtractBearerToken("Bearer     "); ok {
		t.Fatalf("spaces-only token must not parse")
	}
}

func TestValidateAttributionExpiredWrongAudienceRejected(t *testing.T) {
	key, pub := newKeypair(t)
	v := newValidator(t, pub, nil)
	claims := validClaims()
	claims["exp"] = time.Now().Add(-time.Hour).Unix()
	claims["aud"] = "someone-else"
	if _, verr := v.ValidateAttribution(context.Background(), signToken(t, key, claims)); verr == nil {
		t.Fatalf("expired + wrong audience must be rejected")
	}
}
