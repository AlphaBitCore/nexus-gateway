package wiring

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/cmd/compliance-proxy/config"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/testutil"
)

// TestInitCertIssuer_RemoteSigningMode_BadCACertPath exercises the
// "remote" signing mode path where the CA cert file does not exist.
func TestInitCertIssuer_RemoteSigningMode_BadCACertPath(t *testing.T) {
	cfg := &config.Config{}
	cfg.CA.CertPath = "/nonexistent/ca.pem"
	cfg.CA.KeyPath = "/nonexistent/ca.key"
	cfg.CA.KMS.SigningMode = "remote"
	cfg.CA.KMS.SignCommand = []string{"cat"}

	_, err := InitCertIssuer(cfg, nil, testLogger())
	if err == nil {
		t.Fatal("expected error for remote signing mode with bad CA cert path")
	}
}

// TestInitCertIssuer_RemoteSigningMode_ValidCACert exercises the remote
// signing mode when the CA cert file exists. The SignCommand is minimal.
func TestInitCertIssuer_RemoteSigningMode_ValidCACert(t *testing.T) {
	dir := t.TempDir()
	certPath, _, err := testutil.WriteTestCA(dir)
	if err != nil {
		t.Fatalf("WriteTestCA: %v", err)
	}

	cfg := &config.Config{}
	cfg.CA.CertPath = certPath
	cfg.CA.KeyPath = dir + "/ca.key" // not used in remote mode (cert only)
	cfg.CA.KMS.SigningMode = "remote"
	// Use "cat" as the sign command — NewCommandSigner just validates it runs.
	cfg.CA.KMS.SignCommand = []string{"cat"}

	result, err := InitCertIssuer(cfg, nil, testLogger())
	if err != nil {
		t.Fatalf("InitCertIssuer remote: %v", err)
	}
	if result.Issuer == nil {
		t.Error("expected non-nil Issuer in remote signing mode")
	}
}
