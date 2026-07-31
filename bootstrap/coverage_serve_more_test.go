package bootstrap

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
)

// Remaining serve() branches: the exported Serve wrapper, the TLS gRPC
// listener (self-signed pair on an ephemeral port), and the integration
// consumer-pool start-failure abort.

func TestServeExported_Wrapper(t *testing.T) {
	if err := Serve(cancelledCtx(), serveDeps(), Wiring{}); err != nil {
		t.Fatalf("Serve: %v", err)
	}
}

// selfSignedPair writes a throwaway localhost certificate + key and returns
// their paths.
func selfSignedPair(t *testing.T) (certFile, keyFile string) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	dir := t.TempDir()
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	keyDER := x509.MarshalPKCS1PrivateKey(priv)
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

func TestServe_GRPCListenerTLS(t *testing.T) {
	certFile, keyFile := selfSignedPair(t)
	d := serveDeps()
	d.Config.GRPC.Addr = "127.0.0.1:0"
	d.Config.GRPC.CertFile = certFile
	d.Config.GRPC.KeyFile = keyFile
	if err := serve(cancelledCtx(), d, Wiring{Features: []Feature{grpcFeature{}}}); err != nil {
		t.Fatalf("serve with TLS grpc listener: %v", err)
	}
}

// bootIntRequest/bootIntCommand/bootIntHandler are the minimal receiver
// fixture the integration registry's reflective planReceiver accepts.
type bootIntRequest struct {
	Email string `json:"email"`
}

type bootIntCommand struct{ Email string }

func (r bootIntRequest) ToCommand() *bootIntCommand { return &bootIntCommand{Email: r.Email} }

type bootIntResult struct{ OK bool }

func (r bootIntResult) IsSuccess() bool { return r.OK }

type bootIntHandler struct{}

func (h *bootIntHandler) Handle(_ *configuration.AppContext, _ *bootIntCommand) (bootIntResult, error) {
	return bootIntResult{OK: true}, nil
}

// A registered receiver with NO integration yaml block cannot resolve at
// ConsumerPool.Start — serve aborts the boot.
func TestServe_ConsumerPoolStartFailureAborts(t *testing.T) {
	d := serveDeps()
	d.IntegrationRegistry.From("partners").On("onboarded", bootIntRequest{}, &bootIntHandler{})
	err := serve(cancelledCtx(), d, Wiring{})
	if err == nil || !strings.Contains(err.Error(), "integration consumer pool") {
		t.Fatalf("expected the consumer-pool start abort, got %v", err)
	}
}
