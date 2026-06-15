package issuer

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/testutil"
)

// failingKMS is a KMSProvider that always returns an error from Decrypt,
// used to exercise the kms.Decrypt error branch in NewIssuer.
type failingKMS struct{ err error }

func (failingKMS) Name() string { return "failing" }
func (k failingKMS) Decrypt(_ context.Context, _ []byte) ([]byte, error) {
	return nil, k.err
}

func TestNewIssuer(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, err := testutil.WriteTestCA(dir)
	if err != nil {
		t.Fatalf("WriteTestCA: %v", err)
	}

	issuer, err := NewIssuer(certPath, keyPath, nil)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}

	if issuer.caCert == nil {
		t.Fatal("caCert is nil")
	}
	if !issuer.caCert.IsCA {
		t.Error("caCert.IsCA should be true")
	}
	if issuer.caKey == nil {
		t.Fatal("caKey is nil")
	}
	if len(issuer.aesKey) != 32 {
		t.Errorf("aesKey length = %d, want 32", len(issuer.aesKey))
	}
}

func TestNewIssuer_BadPaths(t *testing.T) {
	// Non-existent cert file
	if _, err := NewIssuer("/nonexistent/cert.pem", "/nonexistent/key.pem", nil); err == nil {
		t.Error("expected error for non-existent cert path")
	}

	// Existing cert, non-existent key
	dir := t.TempDir()
	certPath, _, err := testutil.WriteTestCA(dir)
	if err != nil {
		t.Fatalf("WriteTestCA: %v", err)
	}
	if _, err := NewIssuer(certPath, "/nonexistent/key.pem", nil); err == nil {
		t.Error("expected error for non-existent key path")
	}
}

func TestSignCert(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, err := testutil.WriteTestCA(dir)
	if err != nil {
		t.Fatalf("WriteTestCA: %v", err)
	}
	issuer, err := NewIssuer(certPath, keyPath, nil)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}

	hostname := "api.openai.com"
	tlsCert, err := issuer.SignCert(hostname)
	if err != nil {
		t.Fatalf("SignCert: %v", err)
	}

	// Should have leaf + CA in the chain
	if len(tlsCert.Certificate) != 2 {
		t.Fatalf("cert chain length = %d, want 2", len(tlsCert.Certificate))
	}

	// Parse the leaf certificate
	leaf, err := x509.ParseCertificate(tlsCert.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf cert: %v", err)
	}

	// Verify ECDSA P-256
	leafKey, ok := tlsCert.PrivateKey.(*ecdsa.PrivateKey)
	if !ok {
		t.Fatal("private key is not ECDSA")
	}
	if leafKey.Curve != elliptic.P256() {
		t.Error("private key curve is not P-256")
	}

	// Verify SAN
	if len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != hostname {
		t.Errorf("DNSNames = %v, want [%s]", leaf.DNSNames, hostname)
	}

	// Verify ~24h validity (NotBefore is back-dated by 2 minutes for clock skew).
	validity := leaf.NotAfter.Sub(leaf.NotBefore)
	expected := 24*time.Hour + 2*time.Minute
	if validity < expected-time.Minute || validity > expected+time.Minute {
		t.Errorf("validity = %v, want ~%v", validity, expected)
	}

	// Verify KeyUsage
	if leaf.KeyUsage&x509.KeyUsageDigitalSignature == 0 {
		t.Error("leaf missing DigitalSignature key usage")
	}

	// Verify ExtKeyUsage
	found := false
	for _, eku := range leaf.ExtKeyUsage {
		if eku == x509.ExtKeyUsageServerAuth {
			found = true
			break
		}
	}
	if !found {
		t.Error("leaf missing ServerAuth extended key usage")
	}

	// Verify chain: leaf signed by CA
	roots := x509.NewCertPool()
	roots.AddCert(issuer.caCert)
	opts := x509.VerifyOptions{
		Roots:     roots,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if _, err := leaf.Verify(opts); err != nil {
		t.Errorf("leaf cert verification failed: %v", err)
	}
}

func TestEncryptDecryptPrivateKey(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, err := testutil.WriteTestCA(dir)
	if err != nil {
		t.Fatalf("WriteTestCA: %v", err)
	}
	issuer, err := NewIssuer(certPath, keyPath, nil)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}

	// Sign a cert to get a leaf key
	tlsCert, err := issuer.SignCert("test.example.com")
	if err != nil {
		t.Fatalf("SignCert: %v", err)
	}
	originalKey := tlsCert.PrivateKey.(*ecdsa.PrivateKey)

	// Encrypt
	ciphertext, nonce, err := issuer.EncryptPrivateKey(originalKey)
	if err != nil {
		t.Fatalf("EncryptPrivateKey: %v", err)
	}
	if len(ciphertext) == 0 {
		t.Fatal("ciphertext is empty")
	}
	if len(nonce) == 0 {
		t.Fatal("nonce is empty")
	}

	// Decrypt
	decryptedKey, err := issuer.DecryptPrivateKey(ciphertext, nonce)
	if err != nil {
		t.Fatalf("DecryptPrivateKey: %v", err)
	}

	// Compare keys
	if !originalKey.Equal(decryptedKey) {
		t.Error("decrypted key does not match original")
	}
}

func TestDecryptPrivateKey_BadCiphertext(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, err := testutil.WriteTestCA(dir)
	if err != nil {
		t.Fatalf("WriteTestCA: %v", err)
	}
	issuer, err := NewIssuer(certPath, keyPath, nil)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}

	// GCM nonce must be exactly 12 bytes; use a valid-length nonce with bad ciphertext
	if _, err := issuer.DecryptPrivateKey([]byte("bad-ciphertext-padding"), []byte("bad-nonce123")); err == nil {
		t.Error("expected error for bad ciphertext")
	}
}

func TestAESKey(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, err := testutil.WriteTestCA(dir)
	if err != nil {
		t.Fatalf("WriteTestCA: %v", err)
	}
	issuer, err := NewIssuer(certPath, keyPath, nil)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}

	key := issuer.AESKey()
	if len(key) != 32 {
		t.Errorf("AESKey length = %d, want 32", len(key))
	}

	// Verify it returns a copy, not the same slice
	key[0] ^= 0xFF
	original := issuer.AESKey()
	if key[0] == original[0] {
		t.Error("AESKey should return a copy, not a reference to internal state")
	}
}

// TestNewIssuer_BadCertPEM: a file that exists but contains no
// CERTIFICATE PEM block must produce the specific "no CERTIFICATE PEM
// block found" error. Security-relevant: this is the gate that prevents
// an operator from accidentally serving a non-CA file as a CA.
func TestNewIssuer_BadCertPEM(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certPath, []byte("not pem at all"), 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyPath, []byte("ignored"), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	_, err := NewIssuer(certPath, keyPath, nil)
	if err == nil || !strings.Contains(err.Error(), "no CERTIFICATE PEM block") {
		t.Errorf("want 'no CERTIFICATE PEM block' error, got: %v", err)
	}
}

// TestNewIssuer_WrongCertPEMType: a file holding a well-formed PEM block
// with the wrong Type (here, RSA PRIVATE KEY) must also fail the
// CERTIFICATE check.
func TestNewIssuer_WrongCertPEMType(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	bogus := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: []byte("anything")})
	if err := os.WriteFile(certPath, bogus, 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyPath, []byte("ignored"), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	_, err := NewIssuer(certPath, keyPath, nil)
	if err == nil || !strings.Contains(err.Error(), "no CERTIFICATE PEM block") {
		t.Errorf("want PEM-type rejection, got: %v", err)
	}
}

// TestNewIssuer_CertParseFails: PEM block is the right Type but the bytes
// inside aren't a parseable X.509 — x509.ParseCertificate must fail.
func TestNewIssuer_CertParseFails(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	bogus := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("not-asn1-der")})
	if err := os.WriteFile(certPath, bogus, 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyPath, []byte("ignored"), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	_, err := NewIssuer(certPath, keyPath, nil)
	if err == nil || !strings.Contains(err.Error(), "parse CA cert") {
		t.Errorf("want 'parse CA cert' error, got: %v", err)
	}
}

// TestNewIssuer_BadKeyPEM: the cert is valid; the key file is junk. Must
// fail at the EC PRIVATE KEY PEM check.
func TestNewIssuer_BadKeyPEM(t *testing.T) {
	dir := t.TempDir()
	certPath, _, err := testutil.WriteTestCA(dir)
	if err != nil {
		t.Fatalf("WriteTestCA: %v", err)
	}
	keyPath := filepath.Join(dir, "bad-key.pem")
	if err := os.WriteFile(keyPath, []byte("not pem"), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	_, err = NewIssuer(certPath, keyPath, nil)
	if err == nil || !strings.Contains(err.Error(), "no EC PRIVATE KEY PEM block") {
		t.Errorf("want 'no EC PRIVATE KEY PEM block' error, got: %v", err)
	}
}

// TestNewIssuer_KeyParseFails: PEM block has the right Type but bytes
// aren't a parseable EC key.
func TestNewIssuer_KeyParseFails(t *testing.T) {
	dir := t.TempDir()
	certPath, _, err := testutil.WriteTestCA(dir)
	if err != nil {
		t.Fatalf("WriteTestCA: %v", err)
	}
	keyPath := filepath.Join(dir, "bad-key.pem")
	bogus := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: []byte("not-asn1-der")})
	if err := os.WriteFile(keyPath, bogus, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	_, err = NewIssuer(certPath, keyPath, nil)
	if err == nil || !strings.Contains(err.Error(), "parse CA key") {
		t.Errorf("want 'parse CA key' error, got: %v", err)
	}
}

// TestNewIssuer_KMSErrorSurfacedWithName: when the KMS provider's Decrypt
// fails, the error message must include the provider Name so an operator
// can tell whether it was sops, AWS KMS, or Vault that broke.
func TestNewIssuer_KMSErrorSurfacedWithName(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, err := testutil.WriteTestCA(dir)
	if err != nil {
		t.Fatalf("WriteTestCA: %v", err)
	}
	kms := failingKMS{err: errors.New("kms is offline")}
	_, err = NewIssuer(certPath, keyPath, kms)
	if err == nil {
		t.Fatal("expected error from failing kms")
	}
	if !strings.Contains(err.Error(), "failing") || !strings.Contains(err.Error(), "kms is offline") {
		t.Errorf("error must surface provider name + cause, got: %v", err)
	}
}

// TestCACertPEM_RoundTrip asserts CACertPEM() returns bytes that decode
// back into the same DER as the in-memory CA — i.e. we don't mutate the
// cert or strip extensions on the way out (binding for trust-store
// distribution).
func TestCACertPEM_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, err := testutil.WriteTestCA(dir)
	if err != nil {
		t.Fatalf("WriteTestCA: %v", err)
	}
	issuer, err := NewIssuer(certPath, keyPath, nil)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}

	got := issuer.CACertPEM()
	if len(got) == 0 {
		t.Fatal("CACertPEM returned empty")
	}
	block, _ := pem.Decode(got)
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatalf("CACertPEM output is not a CERTIFICATE PEM block")
	}
	parsed, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse CACertPEM output: %v", err)
	}
	if !parsed.Equal(issuer.caCert) {
		t.Error("CACertPEM round-trip changed the cert")
	}
}

// TestCACertExpiry returns the underlying CA NotAfter — must match the
// in-memory cert exactly (not a stale snapshot).
func TestCACertExpiry(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, err := testutil.WriteTestCA(dir)
	if err != nil {
		t.Fatalf("WriteTestCA: %v", err)
	}
	issuer, err := NewIssuer(certPath, keyPath, nil)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	got := issuer.CACertExpiry()
	if !got.Equal(issuer.caCert.NotAfter) {
		t.Errorf("CACertExpiry = %v, want %v", got, issuer.caCert.NotAfter)
	}
	// Test CA NotAfter is 1y out; sanity-check the value is in the future
	// so callers that alert on imminent expiry get a usable answer.
	if !got.After(time.Now()) {
		t.Errorf("CACertExpiry %v is not in the future", got)
	}
}

// TestEncryptDecryptPrivateKey_TamperedCiphertextRejected: GCM is an AEAD;
// flipping a single ciphertext bit must cause Open to fail authentication.
// Security-critical: this is what prevents a Redis row from being swapped.
func TestEncryptDecryptPrivateKey_TamperedCiphertextRejected(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, err := testutil.WriteTestCA(dir)
	if err != nil {
		t.Fatalf("WriteTestCA: %v", err)
	}
	issuer, err := NewIssuer(certPath, keyPath, nil)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	tlsCert, err := issuer.SignCert("encrypt-tamper.example.com")
	if err != nil {
		t.Fatalf("SignCert: %v", err)
	}
	orig := tlsCert.PrivateKey.(*ecdsa.PrivateKey)
	ct, nonce, err := issuer.EncryptPrivateKey(orig)
	if err != nil {
		t.Fatalf("EncryptPrivateKey: %v", err)
	}
	// Flip a bit in the ciphertext.
	ct[0] ^= 0x01
	if _, err := issuer.DecryptPrivateKey(ct, nonce); err == nil {
		t.Fatal("tampered ciphertext must fail GCM auth")
	}
}

// TestEncryptDecryptPrivateKey_DistinctNoncePerCall: GCM requires unique
// nonces per encryption under the same key — security-critical. Two
// successive Encrypt calls must produce different nonces (and therefore
// different ciphertexts even for identical plaintexts).
func TestEncryptDecryptPrivateKey_DistinctNoncePerCall(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, err := testutil.WriteTestCA(dir)
	if err != nil {
		t.Fatalf("WriteTestCA: %v", err)
	}
	issuer, err := NewIssuer(certPath, keyPath, nil)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	tlsCert, err := issuer.SignCert("nonce-uniq.example.com")
	if err != nil {
		t.Fatalf("SignCert: %v", err)
	}
	key := tlsCert.PrivateKey.(*ecdsa.PrivateKey)

	ct1, n1, err := issuer.EncryptPrivateKey(key)
	if err != nil {
		t.Fatalf("Encrypt#1: %v", err)
	}
	ct2, n2, err := issuer.EncryptPrivateKey(key)
	if err != nil {
		t.Fatalf("Encrypt#2: %v", err)
	}
	if string(n1) == string(n2) {
		t.Error("nonces must differ between Encrypt calls (GCM IV reuse = catastrophic)")
	}
	if string(ct1) == string(ct2) {
		t.Error("ciphertexts must differ between Encrypt calls of the same plaintext")
	}
}

// TestDecryptPrivateKey_WrongNonce: a valid ciphertext under a different
// nonce must fail authentication (GCM binds the nonce into the tag).
func TestDecryptPrivateKey_WrongNonce(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, err := testutil.WriteTestCA(dir)
	if err != nil {
		t.Fatalf("WriteTestCA: %v", err)
	}
	issuer, err := NewIssuer(certPath, keyPath, nil)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	tlsCert, _ := issuer.SignCert("wrong-nonce.example.com")
	key := tlsCert.PrivateKey.(*ecdsa.PrivateKey)
	ct, nonce, err := issuer.EncryptPrivateKey(key)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	// 12-byte alt nonce, definitely not the one that produced ct.
	other := make([]byte, len(nonce))
	for i := range other {
		other[i] = nonce[i] ^ 0xFF
	}
	if _, err := issuer.DecryptPrivateKey(ct, other); err == nil {
		t.Error("wrong nonce must fail GCM auth")
	}
}

func BenchmarkSignCert(b *testing.B) {
	dir := b.TempDir()
	certPath, keyPath, err := testutil.WriteTestCA(dir)
	if err != nil {
		b.Fatalf("WriteTestCA: %v", err)
	}
	issuer, err := NewIssuer(certPath, keyPath, nil)
	if err != nil {
		b.Fatalf("NewIssuer: %v", err)
	}

	b.ResetTimer()
	for range b.N {
		if _, err := issuer.SignCert("bench.example.com"); err != nil {
			b.Fatalf("SignCert: %v", err)
		}
	}
}

// TestNewIssuer_NoopProviderIsLegacyBehaviour: passing nil for the kms
// argument must produce identical behaviour to passing NoopProvider (raw PEM
// on disk). The other NewIssuer test cases pass nil; this test pins the
// explicit nil-to-noop-fallback contract in one place.
func TestNewIssuer_NoopProviderIsLegacyBehaviour(t *testing.T) {
	certPath, keyPath, err := testutil.WriteTestCA(t.TempDir())
	if err != nil {
		t.Fatalf("WriteTestCA: %v", err)
	}
	iss, err := NewIssuer(certPath, keyPath, nil)
	if err != nil {
		t.Fatalf("nil kms should fall back to noop, got error: %v", err)
	}
	if iss == nil {
		t.Fatal("expected non-nil issuer")
	}
}

// writeUnconstrainedTestCA writes a self-signed CA cert + key that is CA:TRUE
// but does NOT carry the pathlen:0 basic constraint — the shape of a proxy CA
// generated before pathlen:0 was added to the generation recipes. Used to
// exercise the load-time warning for such legacy CAs.
func writeUnconstrainedTestCA(t *testing.T, dir string) (certPath, keyPath string) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Unconstrained Test CA"},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		// No MaxPathLenZero: the basic constraints carry no path length limit.
	}
	caDER, err := x509.CreateCertificate(rand.Reader, template, template, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create CA cert: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(caKey)
	if err != nil {
		t.Fatalf("marshal CA key: %v", err)
	}
	certPath = filepath.Join(dir, "ca-cert.pem")
	keyPath = filepath.Join(dir, "ca-key.pem")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatalf("write CA cert: %v", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatalf("write CA key: %v", err)
	}
	return certPath, keyPath
}

// captureDefaultSlog swaps the process default slog logger for one writing to
// the returned buffer, restoring the original at test cleanup.
func captureDefaultSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// TestNewIssuer_WarnsOnCAWithoutPathLenZero: loading a CA:TRUE certificate
// without the pathlen:0 basic constraint must succeed (existing deployments
// may carry such a CA) but emit the operator warning telling them to
// regenerate — without pathlen:0 a stolen CA key can mint a subordinate CA.
func TestNewIssuer_WarnsOnCAWithoutPathLenZero(t *testing.T) {
	certPath, keyPath := writeUnconstrainedTestCA(t, t.TempDir())
	buf := captureDefaultSlog(t)

	iss, err := NewIssuer(certPath, keyPath, nil)
	if err != nil {
		t.Fatalf("NewIssuer must accept a legacy CA without pathlen:0, got: %v", err)
	}
	if iss == nil {
		t.Fatal("expected non-nil issuer")
	}
	out := buf.String()
	if !strings.Contains(out, "lacks the pathlen:0 basic constraint") {
		t.Errorf("expected pathlen:0 warning in log output, got: %q", out)
	}
	if !strings.Contains(out, certPath) {
		t.Errorf("warning should name the offending cert path %q, got: %q", certPath, out)
	}
}

// TestNewIssuer_NoWarningOnPathLenZeroCA: a CA carrying pathlen:0 (the shape
// produced by the current generation recipes) must load without the warning,
// so the warning stays a real signal rather than boot noise.
func TestNewIssuer_NoWarningOnPathLenZeroCA(t *testing.T) {
	certPath, keyPath, err := testutil.WriteTestCA(t.TempDir())
	if err != nil {
		t.Fatalf("WriteTestCA: %v", err)
	}
	buf := captureDefaultSlog(t)

	if _, err := NewIssuer(certPath, keyPath, nil); err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	if out := buf.String(); strings.Contains(out, "pathlen:0") {
		t.Errorf("unexpected pathlen warning for a pathlen:0 CA: %q", out)
	}
}
