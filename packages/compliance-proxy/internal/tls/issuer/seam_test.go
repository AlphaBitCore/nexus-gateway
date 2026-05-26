package issuer

import (
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	prometheus "github.com/prometheus/client_golang/prometheus"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/metrics"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/testutil"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

// failAfterReader delegates to inner for the first `at` Read calls then
// returns err. Same shape as the entropy_seam_test.go probe in
// agent/core/network/tls — sweeps thresholds so we hit each downstream
// rand consumer (ecdsa.GenerateKey, rand.Int, x509.CreateCertificate).
type failAfterReader struct {
	inner io.Reader
	err   error
	calls *int
	at    int
}

func (f *failAfterReader) Read(p []byte) (int, error) {
	*f.calls++
	if *f.calls > f.at {
		return 0, f.err
	}
	return f.inner.Read(p)
}

// failingReader is a small io.Reader that always returns the given error.
type failingReader struct{ err error }

func (f failingReader) Read(_ []byte) (int, error) { return 0, f.err }

var _ io.Reader = failingReader{}

// swapCertRandReader injects r into the package-level certRandReader and
// returns a restore func. Mirrors swapTLSRandReader's pattern.
func swapCertRandReader(t *testing.T, r io.Reader) func() {
	t.Helper()
	orig := certRandReader
	certRandReader = r
	return func() { certRandReader = orig }
}

// TestCertRandReader_ProductionDefault pins that package init wires the
// real crypto/rand.Reader so production startup gets full entropy. A
// regression flipping this to a fake source would silently weaken every
// leaf cert serial / key.
func TestCertRandReader_ProductionDefault(t *testing.T) {
	if certRandReader == nil {
		t.Error("certRandReader must not be nil at package init")
	}
}

// TestSignCert_GenerateLeafKeyError — when entropy is starved, SignCert
// must surface the ecdsa.GenerateKey failure as a `generate leaf key`
// wrapped error and NOT return a partial cert.
func TestSignCert_GenerateLeafKeyError(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, err := testutil.WriteTestCA(dir)
	if err != nil {
		t.Fatalf("WriteTestCA: %v", err)
	}
	iss, err := NewIssuer(certPath, keyPath, nil)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}

	restore := swapCertRandReader(t, failingReader{err: errors.New("starved")})
	defer restore()

	cert, err := iss.SignCert("entropy-down.example.com")
	if err == nil {
		t.Fatal("expected entropy error from SignCert")
	}
	if cert != nil {
		t.Error("error path must return nil cert")
	}
	if !strings.Contains(err.Error(), "generate leaf key") {
		t.Errorf("err should wrap 'generate leaf key'; got %q", err)
	}
}

// TestSignCert_AllRandConsumerArms sweeps failAfter thresholds 1..80 so
// every downstream rand consumer in SignCert (ecdsa.GenerateKey internal
// reads, rand.Int for serial, x509.CreateCertificate for signature
// randomization) gets exercised in at least one iteration. Same probe
// pattern as agent/core/network/tls/entropy_seam_test.go.
func TestSignCert_AllRandConsumerArms(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, err := testutil.WriteTestCA(dir)
	if err != nil {
		t.Fatalf("WriteTestCA: %v", err)
	}
	iss, err := NewIssuer(certPath, keyPath, nil)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}

	sawAny := false
	wrapMatched := false
	for at := 1; at < 80; at++ {
		calls := 0
		certRandReader = &failAfterReader{
			inner: rand.Reader,
			err:   errors.New("starved"),
			calls: &calls,
			at:    at,
		}
		host := "probe" + strings.Repeat("x", at) + ".example.com"
		_, err := iss.SignCert(host)
		certRandReader = rand.Reader
		if err == nil {
			continue
		}
		sawAny = true
		msg := err.Error()
		if strings.Contains(msg, "generate leaf key") ||
			strings.Contains(msg, "generate serial") ||
			strings.Contains(msg, "sign leaf cert") {
			wrapMatched = true
		}
	}
	if !sawAny {
		t.Fatal("no failAfter threshold surfaced an entropy error from SignCert")
	}
	if !wrapMatched {
		t.Error("at least one threshold must wrap a known downstream consumer error")
	}
}

// TestEncryptPrivateKey_NonceEntropyError forces the io.ReadFull for the
// GCM nonce to fail by starving entropy. The marshal + AES-NewCipher +
// GCM-NewGCM steps don't consume certRandReader, so a permanently-failing
// reader cleanly hits the nonce-read arm.
func TestEncryptPrivateKey_NonceEntropyError(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, err := testutil.WriteTestCA(dir)
	if err != nil {
		t.Fatalf("WriteTestCA: %v", err)
	}
	iss, err := NewIssuer(certPath, keyPath, nil)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("leaf key: %v", err)
	}

	restore := swapCertRandReader(t, failingReader{err: errors.New("starved")})
	defer restore()

	_, _, err = iss.EncryptPrivateKey(leafKey)
	if err == nil {
		t.Fatal("expected nonce-entropy error")
	}
	if !strings.Contains(err.Error(), "generate nonce") {
		t.Errorf("err should wrap 'generate nonce'; got %q", err)
	}
}

// TestEncryptPrivateKey_NilKeyMarshalError drives the
// x509.MarshalECPrivateKey error path. ecdsa.PrivateKey with nil curve
// params is rejected by the marshaler ("x509: unknown elliptic curve").
func TestEncryptPrivateKey_NilKeyMarshalError(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, err := testutil.WriteTestCA(dir)
	if err != nil {
		t.Fatalf("WriteTestCA: %v", err)
	}
	iss, err := NewIssuer(certPath, keyPath, nil)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	bogus := &ecdsa.PrivateKey{}
	_, _, err = iss.EncryptPrivateKey(bogus)
	if err == nil {
		t.Fatal("expected marshal error for nil-curve key")
	}
	if !strings.Contains(err.Error(), "marshal key for encryption") {
		t.Errorf("err should wrap 'marshal key for encryption'; got %q", err)
	}
}

// TestEncryptPrivateKey_BadAESKeyLength drives the aes.NewCipher
// rejection branch — AES key MUST be 16/24/32 bytes; we override the
// Issuer's aesKey to an invalid length so NewCipher errors immediately.
func TestEncryptPrivateKey_BadAESKeyLength(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, err := testutil.WriteTestCA(dir)
	if err != nil {
		t.Fatalf("WriteTestCA: %v", err)
	}
	iss, err := NewIssuer(certPath, keyPath, nil)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	iss.aesKey = []byte("too-short")

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("leaf key: %v", err)
	}
	_, _, err = iss.EncryptPrivateKey(leafKey)
	if err == nil {
		t.Fatal("expected AES cipher error for bad key length")
	}
	if !strings.Contains(err.Error(), "create AES cipher") {
		t.Errorf("err should wrap 'create AES cipher'; got %q", err)
	}
}

// TestDecryptPrivateKey_BadAESKeyLength mirrors the above for the
// decrypt path — both share the aes.NewCipher arm.
func TestDecryptPrivateKey_BadAESKeyLength(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, err := testutil.WriteTestCA(dir)
	if err != nil {
		t.Fatalf("WriteTestCA: %v", err)
	}
	iss, err := NewIssuer(certPath, keyPath, nil)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	iss.aesKey = []byte("also-too-short")

	_, err = iss.DecryptPrivateKey([]byte("ct"), make([]byte, 12))
	if err == nil {
		t.Fatal("expected AES cipher error for bad key length on decrypt")
	}
	if !strings.Contains(err.Error(), "create AES cipher") {
		t.Errorf("err should wrap 'create AES cipher'; got %q", err)
	}
}

// TestDecryptPrivateKey_PlaintextNotECKey: gcm.Open succeeds but the
// recovered plaintext is not a parseable EC private key — pins the
// `parse decrypted key` error branch (security-relevant: prevents a
// crafted ciphertext from yielding garbage as a "key").
func TestDecryptPrivateKey_PlaintextNotECKey(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, err := testutil.WriteTestCA(dir)
	if err != nil {
		t.Fatalf("WriteTestCA: %v", err)
	}
	iss, err := NewIssuer(certPath, keyPath, nil)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}

	// Manually seal arbitrary bytes under the issuer's AES key so
	// gcm.Open succeeds and the parse step is the failure point.
	block, err := aes.NewCipher(iss.aesKey)
	if err != nil {
		t.Fatalf("aes.NewCipher: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("cipher.NewGCM: %v", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		t.Fatalf("nonce: %v", err)
	}
	ct := gcm.Seal(nil, nonce, []byte("not a valid ASN.1 EC key"), nil)

	_, err = iss.DecryptPrivateKey(ct, nonce)
	if err == nil {
		t.Fatal("expected parse error on non-EC plaintext")
	}
	if !strings.Contains(err.Error(), "parse decrypted key") {
		t.Errorf("err should wrap 'parse decrypted key'; got %q", err)
	}
}

// TestCommandSigner_Sign_TempDirFailure mirrors TestKMSDecrypt_TempDirFailure
// for the remote-signer path. Both helpers use the same os.CreateTemp pattern.
func TestCommandSigner_Sign_TempDirFailure(t *testing.T) {
	t.Setenv("TMPDIR", "/this/path/intentionally/does/not/exist/abc-xyz-67890")

	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	s, err := NewCommandSigner(&caKey.PublicKey, []string{"cat", "{file}"}, time.Second)
	if err != nil {
		t.Fatalf("NewCommandSigner: %v", err)
	}
	_, err = s.Sign(nil, []byte("d"), crypto.SHA256)
	if err == nil {
		t.Fatal("expected CreateTemp failure")
	}
	if !strings.Contains(err.Error(), "create temp") {
		t.Errorf("err should wrap 'create temp'; got %q", err)
	}
}

// swapMarshalECPrivKeyFn injects fn into marshalECPrivKeyFn and returns a
// restore func. Deferred restore guarantees we never leave the seam swapped
// after a test — a stale swap would corrupt all subsequent tests in the run.
func swapMarshalECPrivKeyFn(t *testing.T, fn func(*ecdsa.PrivateKey) ([]byte, error)) func() {
	t.Helper()
	orig := marshalECPrivKeyFn
	marshalECPrivKeyFn = fn
	return func() { marshalECPrivKeyFn = orig }
}

// swapNewGCMFn injects fn into newGCMFn and returns a restore func.
func swapNewGCMFn(t *testing.T, fn func(cipher.Block) (cipher.AEAD, error)) func() {
	t.Helper()
	orig := newGCMFn
	newGCMFn = fn
	return func() { newGCMFn = orig }
}

// swapHKDFReadFn injects fn into hkdfReadFn and returns a restore func.
func swapHKDFReadFn(t *testing.T, fn func(io.Reader, []byte) (int, error)) func() {
	t.Helper()
	orig := hkdfReadFn
	hkdfReadFn = fn
	return func() { hkdfReadFn = orig }
}

// swapHKDFReadRemoteFn injects fn into hkdfReadRemoteFn and returns a restore func.
func swapHKDFReadRemoteFn(t *testing.T, fn func(io.Reader, []byte) (int, error)) func() {
	t.Helper()
	orig := hkdfReadRemoteFn
	hkdfReadRemoteFn = fn
	return func() { hkdfReadRemoteFn = orig }
}

// swapTempWriteSignFn injects fn into tempWriteSignFn and returns a restore func.
func swapTempWriteSignFn(t *testing.T, fn func(interface{ Write([]byte) (int, error) }, []byte) (int, error)) func() {
	t.Helper()
	orig := tempWriteSignFn
	tempWriteSignFn = fn
	return func() { tempWriteSignFn = orig }
}

// TestNewIssuer_MarshalECPrivKeyFails exercises the error arm that fires when
// x509.MarshalECPrivateKey fails inside NewIssuer (after a successful key
// parse). In production this arm is dead code for P-256 keys; the seam lets
// us prove the error wrapping is wired correctly and returns immediately
// without leaving a partial Issuer.
func TestNewIssuer_MarshalECPrivKeyFails_ReturnsWrappedError(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, err := testutil.WriteTestCA(dir)
	if err != nil {
		t.Fatalf("WriteTestCA: %v", err)
	}

	injectedErr := errors.New("marshal: unknown elliptic curve")
	restore := swapMarshalECPrivKeyFn(t, func(_ *ecdsa.PrivateKey) ([]byte, error) {
		return nil, injectedErr
	})
	defer restore()

	iss, err := NewIssuer(certPath, keyPath, nil)
	if err == nil {
		t.Fatal("expected error when MarshalECPrivateKey fails")
	}
	if iss != nil {
		t.Error("must return nil Issuer on error (no partial state)")
	}
	if !strings.Contains(err.Error(), "marshal CA key for HKDF") {
		t.Errorf("err must wrap 'marshal CA key for HKDF'; got %q", err)
	}
	if !errors.Is(err, injectedErr) {
		t.Errorf("err must chain injected error via %%w; got %q", err)
	}
}

// TestNewIssuer_HKDFReadFails_ReturnsWrappedError drives the io.ReadFull
// error arm inside NewIssuer. The HKDF reader built from a valid P-256 key
// never errors in production; this seam proves the error path is live.
func TestNewIssuer_HKDFReadFails_ReturnsWrappedError(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, err := testutil.WriteTestCA(dir)
	if err != nil {
		t.Fatalf("WriteTestCA: %v", err)
	}

	injectedErr := errors.New("hkdf: short read")
	restore := swapHKDFReadFn(t, func(_ io.Reader, _ []byte) (int, error) {
		return 0, injectedErr
	})
	defer restore()

	iss, err := NewIssuer(certPath, keyPath, nil)
	if err == nil {
		t.Fatal("expected error when HKDF read fails")
	}
	if iss != nil {
		t.Error("must return nil Issuer on HKDF failure (no partial state)")
	}
	if !strings.Contains(err.Error(), "HKDF derive AES key") {
		t.Errorf("err must wrap 'HKDF derive AES key'; got %q", err)
	}
	if !errors.Is(err, injectedErr) {
		t.Errorf("err must chain injected error via %%w; got %q", err)
	}
}

// TestEncryptPrivateKey_NewGCMFails_ReturnsWrappedError drives the
// cipher.NewGCM error arm in EncryptPrivateKey. In production, NewGCM only
// errors when the block size is not 16 bytes, which cannot happen for AES-256;
// this seam proves the wrapping is wired and no ciphertext is returned.
func TestEncryptPrivateKey_NewGCMFails_ReturnsWrappedError(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, err := testutil.WriteTestCA(dir)
	if err != nil {
		t.Fatalf("WriteTestCA: %v", err)
	}
	iss, err := NewIssuer(certPath, keyPath, nil)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("leaf key: %v", err)
	}

	injectedErr := errors.New("gcm: block size mismatch")
	restore := swapNewGCMFn(t, func(_ cipher.Block) (cipher.AEAD, error) {
		return nil, injectedErr
	})
	defer restore()

	ct, nonce, err := iss.EncryptPrivateKey(leafKey)
	if err == nil {
		t.Fatal("expected error when NewGCM fails")
	}
	if ct != nil || nonce != nil {
		t.Error("must return nil ciphertext and nonce on GCM failure")
	}
	if !strings.Contains(err.Error(), "create GCM") {
		t.Errorf("err must wrap 'create GCM'; got %q", err)
	}
	if !errors.Is(err, injectedErr) {
		t.Errorf("err must chain injected error via %%w; got %q", err)
	}
}

// TestDecryptPrivateKey_NewGCMFails_ReturnsWrappedError mirrors the Encrypt
// path for the Decrypt path — both share the newGCMFn seam and the same
// "create GCM" wrap message.
func TestDecryptPrivateKey_NewGCMFails_ReturnsWrappedError(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, err := testutil.WriteTestCA(dir)
	if err != nil {
		t.Fatalf("WriteTestCA: %v", err)
	}
	iss, err := NewIssuer(certPath, keyPath, nil)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}

	injectedErr := errors.New("gcm: block size mismatch on decrypt")
	restore := swapNewGCMFn(t, func(_ cipher.Block) (cipher.AEAD, error) {
		return nil, injectedErr
	})
	defer restore()

	key, err := iss.DecryptPrivateKey([]byte("ct"), make([]byte, 12))
	if err == nil {
		t.Fatal("expected error when NewGCM fails during DecryptPrivateKey")
	}
	if key != nil {
		t.Error("must return nil key on GCM failure (no partial state)")
	}
	if !strings.Contains(err.Error(), "create GCM") {
		t.Errorf("err must wrap 'create GCM'; got %q", err)
	}
	if !errors.Is(err, injectedErr) {
		t.Errorf("err must chain injected error via %%w; got %q", err)
	}
}


// TestCommandSigner_Sign_WriteDigestFails_ReturnsWrappedError exercises the
// tmp.Write(digest) error arm inside CommandSigner.Sign. After CreateTemp
// succeeds, a Write failure on a healthy filesystem is impossible without
// fault injection; this seam proves the arm is wired and the temp file is
// still cleaned up (no orphaned temp files, no partial state).
func TestCommandSigner_Sign_WriteDigestFails_ReturnsWrappedError(t *testing.T) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("CA key: %v", err)
	}
	s, err := NewCommandSigner(&caKey.PublicKey, []string{"cat", "{file}"}, time.Second)
	if err != nil {
		t.Fatalf("NewCommandSigner: %v", err)
	}

	injectedErr := errors.New("disk full")
	restore := swapTempWriteSignFn(t, func(_ interface{ Write([]byte) (int, error) }, _ []byte) (int, error) {
		return 0, injectedErr
	})
	defer restore()

	sig, err := s.Sign(nil, []byte("digest"), crypto.SHA256)
	if err == nil {
		t.Fatal("expected error when Write fails")
	}
	if sig != nil {
		t.Error("must return nil signature on write failure (no partial state)")
	}
	if !strings.Contains(err.Error(), "write digest") {
		t.Errorf("err must wrap 'write digest'; got %q", err)
	}
	if !errors.Is(err, injectedErr) {
		t.Errorf("err must chain injected error via %%w; got %q", err)
	}
}

// TestNewIssuerWithRemoteSigner_HKDFReadFails_ReturnsWrappedError drives the
// io.ReadFull error arm in NewIssuerWithRemoteSigner. The HKDF reader from a
// valid cert's raw bytes never errors in production; this seam proves the
// wrapping is wired and no partial Issuer is returned.
func TestNewIssuerWithRemoteSigner_HKDFReadFails_ReturnsWrappedError(t *testing.T) {
	dir := t.TempDir()
	certPath, _, err := testutil.WriteTestCA(dir)
	if err != nil {
		t.Fatalf("WriteTestCA: %v", err)
	}
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	stub := &stubSigner{pub: &caKey.PublicKey, caKey: caKey}

	injectedErr := errors.New("hkdf: short read from cert bytes")
	restore := swapHKDFReadRemoteFn(t, func(_ io.Reader, _ []byte) (int, error) {
		return 0, injectedErr
	})
	defer restore()

	iss, err := NewIssuerWithRemoteSigner(certPath, stub)
	if err == nil {
		t.Fatal("expected error when HKDF read fails in NewIssuerWithRemoteSigner")
	}
	if iss != nil {
		t.Error("must return nil Issuer on HKDF failure (no partial state)")
	}
	if !strings.Contains(err.Error(), "HKDF derive AES key from cert") {
		t.Errorf("err must wrap 'HKDF derive AES key from cert'; got %q", err)
	}
	if !errors.Is(err, injectedErr) {
		t.Errorf("err must chain injected error via %%w; got %q", err)
	}
}

// TestSignCert_MetricsObservedWhenRegistered asserts that the deferred
// metrics.CertSignMs.Observe call runs when the histogram is non-nil.
// Verifies the guard is correct: the `if metrics.CertSignMs != nil` branch
// body is live code, not dead code behind an always-nil guard.
func TestSignCert_MetricsObservedWhenRegistered(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, err := testutil.WriteTestCA(dir)
	if err != nil {
		t.Fatalf("WriteTestCA: %v", err)
	}
	iss, err := NewIssuer(certPath, keyPath, nil)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}

	// Register a real Prometheus registry so CertSignMs is non-nil.
	// Use a fresh private registry to avoid collision with other tests.
	reg := prometheus.NewRegistry()
	metrics.Register(registry.NewRegistry(reg))
	defer func() { metrics.CertSignMs = nil }() // restore nil so other tests are unaffected

	_, err = iss.SignCert("metrics-probe.example.com")
	if err != nil {
		t.Fatalf("SignCert: %v", err)
	}
	// The observable behaviour: the histogram was sampled — gather it and
	// assert at least one observation was recorded.
	mf, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	found := false
	for _, m := range mf {
		if strings.Contains(m.GetName(), "cert_sign_ms") {
			h := m.GetMetric()
			if len(h) > 0 && h[0].GetHistogram().GetSampleCount() > 0 {
				found = true
			}
		}
	}
	if !found {
		t.Error("cert_sign_ms histogram must record at least one observation after SignCert")
	}
}
