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
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// failReader is an io.Reader that always returns a sentinel error; it
// drives the entropy-error branches in token.go and ca.go without
// touching the production crypto/rand source. Matches the same fixture
// pattern used in packages/shared/identity/pkce.
type failReader struct{ err error }

func (f failReader) Read(_ []byte) (int, error) { return 0, f.err }

func TestNew_GeneratesCA(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	ca, err := New(dir, logger)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	if ca.CACertPEM() == "" {
		t.Fatal("CA cert PEM is empty")
	}

	if !fileExists(filepath.Join(dir, "ca.pem")) {
		t.Fatal("ca.pem not written")
	}
	if !fileExists(filepath.Join(dir, "ca-key.pem")) {
		t.Fatal("ca-key.pem not written")
	}
}

func TestNew_LoadsExistingCA(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	ca1, err := New(dir, logger)
	if err != nil {
		t.Fatalf("first New() error: %v", err)
	}
	serial1 := ca1.cert.SerialNumber

	ca2, err := New(dir, logger)
	if err != nil {
		t.Fatalf("second New() error: %v", err)
	}

	if ca2.cert.SerialNumber.Cmp(serial1) != 0 {
		t.Fatal("second New() did not load same CA")
	}
}

// makeEd25519CSR produces a self-signed CSR with an Ed25519 keypair and
// the given CN. Returns PEM-encoded CSR and the matching public key so
// downstream tests can verify the cert binds to the expected key.
func makeEd25519CSR(t *testing.T, cn string) (string, ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519 keygen: %v", err)
	}
	tmpl := &x509.CertificateRequest{Subject: pkix.Name{CommonName: cn}}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, priv)
	if err != nil {
		t.Fatalf("CreateCSR: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})), pub
}

func TestSignAttestationCSR_HappyPath(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	ca, err := New(dir, logger)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	csrPEM, pub := makeEd25519CSR(t, "550e8400-e29b-41d4-a716-446655440000")
	result, err := ca.SignAttestationCSR(csrPEM, "550e8400-e29b-41d4-a716-446655440000")
	if err != nil {
		t.Fatalf("SignAttestationCSR: %v", err)
	}
	if result.KeyPEM != "" {
		t.Errorf("KeyPEM must be empty (CSR-mode); got %q", result.KeyPEM)
	}

	block, _ := pem.Decode([]byte(result.CertPEM))
	if block == nil {
		t.Fatal("cert PEM decode failed")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}

	// 1. Cert public key matches the CSR public key.
	certPub, ok := cert.PublicKey.(ed25519.PublicKey)
	if !ok {
		t.Fatalf("cert public key type = %T; want ed25519.PublicKey", cert.PublicKey)
	}
	if string(certPub) != string(pub) {
		t.Error("cert public key bytes differ from CSR public key")
	}

	// 2. KeyUsage is DigitalSignature ONLY.
	if cert.KeyUsage != x509.KeyUsageDigitalSignature {
		t.Errorf("KeyUsage = %v; want DigitalSignature only", cert.KeyUsage)
	}

	// 3. ExtKeyUsage MUST NOT include ClientAuth — that is the
	//    key-separation invariant enforced for attestation certs.
	for _, eku := range cert.ExtKeyUsage {
		if eku == x509.ExtKeyUsageClientAuth {
			t.Error("attestation cert MUST NOT carry ClientAuth EKU (key-separation invariant)")
		}
	}
	if len(cert.ExtKeyUsage) != 0 {
		t.Errorf("ExtKeyUsage = %v; want empty", cert.ExtKeyUsage)
	}

	// 4. CN preserved.
	if cert.Subject.CommonName != "550e8400-e29b-41d4-a716-446655440000" {
		t.Errorf("CN = %q", cert.Subject.CommonName)
	}
}

func TestSignAttestationCSR_RejectsECDSAKey(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	ca, _ := New(dir, logger)

	clientKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.CertificateRequest{Subject: pkix.Name{CommonName: "x"}}
	der, _ := x509.CreateCertificateRequest(rand.Reader, tmpl, clientKey)
	csrPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))

	_, err := ca.SignAttestationCSR(csrPEM, "x")
	if err == nil {
		t.Fatal("expected rejection of ECDSA CSR; got nil error")
	}
	if !contains(err.Error(), "Ed25519") {
		t.Errorf("error should mention Ed25519; got %q", err.Error())
	}
}

func TestSignAttestationCSR_InvalidPEM(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	ca, _ := New(dir, logger)

	if _, err := ca.SignAttestationCSR("not-pem", "x"); err == nil {
		t.Fatal("expected error for invalid PEM")
	}
}

func TestSignAttestationCSR_TamperedSignature(t *testing.T) {
	// Mutate the CSR after creation so CheckSignature fails. The handler
	// must reject — otherwise an attacker could substitute a different
	// agent's public key into a captured CSR envelope.
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	ca, _ := New(dir, logger)

	csrPEM, _ := makeEd25519CSR(t, "x")
	// Flip one byte inside the base64 body to break the signature.
	block, _ := pem.Decode([]byte(csrPEM))
	if block == nil {
		t.Fatal("decode CSR PEM")
	}
	block.Bytes[len(block.Bytes)-3] ^= 0x01
	tampered := string(pem.EncodeToMemory(block))

	if _, err := ca.SignAttestationCSR(tampered, "x"); err == nil {
		t.Fatal("expected signature-check failure on tampered CSR")
	}
}

func TestSignAttestationCSR_ParseFailure(t *testing.T) {
	// Valid PEM envelope, garbage body — exercises the
	// x509.ParseCertificateRequest error branch.
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	ca, _ := New(dir, logger)

	garbage := string(pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE REQUEST",
		Bytes: []byte("not-a-csr"),
	}))
	_, err := ca.SignAttestationCSR(garbage, "x")
	if err == nil {
		t.Fatal("expected parse-CSR error")
	}
	if !contains(err.Error(), "parse CSR") {
		t.Errorf("error should wrap 'parse CSR'; got %q", err.Error())
	}
}

func TestSignAttestationCSR_EntropyFailure(t *testing.T) {
	// Force x509.CreateCertificate to fail by starving its entropy
	// source — same caRandReader seam the package uses elsewhere.
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	ca, _ := New(dir, logger)

	csrPEM, _ := makeEd25519CSR(t, "x")
	original := caRandReader
	caRandReader = failReader{err: errors.New("entropy starved")}
	t.Cleanup(func() { caRandReader = original })

	_, err := ca.SignAttestationCSR(csrPEM, "x")
	if err == nil {
		t.Fatal("expected entropy-error wrap")
	}
	if !contains(err.Error(), "sign attestation CSR") {
		t.Errorf("error should wrap 'sign attestation CSR'; got %q", err.Error())
	}
}

// contains is a no-import helper for substring assertions in this test
// file (keeps the strings import scoped to ca.go).
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestGenerateDeviceToken(t *testing.T) {
	plain1, hash1, err := GenerateDeviceToken()
	if err != nil {
		t.Fatalf("GenerateDeviceToken error: %v", err)
	}

	if len(plain1) != 64 {
		t.Fatalf("plaintext length = %d, want 64", len(plain1))
	}
	if len(hash1) != 64 {
		t.Fatalf("hash length = %d, want 64", len(hash1))
	}

	plain2, hash2, _ := GenerateDeviceToken()
	if plain1 == plain2 {
		t.Fatal("two calls produced identical tokens")
	}
	if hash1 == hash2 {
		t.Fatal("two calls produced identical hashes")
	}
}

func TestDeviceTokenExpiry(t *testing.T) {
	// The expiry must be exactly now + DeviceTokenTTL — the bounded lifetime
	// that closes F-0202's "device token never expires" gap.
	now := time.Date(2026, 6, 7, 0, 0, 0, 0, time.UTC)
	got := DeviceTokenExpiry(now)
	if want := now.Add(DeviceTokenTTL); !got.Equal(want) {
		t.Fatalf("DeviceTokenExpiry = %v, want %v", got, want)
	}
	// Guard the chosen TTL stays in the sane 1-day..60-day band so an accidental
	// edit to a zero / forever value is caught.
	if DeviceTokenTTL < 24*time.Hour || DeviceTokenTTL > 60*24*time.Hour {
		t.Fatalf("DeviceTokenTTL %v outside the sane [1d,60d] band", DeviceTokenTTL)
	}
}

func TestHashDeviceToken_RoundTrip(t *testing.T) {
	plain, expectedHash, err := GenerateDeviceToken()
	if err != nil {
		t.Fatalf("GenerateDeviceToken error: %v", err)
	}

	got, err := HashDeviceToken(plain)
	if err != nil {
		t.Fatalf("HashDeviceToken error: %v", err)
	}
	if got != expectedHash {
		t.Fatalf("hash mismatch: got %q, want %q", got, expectedHash)
	}
}

func TestHashDeviceToken_InvalidHex(t *testing.T) {
	_, err := HashDeviceToken("not-hex-data!")
	if err == nil {
		t.Fatal("expected error for invalid hex")
	}
}

func TestNewFromFiles(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	ca1, _ := New(dir, logger)

	ca2, err := NewFromFiles(filepath.Join(dir, "ca.pem"), filepath.Join(dir, "ca-key.pem"), logger)
	if err != nil {
		t.Fatalf("NewFromFiles error: %v", err)
	}
	if ca2.cert.SerialNumber.Cmp(ca1.cert.SerialNumber) != 0 {
		t.Fatal("NewFromFiles loaded different CA")
	}
}

func TestNewFromFiles_Missing(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	_, err := NewFromFiles("/nonexistent/ca.pem", "/nonexistent/key.pem", logger)
	if err == nil {
		t.Fatal("expected error for missing files")
	}
}

// TestNewFromFiles_MalformedCertPEM covers the `pem.Decode` returns
// nil branch in load() for the cert. Without this, a corrupted CA
// cert file would surface as a confusing "x509: malformed" downstream
// instead of "invalid CA cert PEM" at the load boundary.
func TestNewFromFiles_MalformedCertPEM(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.pem")
	keyPath := filepath.Join(dir, "ca-key.pem")
	if err := os.WriteFile(certPath, []byte("not pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("not pem either"), 0o600); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	if _, err := NewFromFiles(certPath, keyPath, logger); err == nil {
		t.Fatal("expected error on malformed cert PEM")
	}
}

// TestNewFromFiles_ValidCertMalformedKeyPEM covers the
// `pem.Decode(keyData)` nil branch in load() — a valid cert paired
// with a junk key must reject explicitly.
func TestNewFromFiles_ValidCertMalformedKeyPEM(t *testing.T) {
	// Generate a real CA first, then overwrite the key file with junk.
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	if _, err := New(dir, logger); err != nil {
		t.Fatalf("seed CA: %v", err)
	}
	keyPath := filepath.Join(dir, "ca-key.pem")
	if err := os.WriteFile(keyPath, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFromFiles(filepath.Join(dir, "ca.pem"), keyPath, logger); err == nil {
		t.Fatal("expected error on valid cert + malformed key")
	}
}

// TestNewFromFiles_ValidCertParseECPrivateKeyError covers the
// `x509.ParseECPrivateKey(keyBlock.Bytes)` error branch — a PEM with
// the EC PRIVATE KEY type marker but non-EC bytes inside.
func TestNewFromFiles_ValidCertParseECPrivateKeyError(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	if _, err := New(dir, logger); err != nil {
		t.Fatalf("seed CA: %v", err)
	}
	keyPath := filepath.Join(dir, "ca-key.pem")
	garbage := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: []byte("not-an-ec-key")})
	if err := os.WriteFile(keyPath, garbage, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFromFiles(filepath.Join(dir, "ca.pem"), keyPath, logger); err == nil {
		t.Fatal("expected ParseECPrivateKey error")
	}
}

// TestNewFromFiles_ParseCertificateError covers the
// `x509.ParseCertificate(block.Bytes)` error in load() — PEM is
// well-formed but cert bytes are garbage.
func TestNewFromFiles_ParseCertificateError(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.pem")
	keyPath := filepath.Join(dir, "ca-key.pem")
	bad := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("not-a-cert")})
	if err := os.WriteFile(certPath, bad, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("doesn't matter"), 0o600); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	if _, err := NewFromFiles(certPath, keyPath, logger); err == nil {
		t.Fatal("expected ParseCertificate error")
	}
}

// TestNew_LoadsExistingErrorWraps covers the `New()` branch that finds
// existing ca.pem + ca-key.pem on disk but fails to load them — the
// wrap message must surface "load existing CA: …" so an operator
// debugging a corrupted state directory sees the problem at the New()
// boundary, not a downstream cert-issuance failure.
func TestNew_LoadsExistingErrorWraps(t *testing.T) {
	dir := t.TempDir()
	// Plant malformed cert + key so fileExists checks both pass but
	// load() returns an error.
	if err := os.WriteFile(filepath.Join(dir, "ca.pem"), []byte("not pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ca-key.pem"), []byte("not pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	_, err := New(dir, logger)
	if err == nil {
		t.Fatal("expected load error wrap")
	}
}

// TestNew_MkdirAllFails covers `os.MkdirAll` error. Plant a regular
// file at the target path so MkdirAll fails (cannot make a directory
// where a file already exists).
func TestNew_MkdirAllFails(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "blocker")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	_, err := New(target, logger)
	if err == nil {
		t.Fatal("expected MkdirAll error when target is a regular file")
	}
}

// TestGenerateDeviceToken_HashFormat covers entropy distinctness for
// the random device token. Each generation should yield a different
// plaintext; identical hashes would indicate a broken rand.Read.
func TestGenerateDeviceToken_HashFormat(t *testing.T) {
	tok, _, err := GenerateDeviceToken()
	if err != nil {
		t.Fatalf("gen: %v", err)
	}
	tok2, _, _ := GenerateDeviceToken()
	if tok == tok2 {
		t.Error("GenerateDeviceToken returned identical tokens — entropy broken")
	}
}

// TestNew_DefaultDirWhenEmpty covers the `if dir == ""` branch in
// New() which substitutes "./.agent-ca". Use a chdir to a TempDir so
// we don't pollute the working tree.
func TestNew_DefaultDirWhenEmpty(t *testing.T) {
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	ca, err := New("", logger)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if ca.dir != filepath.Join(".", ".agent-ca") {
		t.Errorf("dir: got %q, want %q", ca.dir, filepath.Join(".", ".agent-ca"))
	}
}

// TestGenerateDeviceToken_EntropyError covers the `io.ReadFull` error
// branch in GenerateDeviceToken by substituting tokenRandReader with a
// failReader. Without this, a CSPRNG failure in production would surface
// as a confusing zero-byte token rather than the wrapped
// "generate device token" error.
func TestGenerateDeviceToken_EntropyError(t *testing.T) {
	sentinel := errors.New("entropy boom")
	orig := tokenRandReader
	tokenRandReader = failReader{err: sentinel}
	t.Cleanup(func() { tokenRandReader = orig })

	plain, hashed, err := GenerateDeviceToken()
	if err == nil {
		t.Fatal("expected entropy error, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("error chain missing sentinel: %v", err)
	}
	if plain != "" || hashed != "" {
		t.Fatalf("expected empty strings on error, got plain=%q hash=%q", plain, hashed)
	}
}

// TestLoad_KeyReadError covers the `os.ReadFile(keyPath)` error branch
// in load() (ca.go:248). NewFromFiles wraps load(), so we pass a valid
// cert path and a non-existent key path. Without this test the cert
// read succeeds, the cert parses, then the key read errors — the
// branch our other load-error tests skip past.
func TestLoad_KeyReadError(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	if _, err := New(dir, logger); err != nil {
		t.Fatalf("seed CA: %v", err)
	}
	certPath := filepath.Join(dir, "ca.pem")
	missingKey := filepath.Join(dir, "nonexistent-key.pem")
	if _, err := NewFromFiles(certPath, missingKey, logger); err == nil {
		t.Fatal("expected ReadFile error on missing key path")
	}
}

// TestGenerate_WriteFileError covers the `os.WriteFile(certPath)` error
// branch in generate() (ca.go:209). Calling generate() directly with a
// path inside a read-only directory makes WriteFile fail without
// MkdirAll having a chance to create anything first. Skipped on
// Windows / when running as root since 0500 permission semantics differ.
func TestGenerate_WriteFileError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission-bit semantics differ on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permission checks")
	}

	roDir := t.TempDir()
	if err := os.Chmod(roDir, 0o500); err != nil {
		t.Fatalf("chmod ro: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(roDir, 0o700) })

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	ca := &CA{logger: logger}
	certPath := filepath.Join(roDir, "ca.pem")
	keyPath := filepath.Join(roDir, "ca-key.pem")
	if err := ca.generate(certPath, keyPath); err == nil {
		t.Fatal("expected WriteFile error in read-only dir")
	}
}

// TestGenerate_EntropyError covers the `ecdsa.GenerateKey` error
// branch in generate() (ca.go:193). With caRandReader failing, the CA
// key generation must error out before any disk I/O happens.
func TestGenerate_EntropyError(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	ca := &CA{logger: logger, dir: dir}

	orig := caRandReader
	caRandReader = failReader{err: errors.New("ca entropy fail")}
	t.Cleanup(func() { caRandReader = orig })

	certPath := filepath.Join(dir, "ca.pem")
	keyPath := filepath.Join(dir, "ca-key.pem")
	if err := ca.generate(certPath, keyPath); err == nil {
		t.Fatal("expected ecdsa.GenerateKey entropy error")
	}
	// Neither file should be written when key gen fails up-front.
	if fileExists(certPath) {
		t.Error("ca.pem written despite key-gen failure")
	}
	if fileExists(keyPath) {
		t.Error("ca-key.pem written despite key-gen failure")
	}
}

// TestNew_GenerateErrorWraps covers the `generate()` error wrap branch
// in New() (ca.go:72). We arrange for MkdirAll to succeed (dir already
// exists with rwx) but then strip write permission so the subsequent
// WriteFile inside generate() fails. The error must surface as
// "generate CA: …" at the New() boundary.
func TestNew_GenerateErrorWraps(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission-bit semantics differ on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permission checks")
	}

	dir := t.TempDir()
	// Strip write permission AFTER TempDir creation; MkdirAll on an
	// already-existing dir is a no-op and won't fail, but the WriteFile
	// calls inside generate() will.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod ro: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	if _, err := New(dir, logger); err == nil {
		t.Fatal("expected wrapped generate error from New")
	}
}
