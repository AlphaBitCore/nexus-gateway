package wiring

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/cmd/compliance-proxy/config"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/testutil"
)

func TestInitCertIssuer_NoCACertPathReturnsError(t *testing.T) {
	cfg := &config.Config{}
	cfg.CA.CertPath = "/nonexistent/ca.pem"
	cfg.CA.KeyPath = "/nonexistent/ca.key"
	_, err := InitCertIssuer(cfg, nil, testLogger())
	if err == nil {
		t.Fatal("expected error when CA cert/key paths are invalid")
	}
}

func TestInitCertIssuer_ValidCANoRedis(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, err := testutil.WriteTestCA(dir)
	if err != nil {
		t.Fatalf("WriteTestCA: %v", err)
	}

	cfg := &config.Config{}
	cfg.CA.CertPath = certPath
	cfg.CA.KeyPath = keyPath

	result, err := InitCertIssuer(cfg, nil, testLogger())
	if err != nil {
		t.Fatalf("InitCertIssuer: %v", err)
	}
	if result.Issuer == nil {
		t.Error("expected non-nil Issuer")
	}
	if result.CertCache == nil {
		t.Error("expected non-nil CertCache")
	}
}

func TestInitCertIssuer_WithDomainAllowlistPrewarms(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, err := testutil.WriteTestCA(dir)
	if err != nil {
		t.Fatalf("WriteTestCA: %v", err)
	}

	cfg := &config.Config{}
	cfg.CA.CertPath = certPath
	cfg.CA.KeyPath = keyPath
	// Domain allowlist triggers cert pre-warming (errors are non-fatal).
	cfg.AccessControl.DomainAllowlist = []string{"example.com:443"}

	result, err := InitCertIssuer(cfg, nil, testLogger())
	if err != nil {
		t.Fatalf("InitCertIssuer with domain allowlist: %v", err)
	}
	if result.Issuer == nil {
		t.Error("expected non-nil Issuer")
	}
}

func TestInitCertIssuer_UnknownKMSProviderReturnsError(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, err := testutil.WriteTestCA(dir)
	if err != nil {
		t.Fatalf("WriteTestCA: %v", err)
	}

	cfg := &config.Config{}
	cfg.CA.CertPath = certPath
	cfg.CA.KeyPath = keyPath
	cfg.CA.KMS.Provider = "unknown-kms-provider"

	_, err = InitCertIssuer(cfg, nil, testLogger())
	if err == nil {
		t.Fatal("expected error for unknown KMS provider")
	}
}
