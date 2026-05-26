package wiring

// cert_phase7_test.go — targeted gap tests for InitCertIssuer branches
// not covered by the existing cert_test.go / cert_extra_test.go.

import (
	"os"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/cmd/compliance-proxy/config"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/testutil"
)

// KMS "command" provider paths (cert.go:33-45)

// TestInitCertIssuer_KMSCommand_EmptyArgs_ReturnsError exercises cert.go:38-40
// (kms.NewCommandProvider returns an error when Command is empty).
func TestInitCertIssuer_KMSCommand_EmptyArgs_ReturnsError(t *testing.T) {
	cfg := &config.Config{}
	cfg.CA.KMS.Provider = "command"
	cfg.CA.KMS.Command = []string{} // empty → NewCommandProvider fails
	cfg.CA.CertPath = "/nonexistent/ca.pem"
	cfg.CA.KeyPath = "/nonexistent/ca.key"
	_, err := InitCertIssuer(cfg, nil, testLogger())
	if err == nil {
		t.Fatal("expected error for KMS command provider with empty args")
	}
}

// TestInitCertIssuer_KMSCommand_ValidArgs_CoversSlogLine exercises cert.go:33,
// 41 (kmsProvider = cmdProvider) and 45 (slog.Info "CA private key will be
// unwrapped via KMS"). Uses a real CA from testutil so NewIssuer succeeds.
// The "cat" command passes the PEM key through unchanged so the issuer loads it.
func TestInitCertIssuer_KMSCommand_ValidArgs_SuccessPath(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, err := testutil.WriteTestCA(dir)
	if err != nil {
		t.Fatalf("WriteTestCA: %v", err)
	}

	cfg := &config.Config{}
	cfg.CA.CertPath = certPath
	cfg.CA.KeyPath = keyPath
	cfg.CA.KMS.Provider = "command"
	// "cat {file}" reads the temp file written by Decrypt and passes it through —
	// equivalent to a no-op KMS decrypt. The {file} placeholder is replaced with
	// the temp file path by the CommandProvider.
	cfg.CA.KMS.Command = []string{"cat", "{file}"}
	cfg.CA.KMS.TimeoutSec = 5

	result, err := InitCertIssuer(cfg, nil, testLogger())
	if err != nil {
		t.Fatalf("InitCertIssuer with KMS command (cat): %v", err)
	}
	if result.Issuer == nil {
		t.Error("expected non-nil Issuer when KMS command provider succeeds")
	}
}

// Remote signing mode error paths (cert.go:56-74)

// TestInitCertIssuer_RemoteSigningMode_NoPEMBlock_ReturnsError exercises
// cert.go:56-58 (pem.Decode returns nil when file has no valid PEM header).
func TestInitCertIssuer_RemoteSigningMode_NoPEMBlock_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	// Write a file with no PEM block (garbage content, not "-----BEGIN...").
	certPath := dir + "/garbage.pem"
	if err := os.WriteFile(certPath, []byte("this is not a valid PEM block\n"), 0o600); err != nil {
		t.Fatalf("write garbage cert: %v", err)
	}

	cfg := &config.Config{}
	cfg.CA.CertPath = certPath
	cfg.CA.KMS.SigningMode = "remote"
	cfg.CA.KMS.SignCommand = []string{"cat"}

	_, err := InitCertIssuer(cfg, nil, testLogger())
	if err == nil {
		t.Fatal("expected error when CA cert file has no PEM block")
	}
}

// TestInitCertIssuer_RemoteSigningMode_InvalidDER_ReturnsError exercises
// cert.go:60-62 (x509.ParseCertificate returns error when DER is invalid).
func TestInitCertIssuer_RemoteSigningMode_InvalidDER_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	// Write a PEM file with a CERTIFICATE header but invalid DER bytes inside.
	certPath := dir + "/bad-der.pem"
	// base64-encode "notvalidder" as fake DER content
	badPEM := "-----BEGIN CERTIFICATE-----\nbm90dmFsaWRkZXI=\n-----END CERTIFICATE-----\n"
	if err := os.WriteFile(certPath, []byte(badPEM), 0o600); err != nil {
		t.Fatalf("write bad-der cert: %v", err)
	}

	cfg := &config.Config{}
	cfg.CA.CertPath = certPath
	cfg.CA.KMS.SigningMode = "remote"
	cfg.CA.KMS.SignCommand = []string{"cat"}

	_, err := InitCertIssuer(cfg, nil, testLogger())
	if err == nil {
		t.Fatal("expected error when CA cert has invalid DER")
	}
}

// TestInitCertIssuer_RemoteSigningMode_EmptySignCommand_ReturnsError exercises
// cert.go:68-70 (issuer.NewCommandSigner returns error when SignCommand is empty).
// A valid real cert file is used so x509.ParseCertificate succeeds (line 59).
func TestInitCertIssuer_RemoteSigningMode_EmptySignCommand_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	certPath, _, err := testutil.WriteTestCA(dir)
	if err != nil {
		t.Fatalf("WriteTestCA: %v", err)
	}

	cfg := &config.Config{}
	cfg.CA.CertPath = certPath
	cfg.CA.KMS.SigningMode = "remote"
	cfg.CA.KMS.SignCommand = []string{} // empty → NewCommandSigner fails

	_, err = InitCertIssuer(cfg, nil, testLogger())
	if err == nil {
		t.Fatal("expected error when remote signing command is empty")
	}
}

// Cert pre-warming error (cert.go:94-96)

// TestInitCertIssuer_Warmup_InvalidDomain_LogsWarning exercises cert.go:94-96
// (cache.Warmup returns a non-nil error when cert issuance fails for a domain).
// Using the raw "INVALID\x00DOMAIN" hostname makes the TLS cert library reject it.
func TestInitCertIssuer_Warmup_InvalidDomain_LogsWarning(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, err := testutil.WriteTestCA(dir)
	if err != nil {
		t.Fatalf("WriteTestCA: %v", err)
	}

	cfg := &config.Config{}
	cfg.CA.CertPath = certPath
	cfg.CA.KeyPath = keyPath
	// A hostname with a null byte is guaranteed to fail x509 cert issuance.
	// ExtractDomains strips port 443 — pass the raw form without a port so
	// it's used as-is as the domain.
	cfg.AccessControl.DomainAllowlist = []string{"*.invalid\x00domain"}

	// Must not return an error even when warmup fails — it only warns.
	result, err := InitCertIssuer(cfg, nil, testLogger())
	if err != nil {
		t.Fatalf("InitCertIssuer returned error even though warmup failure is non-fatal: %v", err)
	}
	if result.Issuer == nil {
		t.Error("expected non-nil Issuer even when warmup fails")
	}
}
