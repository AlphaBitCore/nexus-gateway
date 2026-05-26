// Package agentca manages the X.509 Certificate Authority for agent mTLS.
// It generates a self-signed ECDSA P-256 CA, issues client certificates,
// and signs CSRs submitted by agents during enrollment.
package agentca

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	caValidityYears  = 10
	certValidityDays = 90
	serialBytesLen   = 16
)

// caRandReader is the entropy source used by all crypto operations in this
// package (ECDSA key generation, x509 certificate creation). It is a
// package-level variable solely so tests can substitute a failing reader and
// exercise the entropy-error branches; production code never reassigns it.
// Matches the same seam pattern used by packages/shared/identity/pkce.
var caRandReader io.Reader = rand.Reader

// CA holds the agent certificate authority state.
type CA struct {
	mu      sync.RWMutex
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	certPEM string
	dir     string
	logger  *slog.Logger
}

// CertResult holds the result of a certificate issuance.
type CertResult struct {
	CertPEM   string
	KeyPEM    string // empty for CSR-based enrollment
	CaCertPEM string
	Serial    string
	ExpiresAt time.Time
}

// New creates or loads the agent CA from the given directory.
func New(dir string, logger *slog.Logger) (*CA, error) {
	if dir == "" {
		dir = filepath.Join(".", ".agent-ca")
	}

	ca := &CA{dir: dir, logger: logger}

	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("create CA dir: %w", err)
	}

	certPath := filepath.Join(dir, "ca.pem")
	keyPath := filepath.Join(dir, "ca-key.pem")

	if fileExists(certPath) && fileExists(keyPath) {
		if err := ca.load(certPath, keyPath); err != nil {
			return nil, fmt.Errorf("load existing CA: %w", err)
		}
		logger.Info("Loaded existing agent CA", "serial", formatSerial(ca.cert.SerialNumber))
		return ca, nil
	}

	if err := ca.generate(certPath, keyPath); err != nil {
		return nil, fmt.Errorf("generate CA: %w", err)
	}
	logger.Info("Generated new agent CA", "serial", formatSerial(ca.cert.SerialNumber))
	return ca, nil
}

// NewFromFiles loads an existing CA from explicit cert and key file paths.
// Unlike New, this does not auto-generate a CA if the files are missing.
func NewFromFiles(certFile, keyFile string, logger *slog.Logger) (*CA, error) {
	ca := &CA{logger: logger}
	if err := ca.load(certFile, keyFile); err != nil {
		return nil, fmt.Errorf("load CA from files: %w", err)
	}
	logger.Info("Loaded agent CA from files", "serial", formatSerial(ca.cert.SerialNumber))
	return ca, nil
}

// CACertPEM returns the CA certificate in PEM format.
func (ca *CA) CACertPEM() string {
	ca.mu.RLock()
	defer ca.mu.RUnlock()
	return ca.certPEM
}

// SignCSR signs a PKCS#10 Certificate Signing Request submitted by an agent.
func (ca *CA) SignCSR(csrPEM string, subjectCN string) (*CertResult, error) {
	ca.mu.RLock()
	defer ca.mu.RUnlock()

	block, _ := pem.Decode([]byte(csrPEM))
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return nil, errors.New("invalid CSR PEM")
	}

	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse CSR: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("CSR signature invalid: %w", err)
	}

	serial, serialHex := randomSerial()
	expiresAt := time.Now().Add(certValidityDays * 24 * time.Hour)

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: subjectCN},
		NotBefore:    time.Now(),
		NotAfter:     expiresAt,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}

	certDER, err := x509.CreateCertificate(caRandReader, template, ca.cert, csr.PublicKey, ca.key)
	if err != nil {
		return nil, fmt.Errorf("sign CSR: %w", err)
	}

	return &CertResult{
		CertPEM:   pemEncode("CERTIFICATE", certDER),
		CaCertPEM: ca.certPEM,
		Serial:    serialHex,
		ExpiresAt: expiresAt,
	}, nil
}

// SignAttestationCSR signs an Ed25519 CSR for the agent-attestation
// use-case. Unlike SignCSR, it enforces two extra invariants:
//
//  1. The CSR public key MUST be Ed25519. ECDSA / RSA / DSA CSRs are
//     rejected. The architecture freezes Ed25519 as the attestation
//     signing algorithm (per-agent isolation, deterministic signing,
//     ~80µs verify); accepting any other algorithm here would silently
//     break the CP-side verifier and weaken the threat model.
//
//  2. The issued cert has KeyUsage=DigitalSignature ONLY. ExtKeyUsage is
//     left empty — in particular, ClientAuth is NOT set. This enforces
//     the key-separation principle: the
//     attestation key must never be usable as an mTLS client cert, so a
//     compromised attestation key cannot escalate into impersonating
//     the agent's mTLS identity. CP-side cert chain validation should
//     reject any "attestation" cert that carries ClientAuth EKU.
//
// Returned CertResult.KeyPEM is empty — the agent keeps the private key
// in its platform keystore (Apple Keychain / Linux Secret Service /
// Windows CNG) and only ever ships the CSR + public key to Hub.
func (ca *CA) SignAttestationCSR(csrPEM string, subjectCN string) (*CertResult, error) {
	ca.mu.RLock()
	defer ca.mu.RUnlock()

	block, _ := pem.Decode([]byte(csrPEM))
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return nil, errors.New("invalid CSR PEM")
	}

	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse CSR: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("CSR signature invalid: %w", err)
	}
	if _, ok := csr.PublicKey.(ed25519.PublicKey); !ok {
		return nil, fmt.Errorf("attestation CSR must use Ed25519 public key; got %T", csr.PublicKey)
	}

	serial, serialHex := randomSerial()
	expiresAt := time.Now().Add(certValidityDays * 24 * time.Hour)

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: subjectCN},
		NotBefore:    time.Now(),
		NotAfter:     expiresAt,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		// ExtKeyUsage deliberately empty: this cert must NOT be usable
		// as an mTLS client cert. The agent already holds a separate
		// P-256 cert (issued via SignCSR) for that role.
	}

	certDER, err := x509.CreateCertificate(caRandReader, template, ca.cert, csr.PublicKey, ca.key)
	if err != nil {
		return nil, fmt.Errorf("sign attestation CSR: %w", err)
	}

	return &CertResult{
		CertPEM:   pemEncode("CERTIFICATE", certDER),
		CaCertPEM: ca.certPEM,
		Serial:    serialHex,
		ExpiresAt: expiresAt,
	}, nil
}

// IssueClientCert generates a new keypair and issues a client certificate
// (legacy mode — private key returned to caller).
func (ca *CA) IssueClientCert(subjectCN string) (*CertResult, error) {
	ca.mu.RLock()
	defer ca.mu.RUnlock()

	clientKey, err := ecdsa.GenerateKey(elliptic.P256(), caRandReader)
	if err != nil {
		return nil, fmt.Errorf("generate client key: %w", err)
	}

	serial, serialHex := randomSerial()
	expiresAt := time.Now().Add(certValidityDays * 24 * time.Hour)

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: subjectCN},
		NotBefore:    time.Now(),
		NotAfter:     expiresAt,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}

	certDER, err := x509.CreateCertificate(caRandReader, template, ca.cert, &clientKey.PublicKey, ca.key)
	if err != nil {
		return nil, fmt.Errorf("create certificate: %w", err)
	}

	certPEM := pemEncode("CERTIFICATE", certDER)
	keyDER, err := x509.MarshalECPrivateKey(clientKey)
	if err != nil {
		return nil, fmt.Errorf("marshal client key: %w", err)
	}

	return &CertResult{
		CertPEM:   certPEM,
		KeyPEM:    pemEncode("EC PRIVATE KEY", keyDER),
		CaCertPEM: ca.certPEM,
		Serial:    serialHex,
		ExpiresAt: expiresAt,
	}, nil
}

func (ca *CA) generate(certPath, keyPath string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), caRandReader)
	if err != nil {
		return fmt.Errorf("generate CA key: %w", err)
	}

	serial, _ := randomSerial()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "Nexus Agent CA",
			Organization: []string{"Nexus Gateway"},
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().AddDate(caValidityYears, 0, 0),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}

	certDER, err := x509.CreateCertificate(caRandReader, template, template, &key.PublicKey, key)
	if err != nil {
		return fmt.Errorf("create CA cert: %w", err)
	}

	certPEMStr := pemEncode("CERTIFICATE", certDER)
	if err := os.WriteFile(certPath, []byte(certPEMStr), 0600); err != nil {
		return fmt.Errorf("write CA cert: %w", err)
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return fmt.Errorf("marshal CA key: %w", err)
	}
	keyPEMStr := pemEncode("EC PRIVATE KEY", keyDER)
	if err := os.WriteFile(keyPath, []byte(keyPEMStr), 0600); err != nil {
		return fmt.Errorf("write CA key: %w", err)
	}

	parsed, err := x509.ParseCertificate(certDER)
	if err != nil {
		return fmt.Errorf("parse generated cert: %w", err)
	}

	ca.cert = parsed
	ca.key = key
	ca.certPEM = certPEMStr
	return nil
}

func (ca *CA) load(certPath, keyPath string) error {
	certData, err := os.ReadFile(certPath)
	if err != nil {
		return fmt.Errorf("read CA cert: %w", err)
	}
	block, _ := pem.Decode(certData)
	if block == nil {
		return errors.New("invalid CA cert PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fmt.Errorf("parse CA cert: %w", err)
	}

	keyData, err := os.ReadFile(keyPath)
	if err != nil {
		return fmt.Errorf("read CA key: %w", err)
	}
	keyBlock, _ := pem.Decode(keyData)
	if keyBlock == nil {
		return errors.New("invalid CA key PEM")
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return fmt.Errorf("parse CA key: %w", err)
	}

	ca.cert = cert
	ca.key = key
	ca.certPEM = string(certData)
	return nil
}

func randomSerial() (*big.Int, string) {
	b := make([]byte, serialBytesLen)
	_, _ = rand.Read(b)
	serial := new(big.Int).SetBytes(b)
	hex := fmt.Sprintf("%032X", serial)
	return serial, hex
}

func formatSerial(n *big.Int) string {
	return strings.ToUpper(fmt.Sprintf("%032x", n))
}

func pemEncode(blockType string, data []byte) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: data}))
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
