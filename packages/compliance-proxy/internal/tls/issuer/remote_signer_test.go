package issuer

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/testutil"
)

// --- Fakes for the cert-cache DEK bootstrap (CertCacheDEKStore + KMS) ---

// inMemoryDEKStore is a fake CertCacheDEKStore backing the wrapped DEK blob
// in memory so tests can drive the first-boot / subsequent-boot / race-lost /
// Redis-error branches deterministically without a live Redis.
type inMemoryDEKStore struct {
	blob []byte // nil = key absent

	getErr   error // error from every GetWrappedDEK call
	reGetErr error // error returned ONLY on the post-race (2nd+) GetWrappedDEK
	setErr   error // error from SetWrappedDEKIfAbsent

	// forceLostRace makes SetWrappedDEKIfAbsent report won=false (as if a
	// concurrent instance wrote first) and installs raceWinnerBlob as the
	// value the winner persisted.
	forceLostRace  bool
	raceWinnerBlob []byte

	getCalls int
	setCalls int
}

func (s *inMemoryDEKStore) GetWrappedDEK(_ context.Context) ([]byte, bool, error) {
	s.getCalls++
	if s.reGetErr != nil && s.getCalls >= 2 {
		return nil, false, s.reGetErr
	}
	if s.getErr != nil {
		return nil, false, s.getErr
	}
	if s.blob == nil {
		return nil, false, nil
	}
	return s.blob, true, nil
}

func (s *inMemoryDEKStore) SetWrappedDEKIfAbsent(_ context.Context, blob []byte) (bool, error) {
	s.setCalls++
	if s.setErr != nil {
		return false, s.setErr
	}
	if s.forceLostRace {
		s.blob = s.raceWinnerBlob // the winner's blob now stands
		return false, nil
	}
	if s.blob != nil {
		return false, nil
	}
	s.blob = blob
	return true, nil
}

// fakeKMS implements both kms.Encryptor and kms.KMSProvider with injectable
// encrypt/decrypt behaviour. The default identity helpers wrap with a "WRAP:"
// prefix so a wrapped blob is observably different from the DEK yet
// round-trips, mirroring a real envelope.
type fakeKMS struct {
	name string
	enc  func([]byte) ([]byte, error)
	dec  func([]byte) ([]byte, error)
}

func (f *fakeKMS) Name() string {
	if f.name != "" {
		return f.name
	}
	return "fake-kms"
}

func (f *fakeKMS) Encrypt(_ context.Context, pt []byte) ([]byte, error) { return f.enc(pt) }
func (f *fakeKMS) Decrypt(_ context.Context, ct []byte) ([]byte, error) { return f.dec(ct) }

const wrapPrefix = "WRAP:"

// identityKMS returns a fakeKMS whose Encrypt/Decrypt round-trip via a
// "WRAP:" prefix — a faithful stand-in for a real KMS envelope.
func identityKMS() *fakeKMS {
	return &fakeKMS{
		enc: func(b []byte) ([]byte, error) { return append([]byte(wrapPrefix), b...), nil },
		dec: func(b []byte) ([]byte, error) {
			if !bytes.HasPrefix(b, []byte(wrapPrefix)) {
				return nil, errors.New("fake-kms: blob not wrapped")
			}
			return bytes.TrimPrefix(b, []byte(wrapPrefix)), nil
		},
	}
}

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

// newRemoteStub builds a stub remote signer over a throwaway CA key.
func newRemoteStub(t *testing.T) *stubSigner {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("CA key: %v", err)
	}
	return &stubSigner{pub: &caKey.PublicKey, caKey: caKey}
}

// TestNewIssuerWithRemoteSigner_FirstBoot_GeneratesWrapsAndStoresDEK pins the
// first-boot path: the DEK is absent, so the issuer generates a fresh DEK,
// KMS-wraps it, and SETNX-persists the wrapped blob. Observable evidence:
// the store now holds the WRAPPED blob (not the raw DEK), Encrypt+SetNX ran
// exactly once, and the 32-byte AES key is derived (caKey nil, signer wired).
func TestNewIssuerWithRemoteSigner_FirstBoot_GeneratesWrapsAndStoresDEK(t *testing.T) {
	dir := t.TempDir()
	certPath, _, err := testutil.WriteTestCA(dir)
	if err != nil {
		t.Fatalf("WriteTestCA: %v", err)
	}
	store := &inMemoryDEKStore{}
	k := identityKMS()

	iss, err := NewIssuerWithRemoteSigner(context.Background(), certPath, newRemoteStub(t), store, k, k)
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
	// The store must hold the WRAPPED DEK (KMS ciphertext), never the raw key.
	if store.blob == nil {
		t.Fatal("first boot must persist the wrapped DEK in the store")
	}
	if !bytes.HasPrefix(store.blob, []byte(wrapPrefix)) {
		t.Error("persisted blob must be the KMS-wrapped DEK, not raw key material")
	}
	if store.setCalls != 1 {
		t.Errorf("SetWrappedDEKIfAbsent calls = %d, want 1", store.setCalls)
	}
}

// TestNewIssuerWithRemoteSigner_SubsequentBoot_ReusesStoredDEK pins the
// restart / second-instance path: a wrapped DEK already exists, so the issuer
// unwraps it (no generate, no SETNX) and derives the SAME AES key as the
// instance that first created it — so cached leaf keys survive restart.
func TestNewIssuerWithRemoteSigner_SubsequentBoot_ReusesStoredDEK(t *testing.T) {
	dir := t.TempDir()
	certPath, _, err := testutil.WriteTestCA(dir)
	if err != nil {
		t.Fatalf("WriteTestCA: %v", err)
	}
	store := &inMemoryDEKStore{}
	k := identityKMS()

	first, err := NewIssuerWithRemoteSigner(context.Background(), certPath, newRemoteStub(t), store, k, k)
	if err != nil {
		t.Fatalf("first boot: %v", err)
	}
	// Second boot over the SAME store must NOT generate/persist again.
	store.setCalls = 0
	second, err := NewIssuerWithRemoteSigner(context.Background(), certPath, newRemoteStub(t), store, k, k)
	if err != nil {
		t.Fatalf("second boot: %v", err)
	}
	if store.setCalls != 0 {
		t.Errorf("subsequent boot must not SETNX again; setCalls = %d", store.setCalls)
	}
	if !bytes.Equal(first.aesKey, second.aesKey) {
		t.Error("subsequent boot must derive the same AES key (cached leaf keys must survive restart)")
	}
}

// TestNewIssuerWithRemoteSigner_KeyIndependentOfCACert is the core security
// assertion: the cache AES key must NOT be a function of the
// (public) CA cert. Two issuers sharing the same DEK store but DIFFERENT CA
// certs MUST derive the SAME key — proving the key comes from the
// KMS-managed DEK, so the published CA cert grants no decryption power.
func TestNewIssuerWithRemoteSigner_KeyIndependentOfCACert(t *testing.T) {
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
	store := &inMemoryDEKStore{}
	k := identityKMS()

	issA, err := NewIssuerWithRemoteSigner(context.Background(), certA, newRemoteStub(t), store, k, k)
	if err != nil {
		t.Fatalf("issuer A: %v", err)
	}
	issB, err := NewIssuerWithRemoteSigner(context.Background(), certB, newRemoteStub(t), store, k, k)
	if err != nil {
		t.Fatalf("issuer B: %v", err)
	}
	if !bytes.Equal(issA.aesKey, issB.aesKey) {
		t.Error("AES key must depend ONLY on the KMS DEK, not on the CA cert (F-0019)")
	}
}

// TestNewIssuerWithRemoteSigner_SETNXRaceLost_ReusesWinnerDEK pins the
// multi-instance race: this instance generated a DEK but lost the SETNX, so
// it must discard its own DEK, re-read the winner's wrapped blob, and derive
// the winner's AES key — so all instances converge.
func TestNewIssuerWithRemoteSigner_SETNXRaceLost_ReusesWinnerDEK(t *testing.T) {
	dir := t.TempDir()
	certPath, _, err := testutil.WriteTestCA(dir)
	if err != nil {
		t.Fatalf("WriteTestCA: %v", err)
	}
	k := identityKMS()

	// The "winner" wrapped a known DEK; derive the AES key it would produce
	// by booting an issuer that simply reads that blob.
	winnerDEK := bytes.Repeat([]byte{0x5a}, dekLen)
	winnerWrapped, _ := k.Encrypt(context.Background(), winnerDEK)
	refStore := &inMemoryDEKStore{blob: winnerWrapped}
	ref, err := NewIssuerWithRemoteSigner(context.Background(), certPath, newRemoteStub(t), refStore, k, k)
	if err != nil {
		t.Fatalf("reference boot: %v", err)
	}

	// This instance starts empty, generates its own DEK, but loses the race —
	// the winner's blob is installed and SetNX reports won=false.
	raceStore := &inMemoryDEKStore{forceLostRace: true, raceWinnerBlob: winnerWrapped}
	loser, err := NewIssuerWithRemoteSigner(context.Background(), certPath, newRemoteStub(t), raceStore, k, k)
	if err != nil {
		t.Fatalf("race-lost boot: %v", err)
	}
	if !bytes.Equal(loser.aesKey, ref.aesKey) {
		t.Error("after losing SETNX, the instance must adopt the winner's DEK (convergence)")
	}
}

// TestNewIssuerWithRemoteSigner_BadCertPath — file doesn't exist, must
// produce a clean error mentioning the path so the operator sees what's
// missing.
func TestNewIssuerWithRemoteSigner_BadCertPath(t *testing.T) {
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	stub := &stubSigner{pub: &caKey.PublicKey, caKey: caKey}
	_, err := NewIssuerWithRemoteSigner(context.Background(), "/nonexistent/ca.pem", stub, &inMemoryDEKStore{}, identityKMS(), identityKMS())
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
	_, err := NewIssuerWithRemoteSigner(context.Background(), certPath, stub, &inMemoryDEKStore{}, identityKMS(), identityKMS())
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
	_, err := NewIssuerWithRemoteSigner(context.Background(), certPath, stub, &inMemoryDEKStore{}, identityKMS(), identityKMS())
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
	_, err := NewIssuerWithRemoteSigner(context.Background(), certPath, stub, &inMemoryDEKStore{}, identityKMS(), identityKMS())
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

	iss, err := NewIssuerWithRemoteSigner(context.Background(), certPath, stub, &inMemoryDEKStore{}, identityKMS(), identityKMS())
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
	iss, err := NewIssuerWithRemoteSigner(context.Background(), certPath, stub, &inMemoryDEKStore{}, identityKMS(), identityKMS())
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

// --- cert-cache DEK bootstrap: fail-closed error paths ---

// TestBootstrap_NilStore_FailClosed: remote mode without a Redis-backed DEK
// store must error and name redis.addrs — NEVER silently derive a key.
func TestNewIssuerWithRemoteSigner_NilStore_FailClosed(t *testing.T) {
	dir := t.TempDir()
	certPath, _, _ := testutil.WriteTestCA(dir)
	k := identityKMS()
	_, err := NewIssuerWithRemoteSigner(context.Background(), certPath, newRemoteStub(t), nil, k, k)
	if err == nil || !strings.Contains(err.Error(), "redis.addrs") {
		t.Errorf("nil store must fail-closed naming redis.addrs; got %v", err)
	}
}

// TestNewIssuerWithRemoteSigner_NilEncryptor_FailClosed: a missing KMS
// encrypt command must error and name ca.kms.encryptCommand.
func TestNewIssuerWithRemoteSigner_NilEncryptor_FailClosed(t *testing.T) {
	dir := t.TempDir()
	certPath, _, _ := testutil.WriteTestCA(dir)
	_, err := NewIssuerWithRemoteSigner(context.Background(), certPath, newRemoteStub(t), &inMemoryDEKStore{}, nil, identityKMS())
	if err == nil || !strings.Contains(err.Error(), "encryptCommand") {
		t.Errorf("nil encryptor must fail-closed naming ca.kms.encryptCommand; got %v", err)
	}
}

// TestNewIssuerWithRemoteSigner_NilDecryptor_FailClosed: a missing KMS
// decrypt command must error and name ca.kms.command.
func TestNewIssuerWithRemoteSigner_NilDecryptor_FailClosed(t *testing.T) {
	dir := t.TempDir()
	certPath, _, _ := testutil.WriteTestCA(dir)
	_, err := NewIssuerWithRemoteSigner(context.Background(), certPath, newRemoteStub(t), &inMemoryDEKStore{}, identityKMS(), nil)
	if err == nil || !strings.Contains(err.Error(), "ca.kms.command") {
		t.Errorf("nil decryptor must fail-closed naming ca.kms.command; got %v", err)
	}
}

// TestNewIssuerWithRemoteSigner_KMSEncryptFails_FailClosed: when wrapping the
// fresh DEK fails (e.g. missing kms:Encrypt grant), startup must abort with an
// error naming encryptCommand and kms:Encrypt — not fall back to a CA key.
func TestNewIssuerWithRemoteSigner_KMSEncryptFails_FailClosed(t *testing.T) {
	dir := t.TempDir()
	certPath, _, _ := testutil.WriteTestCA(dir)
	k := &fakeKMS{
		enc: func([]byte) ([]byte, error) { return nil, errors.New("AccessDenied: kms:Encrypt") },
		dec: func(b []byte) ([]byte, error) { return b, nil },
	}
	iss, err := NewIssuerWithRemoteSigner(context.Background(), certPath, newRemoteStub(t), &inMemoryDEKStore{}, k, k)
	if err == nil {
		t.Fatal("KMS encrypt failure must abort startup")
	}
	if iss != nil {
		t.Error("must return nil Issuer (no CA-derived fallback)")
	}
	if !strings.Contains(err.Error(), "encryptCommand") || !strings.Contains(err.Error(), "kms:Encrypt") {
		t.Errorf("err must name encryptCommand + kms:Encrypt; got %q", err)
	}
}

// TestNewIssuerWithRemoteSigner_KMSDecryptFails_FailClosed: on a subsequent
// boot, if unwrapping the stored DEK fails (e.g. missing kms:Decrypt grant),
// startup must abort naming ca.kms.command + kms:Decrypt.
func TestNewIssuerWithRemoteSigner_KMSDecryptFails_FailClosed(t *testing.T) {
	dir := t.TempDir()
	certPath, _, _ := testutil.WriteTestCA(dir)
	k := &fakeKMS{
		enc: func(b []byte) ([]byte, error) { return b, nil },
		dec: func([]byte) ([]byte, error) { return nil, errors.New("AccessDenied: kms:Decrypt") },
	}
	// Pre-seed a stored blob so the GET-found → Decrypt path runs.
	store := &inMemoryDEKStore{blob: bytes.Repeat([]byte{1}, dekLen)}
	iss, err := NewIssuerWithRemoteSigner(context.Background(), certPath, newRemoteStub(t), store, k, k)
	if err == nil {
		t.Fatal("KMS decrypt failure must abort startup")
	}
	if iss != nil {
		t.Error("must return nil Issuer (no CA-derived fallback)")
	}
	if !strings.Contains(err.Error(), "ca.kms.command") || !strings.Contains(err.Error(), "kms:Decrypt") {
		t.Errorf("err must name ca.kms.command + kms:Decrypt; got %q", err)
	}
}

// TestNewIssuerWithRemoteSigner_DEKWrongLength_FailClosed: a KMS decrypt that
// returns a malformed (non-32-byte) DEK must be rejected, not silently used.
func TestNewIssuerWithRemoteSigner_DEKWrongLength_FailClosed(t *testing.T) {
	dir := t.TempDir()
	certPath, _, _ := testutil.WriteTestCA(dir)
	k := &fakeKMS{
		enc: func(b []byte) ([]byte, error) { return b, nil },
		dec: func([]byte) ([]byte, error) { return []byte("too-short"), nil },
	}
	store := &inMemoryDEKStore{blob: bytes.Repeat([]byte{1}, dekLen)}
	_, err := NewIssuerWithRemoteSigner(context.Background(), certPath, newRemoteStub(t), store, k, k)
	if err == nil || !strings.Contains(err.Error(), "malformed key") {
		t.Errorf("malformed DEK length must fail-closed; got %v", err)
	}
}

// TestNewIssuerWithRemoteSigner_RedisGetError_FailClosed: a Redis transport
// failure on the initial GET must NOT be treated as "absent" — it aborts
// startup so we never generate a divergent DEK on a transient blip.
func TestNewIssuerWithRemoteSigner_RedisGetError_FailClosed(t *testing.T) {
	dir := t.TempDir()
	certPath, _, _ := testutil.WriteTestCA(dir)
	k := identityKMS()
	store := &inMemoryDEKStore{getErr: errors.New("connection refused")}
	_, err := NewIssuerWithRemoteSigner(context.Background(), certPath, newRemoteStub(t), store, k, k)
	if err == nil || !strings.Contains(err.Error(), "read wrapped cert-cache DEK") {
		t.Errorf("redis GET error must fail-closed; got %v", err)
	}
}

// TestNewIssuerWithRemoteSigner_SETNXError_FailClosed: a Redis failure on the
// create-once write aborts startup with a SETNX-naming error.
func TestNewIssuerWithRemoteSigner_SETNXError_FailClosed(t *testing.T) {
	dir := t.TempDir()
	certPath, _, _ := testutil.WriteTestCA(dir)
	k := identityKMS()
	store := &inMemoryDEKStore{setErr: errors.New("connection reset")}
	_, err := NewIssuerWithRemoteSigner(context.Background(), certPath, newRemoteStub(t), store, k, k)
	if err == nil || !strings.Contains(err.Error(), "SETNX") {
		t.Errorf("redis SETNX error must fail-closed; got %v", err)
	}
}

// TestNewIssuerWithRemoteSigner_ReGetAfterRaceError_FailClosed: losing the
// SETNX race and then failing the re-GET must abort, not proceed with a DEK
// nobody else can read.
func TestNewIssuerWithRemoteSigner_ReGetAfterRaceError_FailClosed(t *testing.T) {
	dir := t.TempDir()
	certPath, _, _ := testutil.WriteTestCA(dir)
	k := identityKMS()
	store := &inMemoryDEKStore{forceLostRace: true, raceWinnerBlob: nil, reGetErr: errors.New("blip")}
	_, err := NewIssuerWithRemoteSigner(context.Background(), certPath, newRemoteStub(t), store, k, k)
	if err == nil || !strings.Contains(err.Error(), "re-read winner") {
		t.Errorf("re-GET error after race must fail-closed; got %v", err)
	}
}

// TestNewIssuerWithRemoteSigner_DEKVanishedAfterRace_FailClosed: we lose the
// SETNX race but the winner's blob is then absent on re-GET (e.g. it expired
// or was deleted in the gap). Rather than proceed with our own discarded DEK,
// startup must abort and ask for a retry.
func TestNewIssuerWithRemoteSigner_DEKVanishedAfterRace_FailClosed(t *testing.T) {
	dir := t.TempDir()
	certPath, _, _ := testutil.WriteTestCA(dir)
	k := identityKMS()
	// forceLostRace with a nil winner blob → re-GET reports absent.
	store := &inMemoryDEKStore{forceLostRace: true, raceWinnerBlob: nil}
	_, err := NewIssuerWithRemoteSigner(context.Background(), certPath, newRemoteStub(t), store, k, k)
	if err == nil || !strings.Contains(err.Error(), "vanished") {
		t.Errorf("absent winner blob after race must fail-closed; got %v", err)
	}
}

// TestNewIssuerWithRemoteSigner_DEKEntropyFails_FailClosed drives the
// io.ReadFull error arm for fresh-DEK generation via the dekRandReader seam.
func TestNewIssuerWithRemoteSigner_DEKEntropyFails_FailClosed(t *testing.T) {
	dir := t.TempDir()
	certPath, _, _ := testutil.WriteTestCA(dir)
	orig := dekRandReader
	dekRandReader = failingReader{err: errors.New("entropy starved")}
	defer func() { dekRandReader = orig }()

	k := identityKMS()
	_, err := NewIssuerWithRemoteSigner(context.Background(), certPath, newRemoteStub(t), &inMemoryDEKStore{}, k, k)
	if err == nil || !strings.Contains(err.Error(), "generate cert-cache DEK") {
		t.Errorf("DEK entropy failure must fail-closed; got %v", err)
	}
}

// TestRemoteIssuer_CacheRoundTripUnderDEKKey proves the bootstrapped DEK
// actually drives a working AES-GCM round-trip: a leaf key encrypted by one
// remote issuer decrypts under a second remote issuer that re-derives the
// SAME DEK from the shared store (the cross-restart cache-hit invariant).
func TestRemoteIssuer_CacheRoundTripUnderDEKKey(t *testing.T) {
	dir := t.TempDir()
	certPath, _, _ := testutil.WriteTestCA(dir)
	store := &inMemoryDEKStore{}
	k := identityKMS()

	a, err := NewIssuerWithRemoteSigner(context.Background(), certPath, newRemoteStub(t), store, k, k)
	if err != nil {
		t.Fatalf("issuer A: %v", err)
	}
	leaf, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ct, nonce, err := a.EncryptPrivateKey(leaf)
	if err != nil {
		t.Fatalf("EncryptPrivateKey: %v", err)
	}

	b, err := NewIssuerWithRemoteSigner(context.Background(), certPath, newRemoteStub(t), store, k, k)
	if err != nil {
		t.Fatalf("issuer B: %v", err)
	}
	got, err := b.DecryptPrivateKey(ct, nonce)
	if err != nil {
		t.Fatalf("DecryptPrivateKey under re-derived DEK: %v", err)
	}
	if !got.Equal(leaf) {
		t.Error("round-tripped leaf key must match under the shared DEK-derived AES key")
	}
}

// errSimulatedKMSDown is a sentinel for the SignCert_RemoteSignerError test
// — extracted so the import block doesn't need 'errors' twice.
var errSimulatedKMSDown = stubError("simulated KMS down")

type stubError string

func (e stubError) Error() string { return string(e) }
