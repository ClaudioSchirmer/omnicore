package authcore

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- key generation helpers -------------------------------------------------

func newRSAPrivatePEM(t *testing.T, bits int) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		t.Fatalf("rsa keygen: %v", err)
	}
	return pkcs8PEM(t, key)
}

func newECDSAPrivatePEM(t *testing.T, curve elliptic.Curve) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(curve, rand.Reader)
	if err != nil {
		t.Fatalf("ecdsa keygen: %v", err)
	}
	return pkcs8PEM(t, key)
}

func newEd25519PrivatePEM(t *testing.T) string {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519 keygen: %v", err)
	}
	return pkcs8PEM(t, key)
}

func pkcs8PEM(t *testing.T, key any) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal pkcs8: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

func rs256Key(t *testing.T, kid string, state KeyState) SigningKey {
	return SigningKey{KID: kid, Algorithm: "RS256", PrivatePEM: newRSAPrivatePEM(t, 2048), State: state}
}

func es256Key(t *testing.T, kid string, state KeyState) SigningKey {
	return SigningKey{KID: kid, Algorithm: "ES256", PrivatePEM: newECDSAPrivatePEM(t, elliptic.P256()), State: state}
}

func eddsaKey(t *testing.T, kid string, state KeyState) SigningKey {
	return SigningKey{KID: kid, Algorithm: "EdDSA", PrivatePEM: newEd25519PrivatePEM(t), State: state}
}

func newTestIssuer(t *testing.T, opts IssuerOptions) *Issuer {
	t.Helper()
	if opts.SelfURL == "" {
		opts.SelfURL = testIssuer
	}
	if len(opts.Audience) == 0 {
		opts.Audience = []string{testAudience}
	}
	if opts.TokenTTL == 0 {
		opts.TokenTTL = 15 * time.Minute
	}
	iss, err := NewIssuer(opts)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	return iss
}

// --- NewIssuer construction guards ------------------------------------------

func TestNewIssuer_RequiresSelfURL(t *testing.T) {
	_, err := NewIssuer(IssuerOptions{Keys: []SigningKey{rs256Key(t, "k1", KeyCurrent)}})
	if err == nil || !strings.Contains(err.Error(), "SelfURL") {
		t.Fatalf("expected SelfURL error, got %v", err)
	}
}

func TestNewIssuer_RequiresAtLeastOneKey(t *testing.T) {
	_, err := NewIssuer(IssuerOptions{SelfURL: testIssuer})
	if err == nil || !strings.Contains(err.Error(), "at least one key") {
		t.Fatalf("expected 'at least one key' error, got %v", err)
	}
}

func TestNewIssuer_RequiresExactlyOneCurrent(t *testing.T) {
	t.Run("zero current", func(t *testing.T) {
		_, err := NewIssuer(IssuerOptions{SelfURL: testIssuer, Keys: []SigningKey{rs256Key(t, "k1", KeyNext)}})
		if err == nil || !strings.Contains(err.Error(), "exactly one key must have State=KeyCurrent") {
			t.Fatalf("expected zero-current error, got %v", err)
		}
	})
	t.Run("two current", func(t *testing.T) {
		_, err := NewIssuer(IssuerOptions{SelfURL: testIssuer, Keys: []SigningKey{
			rs256Key(t, "k1", KeyCurrent),
			rs256Key(t, "k2", KeyCurrent),
		}})
		if err == nil || !strings.Contains(err.Error(), "exactly one key must have State=KeyCurrent") {
			t.Fatalf("expected two-current error, got %v", err)
		}
	})
}

func TestNewIssuer_RejectsDuplicateKID(t *testing.T) {
	_, err := NewIssuer(IssuerOptions{SelfURL: testIssuer, Keys: []SigningKey{
		rs256Key(t, "dup", KeyCurrent),
		rs256Key(t, "dup", KeyPrevious),
	}})
	if err == nil || !strings.Contains(err.Error(), "duplicate kid") {
		t.Fatalf("expected duplicate kid error, got %v", err)
	}
}

func TestNewIssuer_RejectsEmptyKID(t *testing.T) {
	k := rs256Key(t, "", KeyCurrent)
	_, err := NewIssuer(IssuerOptions{SelfURL: testIssuer, Keys: []SigningKey{k}})
	if err == nil || !strings.Contains(err.Error(), "empty KID") {
		t.Fatalf("expected empty KID error, got %v", err)
	}
}

func TestNewIssuer_RejectsUnsupportedAlgorithm(t *testing.T) {
	k := SigningKey{KID: "k1", Algorithm: "HS256", PrivatePEM: "irrelevant", State: KeyCurrent}
	_, err := NewIssuer(IssuerOptions{SelfURL: testIssuer, Keys: []SigningKey{k}})
	if err == nil || !strings.Contains(err.Error(), "unsupported algorithm") {
		t.Fatalf("expected unsupported algorithm error, got %v", err)
	}
}

func TestNewIssuer_RejectsMalformedPEM(t *testing.T) {
	k := SigningKey{KID: "k1", Algorithm: "RS256", PrivatePEM: "not a pem", State: KeyCurrent}
	_, err := NewIssuer(IssuerOptions{SelfURL: testIssuer, Keys: []SigningKey{k}})
	if err == nil || !strings.Contains(err.Error(), "no PEM block found") {
		t.Fatalf("expected no-PEM-block error, got %v", err)
	}
}

func TestNewIssuer_RejectsUnparsablePKCS8(t *testing.T) {
	bogus := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("not-a-real-key")})
	k := SigningKey{KID: "k1", Algorithm: "RS256", PrivatePEM: string(bogus), State: KeyCurrent}
	_, err := NewIssuer(IssuerOptions{SelfURL: testIssuer, Keys: []SigningKey{k}})
	if err == nil || !strings.Contains(err.Error(), "parse PKCS8 private key") {
		t.Fatalf("expected PKCS8 parse error, got %v", err)
	}
}

func TestNewIssuer_RejectsAlgorithmKeyTypeMismatch(t *testing.T) {
	// An EdDSA-shaped key declared as RS256.
	k := SigningKey{KID: "k1", Algorithm: "RS256", PrivatePEM: newEd25519PrivatePEM(t), State: KeyCurrent}
	_, err := NewIssuer(IssuerOptions{SelfURL: testIssuer, Keys: []SigningKey{k}})
	if err == nil || !strings.Contains(err.Error(), "requires an RSA private key") {
		t.Fatalf("expected RSA type-mismatch error, got %v", err)
	}
}

func TestNewIssuer_RejectsWeakRSAKey(t *testing.T) {
	k := rs256Key(t, "k1", KeyCurrent)
	k.PrivatePEM = newRSAPrivatePEM(t, 1024)
	_, err := NewIssuer(IssuerOptions{SelfURL: testIssuer, Keys: []SigningKey{k}})
	if err == nil || !strings.Contains(err.Error(), ">= 2048 bits") {
		t.Fatalf("expected weak-RSA error, got %v", err)
	}
}

func TestNewIssuer_RejectsAlgorithmKeyTypeMismatch_ES256(t *testing.T) {
	k := SigningKey{KID: "k1", Algorithm: "ES256", PrivatePEM: newRSAPrivatePEM(t, 2048), State: KeyCurrent}
	_, err := NewIssuer(IssuerOptions{SelfURL: testIssuer, Keys: []SigningKey{k}})
	if err == nil || !strings.Contains(err.Error(), "requires an ECDSA private key") {
		t.Fatalf("expected ECDSA type-mismatch error, got %v", err)
	}
}

func TestNewIssuer_RejectsAlgorithmKeyTypeMismatch_EdDSA(t *testing.T) {
	k := SigningKey{KID: "k1", Algorithm: "EdDSA", PrivatePEM: newRSAPrivatePEM(t, 2048), State: KeyCurrent}
	_, err := NewIssuer(IssuerOptions{SelfURL: testIssuer, Keys: []SigningKey{k}})
	if err == nil || !strings.Contains(err.Error(), "requires an Ed25519 private key") {
		t.Fatalf("expected Ed25519 type-mismatch error, got %v", err)
	}
}

func TestNewIssuer_RejectsWrongCurveForES256(t *testing.T) {
	k := SigningKey{KID: "k1", Algorithm: "ES256", PrivatePEM: newECDSAPrivatePEM(t, elliptic.P384()), State: KeyCurrent}
	_, err := NewIssuer(IssuerOptions{SelfURL: testIssuer, Keys: []SigningKey{k}})
	if err == nil || !strings.Contains(err.Error(), "requires a P-256 key") {
		t.Fatalf("expected P-256 error, got %v", err)
	}
}

func TestNewIssuer_RequiresRefreshStoreWhenTTLSet(t *testing.T) {
	_, err := NewIssuer(IssuerOptions{
		SelfURL:         testIssuer,
		Keys:            []SigningKey{rs256Key(t, "k1", KeyCurrent)},
		RefreshTokenTTL: time.Hour,
	})
	if err == nil || !strings.Contains(err.Error(), "requires a non-nil RefreshStore") {
		t.Fatalf("expected RefreshStore-required error, got %v", err)
	}
}

// --- Issue -------------------------------------------------------------------

func TestIssue_DefaultClaims(t *testing.T) {
	iss := newTestIssuer(t, IssuerOptions{Keys: []SigningKey{rs256Key(t, "k1", KeyCurrent)}})
	before := time.Now()
	tok, err := iss.Issue(context.Background(), TokenRequest{Subject: "user-1"})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if tok.Token == "" || tok.JTI == "" {
		t.Fatalf("empty token/jti: %+v", tok)
	}
	if tok.ExpiresAt.Before(before.Add(14*time.Minute)) || tok.ExpiresAt.After(before.Add(16*time.Minute)) {
		t.Fatalf("unexpected ExpiresAt: %v", tok.ExpiresAt)
	}

	claims := parseUnverified(t, tok.Token)
	if claims["iss"] != testIssuer {
		t.Fatalf("iss = %v", claims["iss"])
	}
	if claims["sub"] != "user-1" {
		t.Fatalf("sub = %v", claims["sub"])
	}
	if claims["jti"] != tok.JTI {
		t.Fatalf("jti mismatch: %v vs %v", claims["jti"], tok.JTI)
	}
	if _, ok := claims["iat"]; !ok {
		t.Fatalf("iat missing")
	}
}

func TestIssue_CustomClaimsCarryThrough(t *testing.T) {
	iss := newTestIssuer(t, IssuerOptions{Keys: []SigningKey{rs256Key(t, "k1", KeyCurrent)}})
	tok, err := iss.Issue(context.Background(), TokenRequest{
		Subject: "user-1",
		Claims:  map[string]any{"groups": []string{"admin"}, "tenant_id": "t-1"},
	})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	claims := parseUnverified(t, tok.Token)
	if claims["tenant_id"] != "t-1" {
		t.Fatalf("tenant_id missing: %v", claims)
	}
}

func TestIssue_RejectsReservedClaims(t *testing.T) {
	iss := newTestIssuer(t, IssuerOptions{Keys: []SigningKey{rs256Key(t, "k1", KeyCurrent)}})
	for _, reserved := range []string{"iss", "aud", "exp", "iat", "nbf", "jti"} {
		_, err := iss.Issue(context.Background(), TokenRequest{
			Subject: "user-1",
			Claims:  map[string]any{reserved: "forged"},
		})
		if err == nil || !strings.Contains(err.Error(), reserved) {
			t.Fatalf("claim %q: expected rejection, got %v", reserved, err)
		}
	}
}

func TestIssue_TTLOverrideAndMaxCap(t *testing.T) {
	iss := newTestIssuer(t, IssuerOptions{
		Keys:        []SigningKey{rs256Key(t, "k1", KeyCurrent)},
		MaxTokenTTL: 5 * time.Minute,
	})
	before := time.Now()
	tok, err := iss.Issue(context.Background(), TokenRequest{Subject: "user-1", TTL: time.Hour})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if tok.ExpiresAt.After(before.Add(6 * time.Minute)) {
		t.Fatalf("TTL not capped by MaxTokenTTL: expires %v", tok.ExpiresAt)
	}
}

func TestIssue_AudienceOverride(t *testing.T) {
	iss := newTestIssuer(t, IssuerOptions{Keys: []SigningKey{rs256Key(t, "k1", KeyCurrent)}})
	tok, err := iss.Issue(context.Background(), TokenRequest{Subject: "user-1", Audience: []string{"orders-api"}})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	claims := parseUnverified(t, tok.Token)
	aud, _ := claims["aud"].([]any)
	if len(aud) != 1 || aud[0] != "orders-api" {
		t.Fatalf("aud = %v", claims["aud"])
	}
}

func TestIssue_NotBeforeBackdatesClaim(t *testing.T) {
	iss := newTestIssuer(t, IssuerOptions{
		Keys:      []SigningKey{rs256Key(t, "k1", KeyCurrent)},
		NotBefore: 30 * time.Second,
	})
	tok, err := iss.Issue(context.Background(), TokenRequest{Subject: "user-1"})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	claims := parseUnverified(t, tok.Token)
	nbf, ok := claims["nbf"].(float64)
	if !ok {
		t.Fatalf("nbf missing or wrong type: %v", claims["nbf"])
	}
	iat, _ := claims["iat"].(float64)
	if nbf >= iat {
		t.Fatalf("nbf (%v) should be backdated before iat (%v)", nbf, iat)
	}
}

func TestIssue_RespectsCancelledContext(t *testing.T) {
	iss := newTestIssuer(t, IssuerOptions{Keys: []SigningKey{rs256Key(t, "k1", KeyCurrent)}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := iss.Issue(ctx, TokenRequest{Subject: "user-1"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

// --- JWKS ----------------------------------------------------------------

func TestJWKS_ContainsEveryKeyNoPrivateMaterial(t *testing.T) {
	iss := newTestIssuer(t, IssuerOptions{Keys: []SigningKey{
		rs256Key(t, "next-key", KeyNext),
		rs256Key(t, "current-key", KeyCurrent),
		es256Key(t, "prev-key", KeyPrevious),
	}})
	doc, err := iss.JWKS()
	if err != nil {
		t.Fatalf("JWKS: %v", err)
	}
	s := string(doc)
	for _, kid := range []string{"next-key", "current-key", "prev-key"} {
		if !strings.Contains(s, kid) {
			t.Fatalf("JWKS missing kid %q: %s", kid, s)
		}
	}
	for _, forbidden := range []string{`"d"`, `"p"`, `"q"`, `"dp"`, `"dq"`, `"qi"`} {
		if strings.Contains(s, forbidden) {
			t.Fatalf("JWKS leaked private field %s: %s", forbidden, s)
		}
	}
}

// --- Round-trip: Issuer.Issue -> Validator.ValidateToken via published JWKS -

func TestRoundTrip_IssuerToValidator(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  func(t *testing.T, kid string, state KeyState) SigningKey
	}{
		{"RS256", rs256Key},
		{"ES256", es256Key},
		{"EdDSA", eddsaKey},
	} {
		t.Run(tc.name, func(t *testing.T) {
			iss := newTestIssuer(t, IssuerOptions{Keys: []SigningKey{tc.key(t, "k1", KeyCurrent)}})

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				doc, err := iss.JWKS()
				if err != nil {
					t.Fatalf("JWKS: %v", err)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(doc)
			}))
			defer srv.Close()

			tok, err := iss.Issue(context.Background(), TokenRequest{Subject: "user-1"})
			if err != nil {
				t.Fatalf("Issue: %v", err)
			}

			v, err := New(Options{
				Issuer:   testIssuer,
				Audience: testAudience,
				JWKSURL:  srv.URL,
			})
			if err != nil {
				t.Fatalf("New(Validator): %v", err)
			}
			identity, verr := v.ValidateToken(context.Background(), tok.Token)
			if verr != nil {
				t.Fatalf("round-trip validation failed: %v", verr)
			}
			if identity.Subject != "user-1" {
				t.Fatalf("subject mismatch: %v", identity.Subject)
			}
		})
	}
}

func TestRoundTrip_PreviousKeyStillValidates(t *testing.T) {
	// Yesterday: oldKey was Current, minted a token still in flight.
	oldKey := rs256Key(t, "old", KeyCurrent)
	yesterday := newTestIssuer(t, IssuerOptions{Keys: []SigningKey{oldKey}})
	oldToken, err := yesterday.Issue(context.Background(), TokenRequest{Subject: "user-1"})
	if err != nil {
		t.Fatalf("Issue (yesterday): %v", err)
	}

	// Today: oldKey demoted to Previous, a new key promoted to Current —
	// the rotation protocol's steady state after a promotion.
	oldKey.State = KeyPrevious
	newKey := rs256Key(t, "new", KeyCurrent)
	today := newTestIssuer(t, IssuerOptions{Keys: []SigningKey{oldKey, newKey}})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		doc, err := today.JWKS()
		if err != nil {
			t.Fatalf("JWKS: %v", err)
		}
		_, _ = w.Write(doc)
	}))
	defer srv.Close()

	v, err := New(Options{Issuer: testIssuer, Audience: testAudience, JWKSURL: srv.URL})
	if err != nil {
		t.Fatalf("New(Validator): %v", err)
	}
	if _, verr := v.ValidateToken(context.Background(), oldToken.Token); verr != nil {
		t.Fatalf("token signed by the now-Previous key should still validate: %v", verr)
	}
}

// --- Refresh tokens --------------------------------------------------------

// memRefreshStore is a trivial in-memory RefreshTokenStore for tests.
type memRefreshStore struct {
	mu      sync.Mutex
	records map[string]RefreshTokenRecord
}

func newMemRefreshStore() *memRefreshStore {
	return &memRefreshStore{records: make(map[string]RefreshTokenRecord)}
}

func (s *memRefreshStore) Save(_ context.Context, rec RefreshTokenRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records[rec.Hash] = rec
	return nil
}

func (s *memRefreshStore) Lookup(_ context.Context, hash string) (RefreshTokenRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.records[hash]
	if !ok {
		return RefreshTokenRecord{}, ErrRefreshTokenNotFound
	}
	return rec, nil
}

func (s *memRefreshStore) MarkUsed(_ context.Context, hash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.records[hash]
	if !ok {
		return ErrRefreshTokenNotFound
	}
	rec.Used = true
	s.records[hash] = rec
	return nil
}

func (s *memRefreshStore) RevokeFamily(_ context.Context, familyID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for h, rec := range s.records {
		if rec.FamilyID == familyID {
			rec.Revoked = true
			s.records[h] = rec
		}
	}
	return nil
}

// flakyRefreshStore wraps memRefreshStore and injects failures at
// configurable points — used to exercise the error-propagation branches
// that a well-behaved store never triggers.
type flakyRefreshStore struct {
	*memRefreshStore
	failSaveAfter    int // Save fails once saveCalls exceeds this (0 = fail every call)
	saveCalls        int
	failMarkUsed     bool
	failRevokeFamily bool
}

var errFlakyStore = errors.New("flaky store failure")

func (s *flakyRefreshStore) Save(ctx context.Context, rec RefreshTokenRecord) error {
	s.saveCalls++
	if s.saveCalls > s.failSaveAfter {
		return errFlakyStore
	}
	return s.memRefreshStore.Save(ctx, rec)
}

func (s *flakyRefreshStore) MarkUsed(ctx context.Context, hash string) error {
	if s.failMarkUsed {
		return errFlakyStore
	}
	return s.memRefreshStore.MarkUsed(ctx, hash)
}

func (s *flakyRefreshStore) RevokeFamily(ctx context.Context, familyID string) error {
	if s.failRevokeFamily {
		return errFlakyStore
	}
	return s.memRefreshStore.RevokeFamily(ctx, familyID)
}

func newRefreshIssuer(t *testing.T) (*Issuer, *memRefreshStore) {
	t.Helper()
	store := newMemRefreshStore()
	iss := newTestIssuer(t, IssuerOptions{
		Keys:            []SigningKey{rs256Key(t, "k1", KeyCurrent)},
		RefreshTokenTTL: time.Hour,
		RefreshStore:    store,
	})
	return iss, store
}

func TestIssueWithRefresh_Disabled(t *testing.T) {
	iss := newTestIssuer(t, IssuerOptions{Keys: []SigningKey{rs256Key(t, "k1", KeyCurrent)}})
	_, _, err := iss.IssueWithRefresh(context.Background(), TokenRequest{Subject: "user-1"})
	if err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("expected disabled error, got %v", err)
	}
	_, _, err = iss.RedeemRefreshToken(context.Background(), "anything", nil)
	if err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("expected disabled error on redeem, got %v", err)
	}
}

func TestIssueWithRefresh_HappyPath(t *testing.T) {
	iss, store := newRefreshIssuer(t)
	access, refresh, err := iss.IssueWithRefresh(context.Background(), TokenRequest{
		Subject: "user-1",
		Claims:  map[string]any{"permissions": []string{"read"}},
	})
	if err != nil {
		t.Fatalf("IssueWithRefresh: %v", err)
	}
	if access.Token == "" || refresh.Value == "" || refresh.FamilyID == "" {
		t.Fatalf("incomplete pair: access=%+v refresh=%+v", access, refresh)
	}
	if len(store.records) != 1 {
		t.Fatalf("expected 1 stored record, got %d", len(store.records))
	}
}

func TestRedeemRefreshToken_RotatesAndUpdatesClaims(t *testing.T) {
	iss, _ := newRefreshIssuer(t)
	_, refresh1, err := iss.IssueWithRefresh(context.Background(), TokenRequest{
		Subject: "user-1",
		Claims:  map[string]any{"permissions": []string{"read"}},
	})
	if err != nil {
		t.Fatalf("IssueWithRefresh: %v", err)
	}

	access2, refresh2, err := iss.RedeemRefreshToken(context.Background(), refresh1.Value,
		map[string]any{"permissions": []string{"read", "write"}})
	if err != nil {
		t.Fatalf("RedeemRefreshToken: %v", err)
	}
	if refresh2.Value == refresh1.Value {
		t.Fatalf("refresh token was not rotated")
	}
	if refresh2.FamilyID != refresh1.FamilyID {
		t.Fatalf("family id changed across rotation: %v -> %v", refresh1.FamilyID, refresh2.FamilyID)
	}
	claims := parseUnverified(t, access2.Token)
	perms, _ := claims["permissions"].([]any)
	if len(perms) != 2 {
		t.Fatalf("expected fresh claims (2 perms) reflected on redeem, got %v", claims["permissions"])
	}

	// The redeemed (first) refresh token cannot be used again.
	_, _, err = iss.RedeemRefreshToken(context.Background(), refresh1.Value, nil)
	if !errors.Is(err, ErrRefreshTokenReused) {
		t.Fatalf("expected ErrRefreshTokenReused on first-token reuse, got %v", err)
	}
}

func TestRedeemRefreshToken_ReuseRevokesWholeFamily(t *testing.T) {
	iss, _ := newRefreshIssuer(t)
	_, refresh1, err := iss.IssueWithRefresh(context.Background(), TokenRequest{Subject: "user-1"})
	if err != nil {
		t.Fatalf("IssueWithRefresh: %v", err)
	}
	_, refresh2, err := iss.RedeemRefreshToken(context.Background(), refresh1.Value, nil)
	if err != nil {
		t.Fatalf("RedeemRefreshToken (rotate): %v", err)
	}

	// Replaying the ALREADY-REDEEMED refresh1 triggers reuse detection,
	// which must revoke the whole family — including the CURRENT valid
	// refresh2, simulating the stolen-token scenario.
	_, _, err = iss.RedeemRefreshToken(context.Background(), refresh1.Value, nil)
	if !errors.Is(err, ErrRefreshTokenReused) {
		t.Fatalf("expected ErrRefreshTokenReused, got %v", err)
	}

	_, _, err = iss.RedeemRefreshToken(context.Background(), refresh2.Value, nil)
	if !errors.Is(err, ErrRefreshTokenReused) {
		t.Fatalf("expected refresh2 to be revoked as part of the family, got %v", err)
	}
}

func TestRedeemRefreshToken_NotFound(t *testing.T) {
	iss, _ := newRefreshIssuer(t)
	_, _, err := iss.RedeemRefreshToken(context.Background(), "never-issued", nil)
	if !errors.Is(err, ErrRefreshTokenNotFound) {
		t.Fatalf("expected ErrRefreshTokenNotFound, got %v", err)
	}
}

func TestIssueWithRefresh_PropagatesIssueError(t *testing.T) {
	iss, _ := newRefreshIssuer(t)
	_, _, err := iss.IssueWithRefresh(context.Background(), TokenRequest{
		Subject: "user-1",
		Claims:  map[string]any{"iss": "forged"},
	})
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("expected reserved-claim error to propagate, got %v", err)
	}
}

func TestIssueWithRefresh_PropagatesMintRefreshError(t *testing.T) {
	store := &flakyRefreshStore{memRefreshStore: newMemRefreshStore(), failSaveAfter: 0}
	iss := newTestIssuer(t, IssuerOptions{
		Keys:            []SigningKey{rs256Key(t, "k1", KeyCurrent)},
		RefreshTokenTTL: time.Hour,
		RefreshStore:    store,
	})
	_, _, err := iss.IssueWithRefresh(context.Background(), TokenRequest{Subject: "user-1"})
	if !errors.Is(err, errFlakyStore) {
		t.Fatalf("expected flaky store error to propagate, got %v", err)
	}
}

func TestRedeemRefreshToken_PropagatesIssueError(t *testing.T) {
	iss, _ := newRefreshIssuer(t)
	_, refresh, err := iss.IssueWithRefresh(context.Background(), TokenRequest{Subject: "user-1"})
	if err != nil {
		t.Fatalf("IssueWithRefresh: %v", err)
	}
	_, _, err = iss.RedeemRefreshToken(context.Background(), refresh.Value, map[string]any{"jti": "forged"})
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("expected reserved-claim error to propagate, got %v", err)
	}
}

func TestRedeemRefreshToken_PropagatesRevokeFamilyError(t *testing.T) {
	base := newMemRefreshStore()
	store := &flakyRefreshStore{memRefreshStore: base, failSaveAfter: 1000, failRevokeFamily: true}
	iss := newTestIssuer(t, IssuerOptions{
		Keys:            []SigningKey{rs256Key(t, "k1", KeyCurrent)},
		RefreshTokenTTL: time.Hour,
		RefreshStore:    store,
	})
	_, refresh, err := iss.IssueWithRefresh(context.Background(), TokenRequest{Subject: "user-1"})
	if err != nil {
		t.Fatalf("IssueWithRefresh: %v", err)
	}
	// First redeem marks it Used directly in the underlying store so the
	// SECOND redeem below hits the reuse-detection RevokeFamily call.
	hash := hashRefreshValue(refresh.Value)
	base.mu.Lock()
	rec := base.records[hash]
	rec.Used = true
	base.records[hash] = rec
	base.mu.Unlock()

	_, _, err = iss.RedeemRefreshToken(context.Background(), refresh.Value, nil)
	if !errors.Is(err, errFlakyStore) {
		t.Fatalf("expected RevokeFamily error to propagate, got %v", err)
	}
}

func TestRedeemRefreshToken_PropagatesMarkUsedError(t *testing.T) {
	base := newMemRefreshStore()
	store := &flakyRefreshStore{memRefreshStore: base, failSaveAfter: 1000, failMarkUsed: true}
	iss := newTestIssuer(t, IssuerOptions{
		Keys:            []SigningKey{rs256Key(t, "k1", KeyCurrent)},
		RefreshTokenTTL: time.Hour,
		RefreshStore:    store,
	})
	_, refresh, err := iss.IssueWithRefresh(context.Background(), TokenRequest{Subject: "user-1"})
	if err != nil {
		t.Fatalf("IssueWithRefresh: %v", err)
	}
	_, _, err = iss.RedeemRefreshToken(context.Background(), refresh.Value, nil)
	if !errors.Is(err, errFlakyStore) {
		t.Fatalf("expected MarkUsed error to propagate, got %v", err)
	}
}

func TestRedeemRefreshToken_PropagatesSecondMintRefreshError(t *testing.T) {
	store := &flakyRefreshStore{memRefreshStore: newMemRefreshStore(), failSaveAfter: 1}
	iss := newTestIssuer(t, IssuerOptions{
		Keys:            []SigningKey{rs256Key(t, "k1", KeyCurrent)},
		RefreshTokenTTL: time.Hour,
		RefreshStore:    store,
	})
	// First Save (the initial login) succeeds; the SECOND Save (the
	// rotation inside RedeemRefreshToken) is the one that fails.
	_, refresh, err := iss.IssueWithRefresh(context.Background(), TokenRequest{Subject: "user-1"})
	if err != nil {
		t.Fatalf("IssueWithRefresh: %v", err)
	}
	_, _, err = iss.RedeemRefreshToken(context.Background(), refresh.Value, nil)
	if !errors.Is(err, errFlakyStore) {
		t.Fatalf("expected second-mintRefresh error to propagate, got %v", err)
	}
}

func TestRedeemRefreshToken_Expired(t *testing.T) {
	store := newMemRefreshStore()
	iss := newTestIssuer(t, IssuerOptions{
		Keys:            []SigningKey{rs256Key(t, "k1", KeyCurrent)},
		RefreshTokenTTL: time.Hour,
		RefreshStore:    store,
	})
	_, refresh, err := iss.IssueWithRefresh(context.Background(), TokenRequest{Subject: "user-1"})
	if err != nil {
		t.Fatalf("IssueWithRefresh: %v", err)
	}
	// Force expiry directly in the store.
	hash := hashRefreshValue(refresh.Value)
	store.mu.Lock()
	rec := store.records[hash]
	rec.ExpiresAt = time.Now().Add(-time.Minute)
	store.records[hash] = rec
	store.mu.Unlock()

	_, _, err = iss.RedeemRefreshToken(context.Background(), refresh.Value, nil)
	if !errors.Is(err, ErrRefreshTokenExpired) {
		t.Fatalf("expected ErrRefreshTokenExpired, got %v", err)
	}
}

// --- helpers -----------------------------------------------------------------

// parseUnverified decodes a JWT's claims without verifying the signature —
// fine for tests that already proved signature validity via the real
// Validator in the round-trip tests, and just want to inspect the payload.
func parseUnverified(t *testing.T, token string) map[string]any {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("malformed token: %s", token)
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatalf("unmarshal claims: %v", err)
	}
	return claims
}
