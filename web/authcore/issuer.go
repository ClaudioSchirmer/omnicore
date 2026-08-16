package authcore

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"time"

	"github.com/MicahParks/jwkset"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// KeyState is the rotation lifecycle position of a signing key. A key
// starts as KeyNext (published in the JWKS document, not yet signing) so
// every validator in the mesh has a chance to fetch it before it is ever
// used — the publish-then-sign discipline the rotation protocol depends on
// (see the yaml-reference / token-issuance manual section for the runbook).
type KeyState int

const (
	// KeyNext — published in JWKS, NOT signing yet.
	KeyNext KeyState = iota
	// KeyCurrent — published and signing. Exactly one key must hold this
	// state; NewIssuer rejects zero or more than one.
	KeyCurrent
	// KeyPrevious — published, no longer signing; still validates tokens
	// issued before the last promotion.
	KeyPrevious
)

// signingAlgorithms is the closed set of algorithms an Issuer may sign
// with — asymmetric only, mirroring bootstrap's defaultJWTAlgorithms
// allowlist on the validate side. HMAC is excluded permanently: see the
// rationale on that allowlist.
var signingAlgorithms = map[string]struct{}{
	"RS256": {},
	"ES256": {},
	"EdDSA": {},
}

// SigningKey is one key in an Issuer's rotation set. PrivatePEM is PKCS#8;
// signing material never appears in any Issuer getter, log line, or error
// string.
type SigningKey struct {
	KID        string
	Algorithm  string // RS256 | ES256 | EdDSA — asymmetric only, enforced at construction
	PrivatePEM string // PKCS#8
	State      KeyState
}

// IssuerOptions configures a new Issuer.
type IssuerOptions struct {
	// SelfURL is the `iss` claim every token minted by this Issuer carries.
	SelfURL string

	// Audience is the default `aud` claim; a TokenRequest may override it
	// per token.
	Audience []string

	// TokenTTL is the default access-token lifetime, used when
	// TokenRequest.TTL is zero.
	TokenTTL time.Duration

	// MaxTokenTTL is the hard ceiling on an access token's lifetime —
	// TokenRequest.TTL (and TokenTTL itself) can never exceed it. Zero
	// means no ceiling.
	MaxTokenTTL time.Duration

	// RefreshTokenTTL is the lifetime of a freshly minted refresh token.
	// Zero disables IssueWithRefresh and RedeemRefreshToken entirely.
	RefreshTokenTTL time.Duration

	// Keys is the rotation set: exactly one KeyCurrent, any number of
	// KeyNext/KeyPrevious.
	Keys []SigningKey

	// NotBefore, when non-zero, backdates the `nbf` claim by this duration
	// to tolerate clock skew across the mesh.
	NotBefore time.Duration

	// RefreshStore persists refresh-token rotation state. Required
	// (non-nil) iff RefreshTokenTTL > 0 — the Issuer owns the rotation and
	// reuse-detection algorithm, the caller owns the storage medium.
	RefreshStore RefreshTokenStore
}

// issuerKey is a fully parsed, ready-to-use signing key: the crypto
// material, the matching jwt.SigningMethod, and its precomputed public JWK.
type issuerKey struct {
	kid    string
	state  KeyState
	signer any // *rsa.PrivateKey | *ecdsa.PrivateKey | ed25519.PrivateKey
	method jwt.SigningMethod
	jwk    jwkset.JWKMarshal // PUBLIC fields only — never carries D/P/Q/etc.
}

// Issuer mints and rotates JWT access tokens plus, optionally, opaque
// refresh tokens. Construct once at boot via NewIssuer.
//
// Issuer and Validator deliberately live in the same package: the JWKS
// document Issuer.JWKS produces must be directly consumable by
// BuildKeyfunc, so any divergence between what is published and what is
// accepted is caught by the round-trip test, not by integration surprises.
type Issuer struct {
	selfURL         string
	audience        []string
	tokenTTL        time.Duration
	maxTokenTTL     time.Duration
	refreshTokenTTL time.Duration
	notBefore       time.Duration
	current         *issuerKey
	keys            []*issuerKey // every key, any state — JWKS serves all of them
	refreshStore    RefreshTokenStore
}

// NewIssuer builds an Issuer: parses and validates every key, enforces
// exactly one KeyCurrent and unique kids, and precomputes each key's public
// JWK. Returns an error — never panics — on any malformed or inconsistent
// input; this is a boot-time construction, so the caller is expected to
// treat a non-nil error as a fatal boot abort.
func NewIssuer(opts IssuerOptions) (*Issuer, error) {
	if opts.SelfURL == "" {
		return nil, errors.New("authcore: IssuerOptions.SelfURL is required")
	}
	if len(opts.Keys) == 0 {
		return nil, errors.New("authcore: IssuerOptions.Keys must declare at least one key")
	}
	if opts.RefreshTokenTTL > 0 && opts.RefreshStore == nil {
		return nil, errors.New("authcore: IssuerOptions.RefreshTokenTTL > 0 requires a non-nil RefreshStore")
	}

	seen := make(map[string]struct{}, len(opts.Keys))
	keys := make([]*issuerKey, 0, len(opts.Keys))
	var current *issuerKey
	var currentCount int

	for _, sk := range opts.Keys {
		if sk.KID == "" {
			return nil, errors.New("authcore: a SigningKey with an empty KID is not allowed")
		}
		if _, dup := seen[sk.KID]; dup {
			return nil, fmt.Errorf("authcore: duplicate kid %q", sk.KID)
		}
		seen[sk.KID] = struct{}{}

		signer, method, alg, err := parseSigningKey(sk)
		if err != nil {
			return nil, err
		}
		jwk, err := jwkset.NewJWKFromKey(signer, jwkset.JWKOptions{
			Marshal: jwkset.JWKMarshalOptions{Private: false},
			Metadata: jwkset.JWKMetadataOptions{
				ALG: alg,
				KID: sk.KID,
				USE: jwkset.UseSig,
			},
		})
		if err != nil {
			return nil, fmt.Errorf("authcore: key %q: build public JWK: %w", sk.KID, err)
		}

		ik := &issuerKey{
			kid:    sk.KID,
			state:  sk.State,
			signer: signer,
			method: method,
			jwk:    jwk.Marshal(),
		}
		keys = append(keys, ik)
		if sk.State == KeyCurrent {
			currentCount++
			current = ik
		}
	}
	if currentCount != 1 {
		return nil, fmt.Errorf("authcore: exactly one key must have State=KeyCurrent, got %d", currentCount)
	}

	return &Issuer{
		selfURL:         opts.SelfURL,
		audience:        opts.Audience,
		tokenTTL:        opts.TokenTTL,
		maxTokenTTL:     opts.MaxTokenTTL,
		refreshTokenTTL: opts.RefreshTokenTTL,
		notBefore:       opts.NotBefore,
		current:         current,
		keys:            keys,
		refreshStore:    opts.RefreshStore,
	}, nil
}

// parseSigningKey decodes PrivatePEM (PKCS#8), checks the parsed key's
// concrete type matches the declared Algorithm, and enforces the minimum
// strength for that algorithm (RSA >= 2048 bits; ES256 requires a P-256
// curve).
func parseSigningKey(sk SigningKey) (any, jwt.SigningMethod, jwkset.ALG, error) {
	if _, ok := signingAlgorithms[sk.Algorithm]; !ok {
		return nil, nil, "", fmt.Errorf("authcore: key %q: unsupported algorithm %q (allowed: RS256, ES256, EdDSA)", sk.KID, sk.Algorithm)
	}
	block, _ := pem.Decode([]byte(sk.PrivatePEM))
	if block == nil {
		return nil, nil, "", fmt.Errorf("authcore: key %q: no PEM block found", sk.KID)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, nil, "", fmt.Errorf("authcore: key %q: parse PKCS8 private key: %w", sk.KID, err)
	}

	switch sk.Algorithm {
	case "RS256":
		rsaKey, ok := parsed.(*rsa.PrivateKey)
		if !ok {
			return nil, nil, "", fmt.Errorf("authcore: key %q: algorithm RS256 requires an RSA private key, got %T", sk.KID, parsed)
		}
		if rsaKey.N.BitLen() < 2048 {
			return nil, nil, "", fmt.Errorf("authcore: key %q: RSA key must be >= 2048 bits, got %d", sk.KID, rsaKey.N.BitLen())
		}
		return rsaKey, jwt.SigningMethodRS256, jwkset.AlgRS256, nil
	case "ES256":
		ecKey, ok := parsed.(*ecdsa.PrivateKey)
		if !ok {
			return nil, nil, "", fmt.Errorf("authcore: key %q: algorithm ES256 requires an ECDSA private key, got %T", sk.KID, parsed)
		}
		if ecKey.Curve.Params().BitSize != 256 {
			return nil, nil, "", fmt.Errorf("authcore: key %q: algorithm ES256 requires a P-256 key, got a %d-bit curve", sk.KID, ecKey.Curve.Params().BitSize)
		}
		return ecKey, jwt.SigningMethodES256, jwkset.AlgES256, nil
	default: // "EdDSA" — the only remaining member of signingAlgorithms
		edKey, ok := parsed.(ed25519.PrivateKey)
		if !ok {
			return nil, nil, "", fmt.Errorf("authcore: key %q: algorithm EdDSA requires an Ed25519 private key, got %T", sk.KID, parsed)
		}
		return edKey, jwt.SigningMethodEdDSA, jwkset.AlgEdDSA, nil
	}
}

// reservedClaims are owned by the Issuer and rejected if present in a
// TokenRequest.Claims / RedeemRefreshToken claims map — the caller cannot
// forge its own expiry or issuer. Claims() still overwrites these after
// merging, as defense in depth.
var reservedClaims = []string{"iss", "aud", "exp", "iat", "nbf", "jti"}

func rejectReservedClaims(claims map[string]any) error {
	for _, k := range reservedClaims {
		if _, ok := claims[k]; ok {
			return fmt.Errorf("authcore: claim %q is reserved by the Issuer and cannot be supplied by the caller", k)
		}
	}
	return nil
}

// TokenRequest is the claims the caller controls for one access token.
// Groups, permissions, tenant_id, or any other RBAC shape go through
// Claims; the framework never interprets it — that vocabulary belongs to
// the issuing service's own domain.
type TokenRequest struct {
	Subject  string
	Audience []string       // nil = IssuerOptions.Audience
	TTL      time.Duration  // 0 = IssuerOptions.TokenTTL, capped by MaxTokenTTL
	Claims   map[string]any
}

// IssuedToken is the result of minting one access token.
type IssuedToken struct {
	Token     string
	JTI       string
	ExpiresAt time.Time
}

// Issue mints one access token signed by the current key.
func (i *Issuer) Issue(ctx context.Context, req TokenRequest) (IssuedToken, error) {
	if err := ctx.Err(); err != nil {
		return IssuedToken{}, err
	}
	if err := rejectReservedClaims(req.Claims); err != nil {
		return IssuedToken{}, err
	}

	ttl := req.TTL
	if ttl <= 0 {
		ttl = i.tokenTTL
	}
	if i.maxTokenTTL > 0 && ttl > i.maxTokenTTL {
		ttl = i.maxTokenTTL
	}
	aud := req.Audience
	if len(aud) == 0 {
		aud = i.audience
	}

	now := time.Now()
	jti := uuid.NewString()
	claims := jwt.MapClaims{}
	for k, v := range req.Claims {
		claims[k] = v
	}
	claims["iss"] = i.selfURL
	claims["sub"] = req.Subject
	claims["aud"] = aud
	claims["iat"] = now.Unix()
	claims["exp"] = now.Add(ttl).Unix()
	claims["jti"] = jti
	if i.notBefore > 0 {
		claims["nbf"] = now.Add(-i.notBefore).Unix()
	}

	tok := jwt.NewWithClaims(i.current.method, claims)
	tok.Header["kid"] = i.current.kid
	signed, err := tok.SignedString(i.current.signer)
	if err != nil {
		return IssuedToken{}, fmt.Errorf("authcore: sign token: %w", err)
	}
	return IssuedToken{Token: signed, JTI: jti, ExpiresAt: now.Add(ttl)}, nil
}

// JWKS returns the JWK Set document: every key in any state (Next/Current/
// Previous), so validators can verify tokens signed by Previous and
// pre-fetch Next before it starts signing. Public fields only.
func (i *Issuer) JWKS() ([]byte, error) {
	doc := jwkset.JWKSMarshal{Keys: make([]jwkset.JWKMarshal, 0, len(i.keys))}
	for _, k := range i.keys {
		doc.Keys = append(doc.Keys, k.jwk)
	}
	b, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("authcore: marshal JWKS: %w", err)
	}
	return b, nil
}

// RefreshToken is the opaque, single-use credential returned to the caller
// exactly once — the Value is never re-derivable and the Issuer never
// stores it in plaintext (see RefreshTokenRecord.Hash).
type RefreshToken struct {
	Value     string
	FamilyID  string // all tokens descended from one login; a reuse revokes the whole family
	ExpiresAt time.Time
}

// RefreshTokenRecord is the persisted state of one refresh token in a
// rotation family. Only a hash of the opaque value is ever stored — never
// the raw value, never claims (those come from the caller fresh on every
// RedeemRefreshToken call).
type RefreshTokenRecord struct {
	Hash      string
	FamilyID  string
	Subject   string
	Audience  []string
	ExpiresAt time.Time
	Used      bool
	Revoked   bool
}

// ErrRefreshTokenNotFound is returned by a RefreshTokenStore.Lookup
// implementation when no record matches the given hash — an unknown or
// already-forgotten (e.g. garbage-collected after expiry) refresh token.
var ErrRefreshTokenNotFound = errors.New("authcore: refresh token not found")

// ErrRefreshTokenReused is returned by RedeemRefreshToken when the token
// was already redeemed once before (or its family was already revoked) —
// the standard signal a stolen refresh token was replayed. The entire
// family is revoked as a side effect before this error returns.
var ErrRefreshTokenReused = errors.New("authcore: refresh token reuse detected — session revoked")

// ErrRefreshTokenExpired is returned by RedeemRefreshToken when the token's
// ExpiresAt has passed.
var ErrRefreshTokenExpired = errors.New("authcore: refresh token expired")

// RefreshTokenStore persists refresh-token rotation state. The service
// supplies the implementation (SQL table, Redis, whatever fits); the
// Issuer owns rotation and reuse-detection and never sees the raw secret —
// only Hash ever crosses this seam.
type RefreshTokenStore interface {
	Save(ctx context.Context, rec RefreshTokenRecord) error
	// Lookup returns ErrRefreshTokenNotFound (wrapped or bare, matchable
	// via errors.Is) when hash matches no record.
	Lookup(ctx context.Context, hash string) (RefreshTokenRecord, error)
	MarkUsed(ctx context.Context, hash string) error
	RevokeFamily(ctx context.Context, familyID string) error
}

// IssueWithRefresh mints an access token and a fresh refresh token sharing
// a new FamilyID — the pair a login issues. Requires RefreshTokenTTL > 0.
func (i *Issuer) IssueWithRefresh(ctx context.Context, req TokenRequest) (IssuedToken, RefreshToken, error) {
	if i.refreshStore == nil {
		return IssuedToken{}, RefreshToken{}, errors.New("authcore: refresh tokens are disabled (IssuerOptions.RefreshTokenTTL == 0)")
	}
	access, err := i.Issue(ctx, req)
	if err != nil {
		return IssuedToken{}, RefreshToken{}, err
	}
	aud := req.Audience
	if len(aud) == 0 {
		aud = i.audience
	}
	refresh, err := i.mintRefresh(ctx, req.Subject, aud, uuid.NewString())
	if err != nil {
		return IssuedToken{}, RefreshToken{}, err
	}
	return access, refresh, nil
}

// RedeemRefreshToken validates, single-use-checks, and rotates value.
// claims are supplied fresh by the caller at redemption time — the same
// shape Issue takes — so a permission revoked between logins reaches the
// mesh on the next refresh, not only on the next full login.
//
// Reuse of an already-redeemed value revokes the entire family (the
// session and every token descended from it) and returns
// ErrRefreshTokenReused — the standard signal a stolen refresh token was
// replayed.
func (i *Issuer) RedeemRefreshToken(ctx context.Context, value string, claims map[string]any) (IssuedToken, RefreshToken, error) {
	if i.refreshStore == nil {
		return IssuedToken{}, RefreshToken{}, errors.New("authcore: refresh tokens are disabled (IssuerOptions.RefreshTokenTTL == 0)")
	}
	hash := hashRefreshValue(value)

	rec, err := i.refreshStore.Lookup(ctx, hash)
	if err != nil {
		return IssuedToken{}, RefreshToken{}, err
	}
	if rec.Revoked {
		return IssuedToken{}, RefreshToken{}, ErrRefreshTokenReused
	}
	if rec.Used {
		if revErr := i.refreshStore.RevokeFamily(ctx, rec.FamilyID); revErr != nil {
			return IssuedToken{}, RefreshToken{}, fmt.Errorf("authcore: revoke family after reuse detection: %w", revErr)
		}
		return IssuedToken{}, RefreshToken{}, ErrRefreshTokenReused
	}
	if time.Now().After(rec.ExpiresAt) {
		return IssuedToken{}, RefreshToken{}, ErrRefreshTokenExpired
	}
	if err := i.refreshStore.MarkUsed(ctx, hash); err != nil {
		return IssuedToken{}, RefreshToken{}, fmt.Errorf("authcore: mark refresh token used: %w", err)
	}

	access, err := i.Issue(ctx, TokenRequest{Subject: rec.Subject, Audience: rec.Audience, Claims: claims})
	if err != nil {
		return IssuedToken{}, RefreshToken{}, err
	}
	refresh, err := i.mintRefresh(ctx, rec.Subject, rec.Audience, rec.FamilyID)
	if err != nil {
		return IssuedToken{}, RefreshToken{}, err
	}
	return access, refresh, nil
}

func (i *Issuer) mintRefresh(ctx context.Context, subject string, audience []string, familyID string) (RefreshToken, error) {
	value, hash, err := newOpaqueRefreshValue()
	if err != nil {
		return RefreshToken{}, err
	}
	expiresAt := time.Now().Add(i.refreshTokenTTL)
	rec := RefreshTokenRecord{
		Hash:      hash,
		FamilyID:  familyID,
		Subject:   subject,
		Audience:  audience,
		ExpiresAt: expiresAt,
	}
	if err := i.refreshStore.Save(ctx, rec); err != nil {
		return RefreshToken{}, fmt.Errorf("authcore: save refresh token: %w", err)
	}
	return RefreshToken{Value: value, FamilyID: familyID, ExpiresAt: expiresAt}, nil
}

// newOpaqueRefreshValue generates a 256-bit random value (the credential
// handed to the caller) and its SHA-256 hex hash (what actually gets
// persisted via RefreshTokenStore).
func newOpaqueRefreshValue() (value string, hash string, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("authcore: generate refresh token: %w", err)
	}
	value = base64.RawURLEncoding.EncodeToString(buf)
	return value, hashRefreshValue(value), nil
}

func hashRefreshValue(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
