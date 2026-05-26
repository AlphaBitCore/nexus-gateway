package token_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/token"
)

// TestOpenKeystore_BadPEMRejected covers the "pem.Decode returns nil"
// branch. A PEM-looking file that does not actually contain a block must
// surface an error rather than silently skip — a partial-corruption seed
// would otherwise boot the auth server with zero keys and silently fail
// every signing attempt.
func TestOpenKeystore_BadPEMRejected(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "broken.pem"), []byte("not a pem block"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := token.OpenKeystore(dir); err == nil {
		t.Fatal("expected error for non-PEM .pem file")
	}
}

// TestOpenKeystore_BadX509Rejected covers the x509.ParsePKCS1PrivateKey
// failure branch. A PEM block with the right header but garbage payload
// must surface an error — silently dropping the key would leave the
// keystore short a kid the cluster still believes is active.
func TestOpenKeystore_BadX509Rejected(t *testing.T) {
	dir := t.TempDir()
	// Construct a syntactically valid PEM with PKCS1 RSA header but bogus
	// bytes inside; the x509 parser must reject it.
	data := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: []byte("\x00\x01\x02bogus")})
	if err := os.WriteFile(filepath.Join(dir, "garbage.pem"), data, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := token.OpenKeystore(dir)
	if err == nil {
		t.Fatal("expected error for valid PEM with bogus PKCS1 bytes")
	}
	if !strings.Contains(err.Error(), "parse") {
		t.Errorf("err = %v, want one containing 'parse'", err)
	}
}

// TestOpenKeystore_NonPEMFileIgnored documents the suffix-filter branch:
// a file whose name does not end in .pem must be skipped, not parsed.
// This lets ops drop README / backup files alongside the key set.
func TestOpenKeystore_NonPEMFileIgnored(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README.txt"), []byte("ignored"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	ks, err := token.OpenKeystore(dir)
	if err != nil {
		t.Fatalf("OpenKeystore: %v", err)
	}
	if len(ks.All()) != 0 {
		t.Errorf("non-pem file must not produce a key; got %d keys", len(ks.All()))
	}
}

// TestOpenKeystore_SubdirIgnored documents that a directory entry whose
// name ends in .pem is NOT parsed (e.g. a backup folder mistakenly named
// `archive.pem/`).
func TestOpenKeystore_SubdirIgnored(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "trap.pem"), 0o700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	ks, err := token.OpenKeystore(dir)
	if err != nil {
		t.Fatalf("OpenKeystore: %v", err)
	}
	if len(ks.All()) != 0 {
		t.Errorf(".pem-suffixed subdir must be ignored; got %d keys", len(ks.All()))
	}
}

// TestOpenKeystore_SortsByCreatedAt documents the sort step at the end of
// OpenKeystore: after multiple Generate() calls the most-recent key must
// be the one ActiveKID() returns.
func TestOpenKeystore_SortsByCreatedAt(t *testing.T) {
	dir := t.TempDir()
	ks, err := token.OpenKeystore(dir)
	if err != nil {
		t.Fatalf("OpenKeystore: %v", err)
	}
	if _, err := ks.Generate(); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	kid2, err := ks.Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	// Re-open from disk: the sort branch fires here.
	reloaded, err := token.OpenKeystore(dir)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	keys := reloaded.All()
	if len(keys) != 2 {
		t.Fatalf("keys = %d, want 2", len(keys))
	}
	// Most-recent (kid2) must be last after the oldest-first sort.
	if keys[len(keys)-1].KID != kid2 {
		t.Errorf("active after reload = %q, want %q", keys[len(keys)-1].KID, kid2)
	}
	if reloaded.ActiveKID() != kid2 {
		t.Errorf("ActiveKID after reload = %q, want %q", reloaded.ActiveKID(), kid2)
	}
}

// TestOpenKeystore_MkdirAllFailure exercises the MkdirAll error branch by
// passing a path that is a regular file. MkdirAll surfaces "not a directory"
// before ReadDir is reached.
func TestOpenKeystore_MkdirAllFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission semantics only")
	}
	parent := t.TempDir()
	target := filepath.Join(parent, "blocker")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	if _, err := token.OpenKeystore(target); err == nil {
		t.Fatal("expected error when path is a regular file")
	}
}

// TestOpenKeystore_ReadDirFailure exercises the os.ReadDir error branch:
// MkdirAll on an existing dir is a no-op (succeeds), then ReadDir on a
// mode-0 directory surfaces EACCES. Skipped when running as root.
func TestOpenKeystore_ReadDirFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission semantics only")
	}
	if os.Getuid() == 0 {
		t.Skip("root bypasses chmod 0 — branch unreachable here")
	}
	dir := t.TempDir()
	// Make the directory unreadable AFTER it exists so MkdirAll's idempotent
	// check succeeds but ReadDir gets EACCES.
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if _, err := token.OpenKeystore(dir); err == nil {
		t.Fatal("expected error when dir is unreadable")
	}
}

// TestOpenKeystore_PermissionDeniedReadFails attempts to surface the
// os.ReadFile error branch by making a .pem file unreadable. Skipped on
// platforms where chmod 0 doesn't actually deny the current process (e.g.
// when running as root).
func TestOpenKeystore_PermissionDeniedReadFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission semantics only")
	}
	if os.Getuid() == 0 {
		t.Skip("root bypasses chmod 0 — branch unreachable here")
	}
	dir := t.TempDir()
	bad := filepath.Join(dir, "locked.pem")
	if err := os.WriteFile(bad, []byte("dummy"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Chmod(bad, 0); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(bad, 0o600) })

	if _, err := token.OpenKeystore(dir); err == nil {
		t.Fatal("expected error reading mode-0 file")
	}
}

// TestKeystore_ByKID_MissReturnsFalse covers the negative branch of ByKID
// without going through OpenKeystore — straightforward but locks the
// "ok=false on miss" contract that VerifyLocal relies on.
func TestKeystore_ByKID_MissReturnsFalse(t *testing.T) {
	ks, _ := token.OpenKeystore(t.TempDir())
	if _, ok := ks.ByKID("nonexistent"); ok {
		t.Error("ByKID('nonexistent') = ok=true, want false")
	}
}

// TestKeystore_ActiveKID_EmptyStoreReturnsEmpty locks the "no keys yet"
// contract — Signer.Sign relies on this to surface a clear error rather
// than panic on an out-of-bounds slice index.
func TestKeystore_ActiveKID_EmptyStoreReturnsEmpty(t *testing.T) {
	ks, _ := token.OpenKeystore(t.TempDir())
	if got := ks.ActiveKID(); got != "" {
		t.Errorf("ActiveKID on empty store = %q, want ''", got)
	}
}

// TestKeystore_All_ReturnsCopy guards against aliasing — the caller must
// not be able to mutate the keystore's internal slice via the returned
// view. A leaked alias would let a request handler swap keys at runtime
// without going through Generate().
func TestKeystore_All_ReturnsCopy(t *testing.T) {
	ks, _ := token.OpenKeystore(t.TempDir())
	_, _ = ks.Generate()
	a := ks.All()
	if len(a) != 1 {
		t.Fatalf("All() = %d keys, want 1", len(a))
	}
	a[0].KID = "tampered"
	if ks.ActiveKID() == "tampered" {
		t.Error("All() must return a copy, not an alias to internal state")
	}
}

// makeECDSAPemPlausible writes a PEM file with the RSA-PRIVATE-KEY header
// but ECDSA payload bytes. The x509 PKCS1 parser must reject it. Used to
// exercise the parse branch via a more realistic corruption mode (rather
// than the synthetic garbage variant above).
func makeECDSAPemPlausible(t *testing.T, dir string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ec: %v", err)
	}
	ecDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal ec: %v", err)
	}
	// PEM type intentionally wrong — header says RSA, bytes say EC.
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: ecDER})
	if err := os.WriteFile(filepath.Join(dir, "wrongtype.pem"), pemBytes, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// TestOpenKeystore_RejectsECDSAUnderRSAHeader is the strongest x509 parse
// failure test — a real EC key under the RSA PRIVATE KEY label. Defends
// against ops accidentally restoring the wrong key type from a backup.
func TestOpenKeystore_RejectsECDSAUnderRSAHeader(t *testing.T) {
	dir := t.TempDir()
	makeECDSAPemPlausible(t, dir)
	_, err := token.OpenKeystore(dir)
	if err == nil {
		t.Fatal("expected error parsing EC bytes as RSA PKCS1")
	}
}

// TestKeystore_Generate_WriteFailureSurfaces covers the os.WriteFile
// error branch inside Generate. Strategy: open the keystore on a dir
// that exists, then strip write permission on the dir so the subsequent
// WriteFile gets EACCES. Skipped when running as root (root bypasses
// directory write permissions on most filesystems).
func TestKeystore_Generate_WriteFailureSurfaces(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission semantics only")
	}
	if os.Getuid() == 0 {
		t.Skip("root bypasses chmod — branch unreachable here")
	}
	dir := t.TempDir()
	ks, err := token.OpenKeystore(dir)
	if err != nil {
		t.Fatalf("OpenKeystore: %v", err)
	}
	// Read-only directory: stat OK, but write fails.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	if _, err := ks.Generate(); err == nil {
		t.Fatal("expected error from Generate on read-only dir")
	}
}
