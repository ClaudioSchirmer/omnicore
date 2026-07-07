package bootstrap

import (
	"strings"
	"testing"
)

func TestGRPCConfigDefaults(t *testing.T) {
	var g GRPCConfig
	g.applyDefaults()
	if g.Addr != ":9090" {
		t.Fatalf("default addr: %q", g.Addr)
	}
	explicit := GRPCConfig{Addr: ":7443"}
	explicit.applyDefaults()
	if explicit.Addr != ":7443" {
		t.Fatalf("explicit addr must win: %q", explicit.Addr)
	}
}

func TestGRPCConfigValidateTLSPair(t *testing.T) {
	cases := []struct {
		name    string
		cfg     GRPCConfig
		wantErr string
	}{
		{"neither", GRPCConfig{}, ""},
		{"both", GRPCConfig{CertFile: "c.pem", KeyFile: "k.pem"}, ""},
		{"certOnly", GRPCConfig{CertFile: "c.pem"}, "must be set together"},
		{"keyOnly", GRPCConfig{KeyFile: "k.pem"}, "must be set together"},
	}
	for _, tc := range cases {
		err := tc.cfg.validate()
		if tc.wantErr == "" && err != nil {
			t.Errorf("%s: unexpected error %v", tc.name, err)
		}
		if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
			t.Errorf("%s: want %q, got %v", tc.name, tc.wantErr, err)
		}
	}
}

func TestGRPCConfigValidateTimeout(t *testing.T) {
	bad := GRPCConfig{RequestTimeoutSeconds: -1}
	if err := bad.validate(); err == nil || !strings.Contains(err.Error(), "requestTimeoutSeconds") {
		t.Fatalf("negative timeout must fail: %v", err)
	}
	ok := GRPCConfig{RequestTimeoutSeconds: 30}
	if err := ok.validate(); err != nil {
		t.Fatalf("valid timeout: %v", err)
	}
}

func TestConfigDefaultsIncludeGRPC(t *testing.T) {
	var c Config
	c.applyDefaults()
	if c.GRPC.Addr != ":9090" {
		t.Fatalf("Config.applyDefaults must reach GRPC block: %q", c.GRPC.Addr)
	}
}

func TestGRPCAuthModeDefaultsAndValidation(t *testing.T) {
	var g GRPCConfig
	g.applyDefaults()
	if g.Auth.Mode != "inherit" {
		t.Fatalf("default mode: %q", g.Auth.Mode)
	}
	if g.IdleTimeoutSeconds != 120 {
		t.Fatalf("default idle: %d", g.IdleTimeoutSeconds)
	}

	ok := GRPCConfig{Auth: GRPCAuthConfig{Mode: "internal"}}
	if err := ok.validate(); err != nil {
		t.Fatalf("internal must validate: %v", err)
	}
	bad := GRPCConfig{Auth: GRPCAuthConfig{Mode: "open-bar"}}
	if err := bad.validate(); err == nil || !strings.Contains(err.Error(), "grpc.auth.mode") {
		t.Fatalf("unknown mode must fail: %v", err)
	}
	mtlsNoCA := GRPCConfig{Auth: GRPCAuthConfig{Mode: "mtls"}, CertFile: "c", KeyFile: "k"}
	if err := mtlsNoCA.validate(); err == nil || !strings.Contains(err.Error(), "clientCAFile") {
		t.Fatalf("mtls without CA must fail: %v", err)
	}
	mtlsOK := GRPCConfig{Auth: GRPCAuthConfig{Mode: "mtls"}, CertFile: "c", KeyFile: "k", ClientCAFile: "ca"}
	if err := mtlsOK.validate(); err != nil {
		t.Fatalf("mtls complete must validate: %v", err)
	}
	negIdle := GRPCConfig{IdleTimeoutSeconds: -1}
	negIdle.applyDefaults()
	if negIdle.IdleTimeoutSeconds != -1 {
		t.Fatalf("negative idle must be preserved (explicit disable): %d", negIdle.IdleTimeoutSeconds)
	}
}
