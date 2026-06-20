package web

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"
)

// formatPath renders "$" when the parsed path is empty (root match).
func TestExternalValidator_FormatPath(t *testing.T) {
	if got := (&externalValidator{}).formatPath(); got != "$" {
		t.Errorf("empty jsonPath formatPath = %q, want $", got)
	}
	if got := (&externalValidator{jsonPath: []string{"a", "b"}}).formatPath(); got != "$.a.b" {
		t.Errorf("formatPath = %q, want $.a.b", got)
	}
}

// callIdP surfaces a JSON parse failure as a transport error (fail-closed).
func TestExternalValidator_CallIdP_BadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("this-is-not-json{"))
	}))
	defer srv.Close()
	v, err := newExternalValidator(ExternalValidatorOptions{
		URL:            srv.URL,
		TokenPlacement: "bearer_header",
		Success:        ExternalValidatorSuccess{JSONPath: "$.active", ExpectedValue: true},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := v.Validate(context.Background(), "tok"); err == nil {
		t.Fatal("expected a parse-json error to reject under fail-closed")
	}
}

// callIdP with a root JSONPath ("$") compares the whole payload; a mismatch
// exercises the formatPath empty-segments path inside the mismatch message.
func TestExternalValidator_RootPathMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"active":true}`))
	}))
	defer srv.Close()
	v, err := newExternalValidator(ExternalValidatorOptions{
		URL:            srv.URL,
		TokenPlacement: "bearer_header",
		Success:        ExternalValidatorSuccess{JSONPath: "$", ExpectedValue: "never-matches"},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := v.Validate(context.Background(), "tok"); err == nil {
		t.Fatal("expected rejection when root payload does not equal expected value")
	}
}

// buildKeyfunc surfaces the PEM-parse error path (invalid PEM material).
func TestBuildKeyfunc_InvalidPEM(t *testing.T) {
	if _, err := buildKeyfunc(AuthOptions{PublicKeyPEM: "not-a-pem"}); err == nil {
		t.Fatal("expected buildKeyfunc to fail on invalid PEM")
	}
}

// parsePublicKeyPEM: a well-formed PEM block whose DER is not a PKIX public key
// reaches the x509.ParsePKIXPublicKey error branch.
func TestParsePublicKeyPEM_ValidBlockBadDER(t *testing.T) {
	block := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: []byte("garbage-der")})
	if _, err := parsePublicKeyPEM(block); err == nil {
		t.Fatal("expected x509 parse error for a non-PKIX DER body")
	}
}

// matchPublic both hit and miss.
func TestMatchPublic_HitAndMiss(t *testing.T) {
	routes := []publicRoute{{method: "GET", path: "/health"}}
	if !matchPublic("GET", "/health", routes) {
		t.Error("expected match for GET /health")
	}
	if matchPublic("POST", "/health", routes) {
		t.Error("method mismatch should not match")
	}
	if matchPublic("GET", "/other", routes) {
		t.Error("path mismatch should not match")
	}
}
