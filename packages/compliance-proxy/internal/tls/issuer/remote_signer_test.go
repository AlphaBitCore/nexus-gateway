package issuer

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/testutil"
)

// stubSigner is a test double for crypto.Signer that records the digest it
// was asked to sign and returns a canned signature. Used so SignCert can
// exercise the remoteSigner != nil branch without shelling out.
type stubSigner struct {
	pub      crypto.PublicKey
	caKey    *ecdsa.PrivateKey // backing key used to produce real signatures
	digestIn []byte
	signErr  error
}

func (s *stubSigner) Public() crypto.PublicKey { return s.pub }

func (s *stubSigner) Sign(rnd io.Reader, digest []byte, opts crypto.SignerOpts) ([]byte, error) {
	s.digestIn = append([]byte(nil), digest...)
	if s.signErr != nil {
		return nil, s.signErr
	}
	// Sign with the underlying CA key so x509.CreateCertificate produces a
	// chain that verifies (otherwise the SignCert smoke would still pass
	// the unit assertions, but a Verify() inside the test would fail).
	return s.caKey.Sign(rnd, digest, opts)
}

// TestNewCommandSigner_RejectsEmptyArgs pins the constructor's input
// validation — the same shape as NewCommandProvider (siblings in this file).
// Wrong args = total failure to sign anything; the constructor must catch.
func TestNewCommandSigner_RejectsEmptyArgs(t *testing.T) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("CA key: %v", err)
	}
	if _, err := NewCommandSigner(&caKey.PublicKey, nil, 5*time.Second); err == nil {
		t.Error("nil args must error")
	}
	if _, err := NewCommandSigner(&caKey.PublicKey, []string{}, 5*time.Second); err == nil {
		t.Error("empty args must error")
	}
}

// TestNewCommandSigner_DefaultTimeout matches NewCommandProvider — timeout
// of 0 is interpreted as "use the default 10s" so a misconfigured field
// can't wedge proxy startup with an instant cancellation loop.
func TestNewCommandSigner_DefaultTimeout(t *testing.T) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("CA key: %v", err)
	}
	s, err := NewCommandSigner(&caKey.PublicKey, []string{"echo", "ok"}, 0)
	if err != nil {
		t.Fatalf("NewCommandSigner: %v", err)
	}
	if s.timeout != 10*time.Second {
		t.Errorf("timeout = %v, want default 10s", s.timeout)
	}
}

// TestCommandSigner_Public returns the public key handed to the constructor
// verbatim — x509.CreateCertificate calls Public() to encode the issuer's
// authority key ID.
func TestCommandSigner_Public(t *testing.T) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("CA key: %v", err)
	}
	s, err := NewCommandSigner(&caKey.PublicKey, []string{"true"}, time.Second)
	if err != nil {
		t.Fatalf("NewCommandSigner: %v", err)
	}
	pub, ok := s.Public().(*ecdsa.PublicKey)
	if !ok {
		t.Fatalf("Public() returned non-ECDSA: %T", s.Public())
	}
	if !pub.Equal(&caKey.PublicKey) {
		t.Error("Public() does not match the key handed to NewCommandSigner")
	}
}

// TestCommandSigner_Sign_Echo verifies the happy-path Sign call: the
// command shell-out succeeds, the temp digest file is written + cleaned
// up, and the stdout bytes are returned as the signature. We use `cat
// {file}` so the "signature" is actually the digest — the assertion is on
// the round-trip of bytes through the temp-file + arg-substitution path.
func TestCommandSigner_Sign_Echo(t *testing.T) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("CA key: %v", err)
	}
	s, err := NewCommandSigner(&caKey.PublicKey, []string{"cat", "{file}"}, 5*time.Second)
	if err != nil {
		t.Fatalf("NewCommandSigner: %v", err)
	}
	digest := []byte("a-digest-32-bytes-long-padding!!")
	sig, err := s.Sign(nil, digest, crypto.SHA256)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if string(sig) != string(digest) {
		t.Errorf("Sign returned %q, want round-trip of digest %q", sig, digest)
	}
}

// TestCommandSigner_Sign_NonZeroExitSurfacesStderr — when the KMS command
// fails, the error must include both the command name AND any stderr so
// an operator can diagnose an "auth refused" KMS failure without enabling
// debug logging. Same operational contract as CommandProvider.Decrypt.
func TestCommandSigner_Sign_NonZeroExitSurfacesStderr(t *testing.T) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("CA key: %v", err)
	}
	// `sh -c "echo boom 1>&2; exit 7"` writes "boom" to stderr and exits
	// non-zero — exercises the *exec.ExitError + Stderr arm explicitly.
	s, err := NewCommandSigner(
		&caKey.PublicKey,
		[]string{"sh", "-c", "echo boom 1>&2; exit 7"},
		5*time.Second,
	)
	if err != nil {
		t.Fatalf("NewCommandSigner: %v", err)
	}
	_, err = s.Sign(nil, []byte("d"), crypto.SHA256)
	if err == nil {
		t.Fatal("expected error from non-zero exit")
	}
	msg := err.Error()
	if !strings.Contains(msg, "sh") {
		t.Errorf("err should mention command name; got %q", msg)
	}
	if !strings.Contains(msg, "boom") {
		t.Errorf("err should surface stderr 'boom'; got %q", msg)
	}
}

// TestCommandSigner_Sign_NonExistentCommand drives the err path where the
// process can't be started at all (not an ExitError) — exec.Cmd.Output
// returns a *os.PathError / *exec.Error that's NOT an ExitError, so the
// Stderr branch is skipped and the wrapped error still surfaces cleanly.
func TestCommandSigner_Sign_NonExistentCommand(t *testing.T) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("CA key: %v", err)
	}
	s, err := NewCommandSigner(
		&caKey.PublicKey,
		[]string{"/this/binary/does/not/exist"},
		5*time.Second,
	)
	if err != nil {
		t.Fatalf("NewCommandSigner: %v", err)
	}
	_, err = s.Sign(nil, []byte("d"), crypto.SHA256)
	if err == nil {
		t.Fatal("expected error launching missing binary")
	}
	if !strings.Contains(err.Error(), "this/binary/does/not/exist") {
		t.Errorf("err should mention missing binary path; got %q", err)
	}
}

// TestCommandSigner_Sign_EmptyOutputFails — an empty signature is treated
// as a misconfiguration (same shape as KMS Decrypt's empty-output guard).
// `true` exits 0 but writes nothing, exercising the len(out)==0 branch
// AFTER the cmd.Output success path.
func TestCommandSigner_Sign_EmptyOutputFails(t *testing.T) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("CA key: %v", err)
	}
	s, err := NewCommandSigner(&caKey.PublicKey, []string{"true"}, 5*time.Second)
	if err != nil {
		t.Fatalf("NewCommandSigner: %v", err)
	}
	_, err = s.Sign(nil, []byte("d"), crypto.SHA256)
	if err == nil {
		t.Fatal("expected error for empty signature")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("err should mention 'empty'; got %q", err)
	}
}

// TestNewIssuerWithRemoteSigner happy path: CA cert loads, AES key is
// derived deterministically from the cert raw bytes (via hkdfFromBytes),
// caKey is nil, remoteSigner is wired in, and AES key has the expected
// 32-byte length.
func TestNewIssuerWithRemoteSigner_HappyPath(t *testing.T) {
	dir := t.TempDir()
	certPath, _, err := testutil.WriteTestCA(dir)
	if err != nil {
		t.Fatalf("WriteTestCA: %v", err)
	}
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	stub := &stubSigner{pub: &caKey.PublicKey, caKey: caKey}

	iss, err := NewIssuerWithRemoteSigner(certPath, stub)
	if err != nil {
		t.Fatalf("NewIssuerWithRemoteSigner: %v", err)
	}
	if iss.caCert == nil {
		t.Fatal("caCert is nil")
	}
	if iss.caKey != nil {
		t.Errorf("caKey should be nil in remote-signer mode; got %v", iss.caKey)
	}
	if iss.remoteSigner == nil {
		t.Error("remoteSigner should be wired in")
	}
	if len(iss.aesKey) != 32 {
		t.Errorf("aesKey length = %d, want 32", len(iss.aesKey))
	}
}

// TestNewIssuerWithRemoteSigner_AESKeyDeterministic verifies the AES key
// derivation is purely a function of the CA cert raw bytes — two
// constructions over the same cert produce the same key (so a restart can
// still decrypt the existing Redis cache).
func TestNewIssuerWithRemoteSigner_AESKeyDeterministic(t *testing.T) {
	dir := t.TempDir()
	certPath, _, err := testutil.WriteTestCA(dir)
	if err != nil {
		t.Fatalf("WriteTestCA: %v", err)
	}
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	stub := &stubSigner{pub: &caKey.PublicKey, caKey: caKey}

	iss1, err := NewIssuerWithRemoteSigner(certPath, stub)
	if err != nil {
		t.Fatalf("NewIssuerWithRemoteSigner #1: %v", err)
	}
	iss2, err := NewIssuerWithRemoteSigner(certPath, stub)
	if err != nil {
		t.Fatalf("NewIssuerWithRemoteSigner #2: %v", err)
	}
	if string(iss1.aesKey) != string(iss2.aesKey) {
		t.Error("AES key must be deterministic per CA cert (so caches survive restart)")
	}
}

// TestNewIssuerWithRemoteSigner_DifferentCertsYieldDifferentKeys — when
// the operator rotates the CA, the AES key MUST change so the old
// ciphertexts can't be re-used. This is the binding behaviour the
// docstring promises.
func TestNewIssuerWithRemoteSigner_DifferentCertsYieldDifferentKeys(t *testing.T) {
	dirA := t.TempDir()
	certA, _, err := testutil.WriteTestCA(dirA)
	if err != nil {
		t.Fatalf("WriteTestCA A: %v", err)
	}
	dirB := t.TempDir()
	certB, _, err := testutil.WriteTestCA(dirB)
	if err != nil {
		t.Fatalf("WriteTestCA B: %v", err)
	}
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	stub := &stubSigner{pub: &caKey.PublicKey, caKey: caKey}

	issA, err := NewIssuerWithRemoteSigner(certA, stub)
	if err != nil {
		t.Fatalf("NewIssuerWithRemoteSigner A: %v", err)
	}
	issB, err := NewIssuerWithRemoteSigner(certB, stub)
	if err != nil {
		t.Fatalf("NewIssuerWithRemoteSigner B: %v", err)
	}
	if string(issA.aesKey) == string(issB.aesKey) {
		t.Error("different CA certs MUST yield different AES keys (rotation safety)")
	}
}

// TestNewIssuerWithRemoteSigner_BadCertPath — file doesn't exist, must
// produce a clean error mentioning the path so the operator sees what's
// missing.
func TestNewIssuerWithRemoteSigner_BadCertPath(t *testing.T) {
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	stub := &stubSigner{pub: &caKey.PublicKey, caKey: caKey}
	_, err := NewIssuerWithRemoteSigner("/nonexistent/ca.pem", stub)
	if err == nil {
		t.Fatal("expected error for missing CA file")
	}
	if !strings.Contains(err.Error(), "read CA cert") {
		t.Errorf("err should wrap 'read CA cert'; got %q", err)
	}
}

// TestNewIssuerWithRemoteSigner_BadCertPEM — file exists but contains no
// CERTIFICATE PEM block. Must be rejected at the same gate as NewIssuer.
func TestNewIssuerWithRemoteSigner_BadCertPEM(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(certPath, []byte("not pem"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	stub := &stubSigner{pub: &caKey.PublicKey, caKey: caKey}
	_, err := NewIssuerWithRemoteSigner(certPath, stub)
	if err == nil || !strings.Contains(err.Error(), "no CERTIFICATE PEM block") {
		t.Errorf("want 'no CERTIFICATE PEM block' error; got %v", err)
	}
}

// TestNewIssuerWithRemoteSigner_WrongCertPEMType — PEM block exists but the
// Type field is wrong. Must fail the CERTIFICATE check.
func TestNewIssuerWithRemoteSigner_WrongCertPEMType(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.pem")
	bogus := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: []byte("whatever")})
	if err := os.WriteFile(certPath, bogus, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	stub := &stubSigner{pub: &caKey.PublicKey, caKey: caKey}
	_, err := NewIssuerWithRemoteSigner(certPath, stub)
	if err == nil || !strings.Contains(err.Error(), "no CERTIFICATE PEM block") {
		t.Errorf("want PEM-type rejection; got %v", err)
	}
}

// TestNewIssuerWithRemoteSigner_CertParseFails — PEM block has correct
// Type but bytes aren't a parseable X.509 cert.
func TestNewIssuerWithRemoteSigner_CertParseFails(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.pem")
	bogus := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("not-asn1-der")})
	if err := os.WriteFile(certPath, bogus, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	stub := &stubSigner{pub: &caKey.PublicKey, caKey: caKey}
	_, err := NewIssuerWithRemoteSigner(certPath, stub)
	if err == nil || !strings.Contains(err.Error(), "parse CA cert") {
		t.Errorf("want 'parse CA cert' error; got %v", err)
	}
}

// TestSignCert_UsesRemoteSigner end-to-end: SignCert should pick the
// remote signer (not local caKey, which is nil here) and produce a chain
// that verifies against the CA. This is the only test that hits the
// `i.remoteSigner != nil` branch in issuer.SignCert.
func TestSignCert_UsesRemoteSigner(t *testing.T) {
	dir := t.TempDir()
	// Generate a CA via testutil and load the private key off disk so the
	// stub signer can produce real signatures over the leaf TBS bytes.
	certPath, keyPath, err := testutil.WriteTestCA(dir)
	if err != nil {
		t.Fatalf("WriteTestCA: %v", err)
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("read key: %v", err)
	}
	block, _ := pem.Decode(keyPEM)
	caKey, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		t.Fatalf("parse CA key: %v", err)
	}
	stub := &stubSigner{pub: &caKey.PublicKey, caKey: caKey}

	iss, err := NewIssuerWithRemoteSigner(certPath, stub)
	if err != nil {
		t.Fatalf("NewIssuerWithRemoteSigner: %v", err)
	}
	tlsCert, err := iss.SignCert("remote-signed.example.com")
	if err != nil {
		t.Fatalf("SignCert: %v", err)
	}

	// The stub must have been called — observable evidence the remote
	// signer branch ran (otherwise the local caKey path would have been
	// taken, but caKey is nil here so a regression would Fatalf earlier).
	if len(stub.digestIn) == 0 {
		t.Error("remote signer Sign was not invoked")
	}

	// Chain must verify against the CA we loaded.
	roots := x509.NewCertPool()
	roots.AddCert(iss.caCert)
	leaf, err := x509.ParseCertificate(tlsCert.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:     roots,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Errorf("remote-signed leaf must verify against CA; got %v", err)
	}
}

// TestSignCert_RemoteSignerError surfaces a sign-failure as a
// CreateCertificate error — pins that we don't silently produce a chain
// when the remote KMS is down.
func TestSignCert_RemoteSignerError(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, err := testutil.WriteTestCA(dir)
	if err != nil {
		t.Fatalf("WriteTestCA: %v", err)
	}
	keyPEM, _ := os.ReadFile(keyPath)
	block, _ := pem.Decode(keyPEM)
	caKey, _ := x509.ParseECPrivateKey(block.Bytes)
	stub := &stubSigner{
		pub:     &caKey.PublicKey,
		caKey:   caKey,
		signErr: errSimulatedKMSDown,
	}
	iss, err := NewIssuerWithRemoteSigner(certPath, stub)
	if err != nil {
		t.Fatalf("NewIssuerWithRemoteSigner: %v", err)
	}
	_, err = iss.SignCert("remote-fails.example.com")
	if err == nil {
		t.Fatal("expected SignCert to error when remote signer Sign returns error")
	}
	if !strings.Contains(err.Error(), "sign leaf cert") {
		t.Errorf("err should wrap 'sign leaf cert'; got %q", err)
	}
}

// TestHkdfFromBytes_DeterministicAndKeyStreamLength: hkdfFromBytes is the
// shared helper for NewIssuerWithRemoteSigner; verify it produces a
// deterministic, sufficiently-long keystream so the consumer can pull a
// full 32-byte AES key.
func TestHkdfFromBytes_DeterministicAndKeyStreamLength(t *testing.T) {
	seed := []byte("a-test-seed-of-arbitrary-length-1234567890")
	r1 := hkdfFromBytes(seed)
	r2 := hkdfFromBytes(seed)
	buf1 := make([]byte, 32)
	if _, err := io.ReadFull(r1, buf1); err != nil {
		t.Fatalf("read r1: %v", err)
	}
	buf2 := make([]byte, 32)
	if _, err := io.ReadFull(r2, buf2); err != nil {
		t.Fatalf("read r2: %v", err)
	}
	if string(buf1) != string(buf2) {
		t.Error("hkdfFromBytes must be deterministic for the same seed")
	}
	// Different seed must produce different keystream.
	r3 := hkdfFromBytes([]byte("other-seed"))
	buf3 := make([]byte, 32)
	if _, err := io.ReadFull(r3, buf3); err != nil {
		t.Fatalf("read r3: %v", err)
	}
	if string(buf1) == string(buf3) {
		t.Error("different seeds must produce different keystream")
	}
}

// errSimulatedKMSDown is a sentinel for the SignCert_RemoteSignerError test
// — extracted so the import block doesn't need 'errors' twice.
var errSimulatedKMSDown = stubError("simulated KMS down")

type stubError string

func (e stubError) Error() string { return string(e) }
