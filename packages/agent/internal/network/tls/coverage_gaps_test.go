package tls

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
	"sync"
	"testing"
	"time"
)

// TestNewEngine_DefaultsApplied pins that zero maxCache and zero cacheTTL
// trigger the documented defaults (1000 and 1h) rather than disabling the
// cache or causing immediate expiry.
func TestNewEngine_DefaultsApplied(t *testing.T) {
	e, err := NewEngine(nil, nil, 0, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if e.maxCache != 1000 {
		t.Errorf("maxCache default: want 1000, got %d", e.maxCache)
	}
	if e.cacheTTL != time.Hour {
		t.Errorf("cacheTTL default: want 1h, got %v", e.cacheTTL)
	}
}

// TestNewEngine_AcceptsProvidedCA pins that passing a pre-built CA cert+key
// skips the generateCA branch and reuses what the caller supplied — the
// runtime-daemon path documented in the LoadOrGenerateCA doc comment.
func TestNewEngine_AcceptsProvidedCA(t *testing.T) {
	caCert, caKey, err := generateCA()
	if err != nil {
		t.Fatalf("generateCA: %v", err)
	}
	e, err := NewEngine(caCert, caKey, 5, 30*time.Minute)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	if e.CACert() != caCert {
		t.Error("caller-supplied CA cert must be reused, not regenerated")
	}
	if e.caKey != caKey {
		t.Error("caller-supplied CA key must be reused, not regenerated")
	}
}

// TestLoadOrGenerateCA_EmptyPathRejected pins the input-validation arm:
// callers that pass empty paths get an explicit error rather than silently
// writing to "" (which would also be a useful test of os.WriteFile but is
// not the intent).
func TestLoadOrGenerateCA_EmptyPathRejected(t *testing.T) {
	cases := []struct {
		name     string
		certPath string
		keyPath  string
	}{
		{"both empty", "", ""},
		{"empty cert", "", "/tmp/key.pem"},
		{"empty key", "/tmp/cert.pem", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cert, key, fresh, err := LoadOrGenerateCA(tc.certPath, tc.keyPath)
			if err == nil {
				t.Fatal("expected error for empty path")
			}
			if cert != nil || key != nil {
				t.Error("expected nil cert/key on input-validation failure")
			}
			if fresh {
				t.Error("expected fresh=false on input-validation failure")
			}
			if !strings.Contains(err.Error(), "certPath and keyPath both required") {
				t.Errorf("unexpected error wording: %v", err)
			}
		})
	}
}

// TestLoadOrGenerateCA_GeneratesAndPersists pins the cold-start path:
// neither file exists, so a fresh CA is generated, written to both paths
// at the documented permissions (cert 0644 public, key 0600 secret), and
// fresh=true is reported.
func TestLoadOrGenerateCA_GeneratesAndPersists(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.pem")
	keyPath := filepath.Join(dir, "ca-key.pem")

	cert, key, fresh, err := LoadOrGenerateCA(certPath, keyPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !fresh {
		t.Error("expected fresh=true on first generation")
	}
	if cert == nil || key == nil {
		t.Fatal("expected non-nil cert+key")
	}
	if !cert.IsCA {
		t.Error("returned cert must be a CA")
	}
	// Files must exist with documented permissions (POSIX only — Windows
	// has no equivalent of Unix mode bits and reports 0666 for everything).
	certInfo, err := os.Stat(certPath)
	if err != nil {
		t.Fatalf("cert stat: %v", err)
	}
	keyInfo, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("key stat: %v", err)
	}
	if runtime.GOOS != "windows" {
		if certInfo.Mode().Perm() != 0o644 {
			t.Errorf("cert perm: want 0644, got %o", certInfo.Mode().Perm())
		}
		if keyInfo.Mode().Perm() != 0o600 {
			t.Errorf("key perm: want 0600, got %o", keyInfo.Mode().Perm())
		}
	}
}

// TestLoadOrGenerateCA_LoadsExisting pins the warm-start path: when both
// files already exist on disk, parses them and returns fresh=false. Pair
// of calls must produce the same cert+key (round-trip stability).
func TestLoadOrGenerateCA_LoadsExisting(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.pem")
	keyPath := filepath.Join(dir, "ca-key.pem")

	cert1, key1, fresh1, err := LoadOrGenerateCA(certPath, keyPath)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if !fresh1 {
		t.Fatal("first call should be fresh")
	}

	cert2, key2, fresh2, err := LoadOrGenerateCA(certPath, keyPath)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if fresh2 {
		t.Error("second call should be load (fresh=false)")
	}
	if !cert1.Equal(cert2) {
		t.Error("reload must produce equivalent CA cert")
	}
	if !key1.PublicKey.Equal(&key2.PublicKey) {
		t.Error("reload must produce equivalent CA key (PublicKey identity)")
	}
}

// TestLoadOrGenerateCA_GarbageCertPEM pins the malformed-cert-file arm:
// a non-PEM cert file present alongside a valid key returns a decode error
// (does not silently overwrite the operator's file).
func TestLoadOrGenerateCA_GarbageCertPEM(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.pem")
	keyPath := filepath.Join(dir, "ca-key.pem")

	// Generate a valid key first so only the cert is bad.
	_, _, _, err := LoadOrGenerateCA(certPath, keyPath)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := os.WriteFile(certPath, []byte("not a pem block"), 0o644); err != nil {
		t.Fatalf("write garbage: %v", err)
	}

	cert, key, fresh, err := LoadOrGenerateCA(certPath, keyPath)
	if err == nil {
		t.Fatal("expected error for non-PEM cert file")
	}
	if cert != nil || key != nil || fresh {
		t.Error("expected nil cert/key + fresh=false on decode error")
	}
	if !strings.Contains(err.Error(), "decode cert PEM") {
		t.Errorf("unexpected error wording: %v", err)
	}
}

// TestLoadOrGenerateCA_BadCertContents pins the parse-cert-after-decode
// arm: a valid PEM block whose bytes are NOT a valid x509 certificate
// triggers the ParseCertificate error path (distinct from the decode arm).
func TestLoadOrGenerateCA_BadCertContents(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.pem")
	keyPath := filepath.Join(dir, "ca-key.pem")

	_, _, _, err := LoadOrGenerateCA(certPath, keyPath)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Write a valid PEM block with garbage in the body — decode succeeds,
	// ParseCertificate fails.
	bad := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("not-a-cert")})
	if err := os.WriteFile(certPath, bad, 0o644); err != nil {
		t.Fatalf("write garbage: %v", err)
	}

	_, _, _, err = LoadOrGenerateCA(certPath, keyPath)
	if err == nil {
		t.Fatal("expected ParseCertificate error")
	}
	if !strings.Contains(err.Error(), "parse cert") {
		t.Errorf("unexpected error wording: %v", err)
	}
}

// TestLoadOrGenerateCA_GarbageKeyPEM pins the malformed-key-file arm —
// symmetrical to the bad-cert case but on the key path.
func TestLoadOrGenerateCA_GarbageKeyPEM(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.pem")
	keyPath := filepath.Join(dir, "ca-key.pem")

	_, _, _, err := LoadOrGenerateCA(certPath, keyPath)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := os.WriteFile(keyPath, []byte("not a pem block"), 0o600); err != nil {
		t.Fatalf("write garbage: %v", err)
	}

	_, _, _, err = LoadOrGenerateCA(certPath, keyPath)
	if err == nil {
		t.Fatal("expected error for non-PEM key file")
	}
	if !strings.Contains(err.Error(), "decode key PEM") {
		t.Errorf("unexpected error wording: %v", err)
	}
}

// TestLoadOrGenerateCA_BadKeyContents pins the EC-private-key parse
// failure: PEM block decodes, but bytes aren't a valid EC key.
func TestLoadOrGenerateCA_BadKeyContents(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.pem")
	keyPath := filepath.Join(dir, "ca-key.pem")

	_, _, _, err := LoadOrGenerateCA(certPath, keyPath)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	bad := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: []byte("not-a-key")})
	if err := os.WriteFile(keyPath, bad, 0o600); err != nil {
		t.Fatalf("write garbage: %v", err)
	}

	_, _, _, err = LoadOrGenerateCA(certPath, keyPath)
	if err == nil {
		t.Fatal("expected ParseECPrivateKey error")
	}
	if !strings.Contains(err.Error(), "parse key") {
		t.Errorf("unexpected error wording: %v", err)
	}
}

// TestLoadOrGenerateCA_WriteCertFails pins the cert-write-failure arm by
// pointing certPath at a path that contains a non-directory parent (file
// where a directory is expected). os.WriteFile then returns ENOTDIR.
func TestLoadOrGenerateCA_WriteCertFails(t *testing.T) {
	dir := t.TempDir()
	// Create a regular file, then ask LoadOrGenerateCA to write into a
	// nested path under it — os.WriteFile sees the parent isn't a dir.
	notADir := filepath.Join(dir, "blocker")
	if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed blocker: %v", err)
	}
	certPath := filepath.Join(notADir, "ca.pem")
	keyPath := filepath.Join(dir, "ca-key.pem")

	cert, key, fresh, err := LoadOrGenerateCA(certPath, keyPath)
	if err == nil {
		t.Fatal("expected write-cert error")
	}
	if cert != nil || key != nil || fresh {
		t.Error("expected zero values on write failure")
	}
	if !strings.Contains(err.Error(), "write") {
		t.Errorf("unexpected error wording: %v", err)
	}
}

// TestLoadOrGenerateCA_WriteKeyFails pins the key-write-failure arm:
// cert path is valid, key path is under a non-directory parent. Forces the
// SECOND os.WriteFile to fail (after the cert one already succeeded).
func TestLoadOrGenerateCA_WriteKeyFails(t *testing.T) {
	dir := t.TempDir()
	notADir := filepath.Join(dir, "blocker")
	if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed blocker: %v", err)
	}
	certPath := filepath.Join(dir, "ca.pem")
	keyPath := filepath.Join(notADir, "ca-key.pem")

	_, _, _, err := LoadOrGenerateCA(certPath, keyPath)
	if err == nil {
		t.Fatal("expected write-key error")
	}
	if !strings.Contains(err.Error(), "write") {
		t.Errorf("unexpected error wording: %v", err)
	}
	// Cert side should have been written before the key write failed —
	// proves we reached the key-write step, not the cert-write step.
	if _, err := os.Stat(certPath); err != nil {
		t.Errorf("cert file should exist (write happened before key failure): %v", err)
	}
}

// TestIssueLeafCertByHostname_HappyPath pins the documented hostname-only
// minting: CN = hostname, single SAN = hostname, 24h validity, signed by
// the engine's CA so a verifier rooted at the CA accepts the leaf for
// that hostname.
func TestIssueLeafCertByHostname_HappyPath(t *testing.T) {
	e, _ := NewEngine(nil, nil, 10, time.Hour)
	leaf, err := e.IssueLeafCertByHostname("api.openai.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if leaf == nil || len(leaf.CertDER) == 0 || len(leaf.CertPEM) == 0 || len(leaf.KeyPEM) == 0 {
		t.Fatal("leaf missing fields")
	}
	if leaf.Key == nil {
		t.Fatal("leaf private key missing")
	}

	parsed, err := x509.ParseCertificate(leaf.CertDER)
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	if parsed.Subject.CommonName != "api.openai.com" {
		t.Errorf("CN: want api.openai.com, got %s", parsed.Subject.CommonName)
	}
	if len(parsed.DNSNames) != 1 || parsed.DNSNames[0] != "api.openai.com" {
		t.Errorf("SANs: want [api.openai.com], got %v", parsed.DNSNames)
	}
	// 24h validity window with clock-skew tolerance.
	now := time.Now()
	if parsed.NotBefore.After(now) {
		t.Errorf("NotBefore should be in past (clock-skew): %v vs now %v", parsed.NotBefore, now)
	}
	expectedExpiry := now.Add(24 * time.Hour)
	if parsed.NotAfter.Sub(expectedExpiry).Abs() > time.Minute {
		t.Errorf("NotAfter should be ~24h out: got %v, want ~%v", parsed.NotAfter, expectedExpiry)
	}
	// Verifiable against the engine's CA.
	pool := x509.NewCertPool()
	pool.AddCert(e.CACert())
	if _, err := parsed.Verify(x509.VerifyOptions{Roots: pool, DNSName: "api.openai.com"}); err != nil {
		t.Errorf("leaf must verify against engine CA for the hostname: %v", err)
	}
}

// TestIssueLeafCertByHostname_CacheHit pins the cache-by-hostname behaviour:
// two issues for the same hostname return the same CachedCert pointer.
func TestIssueLeafCertByHostname_CacheHit(t *testing.T) {
	e, _ := NewEngine(nil, nil, 10, time.Hour)
	a, err := e.IssueLeafCertByHostname("api.openai.com")
	if err != nil {
		t.Fatalf("first issue: %v", err)
	}
	b, err := e.IssueLeafCertByHostname("api.openai.com")
	if err != nil {
		t.Fatalf("second issue: %v", err)
	}
	if a != b {
		t.Error("expected cache-hit to return same pointer for same hostname")
	}
	if e.CacheSize() != 1 {
		t.Errorf("cache size: want 1, got %d", e.CacheSize())
	}
}

// TestIssueLeafCertByHostname_CacheKeyDistinctFromFingerprint pins that
// hostname-keyed and fingerprint-keyed entries DO NOT collide — a host
// entry under "host:foo" must coexist alongside upstream fingerprints.
func TestIssueLeafCertByHostname_CacheKeyDistinctFromFingerprint(t *testing.T) {
	e, _ := NewEngine(nil, nil, 10, time.Hour)
	if _, err := e.IssueLeafCertByHostname("api.openai.com"); err != nil {
		t.Fatalf("hostname issue: %v", err)
	}
	upstream := makeUpstreamCert(t, "api.openai.com", []string{"api.openai.com"})
	if _, err := e.IssueLeafCert(upstream); err != nil {
		t.Fatalf("fingerprint issue: %v", err)
	}
	if e.CacheSize() != 2 {
		t.Errorf("expected 2 distinct cache entries (host + fingerprint), got %d", e.CacheSize())
	}
}

// TestIssueLeafCertByHostname_TTLExpiry pins that an expired cache entry
// is re-minted (not returned stale). Drives the time.Since > cacheTTL
// false-branch in the RLock check.
func TestIssueLeafCertByHostname_TTLExpiry(t *testing.T) {
	// 1-nanosecond TTL guarantees every read sees an "expired" entry.
	e, _ := NewEngine(nil, nil, 10, time.Nanosecond)
	a, err := e.IssueLeafCertByHostname("api.openai.com")
	if err != nil {
		t.Fatalf("first issue: %v", err)
	}
	time.Sleep(time.Millisecond)
	b, err := e.IssueLeafCertByHostname("api.openai.com")
	if err != nil {
		t.Fatalf("second issue: %v", err)
	}
	if a == b {
		t.Error("expected fresh mint after TTL expiry, got cache hit")
	}
}

// TestIssueLeafCertByHostname_EvictsWhenFull pins the 25%-eviction policy
// on the hostname path: maxCache=2, three distinct hostnames → second insert
// fills, third forces eviction of the oldest 25% (min 1 = 1 entry evicted),
// final size = 2 - 1 + 1 = 2.
func TestIssueLeafCertByHostname_EvictsWhenFull(t *testing.T) {
	e, _ := NewEngine(nil, nil, 2, time.Hour)
	for _, h := range []string{"a.example.com", "b.example.com", "c.example.com"} {
		if _, err := e.IssueLeafCertByHostname(h); err != nil {
			t.Fatalf("issue %s: %v", h, err)
		}
	}
	if got := e.CacheSize(); got != 2 {
		t.Errorf("cache size after eviction: want 2, got %d", got)
	}
}

// TestIssueLeafCertByHostname_EvictsLargeBatch pins the "evictCount > 1"
// arm of the eviction policy: maxCache=8, eight stored, one more forces
// eviction of 8/4 = 2 entries → final size = 8 - 2 + 1 = 7.
func TestIssueLeafCertByHostname_EvictsLargeBatch(t *testing.T) {
	e, _ := NewEngine(nil, nil, 8, time.Hour)
	for i := 0; i < 9; i++ {
		host := "host" + string(rune('a'+i)) + ".example.com"
		if _, err := e.IssueLeafCertByHostname(host); err != nil {
			t.Fatalf("issue: %v", err)
		}
	}
	if got := e.CacheSize(); got != 7 {
		t.Errorf("cache size after large-batch eviction: want 7 (8 - 2 + 1), got %d", got)
	}
}

// TestIssueLeafCert_EvictsLargeBatch mirrors the IssueLeafCertByHostname
// large-batch arm on the fingerprint-keyed path so both code paths' eviction
// helpers are exercised symmetrically.
func TestIssueLeafCert_EvictsLargeBatch(t *testing.T) {
	e, _ := NewEngine(nil, nil, 8, time.Hour)
	for i := 0; i < 9; i++ {
		host := "host" + string(rune('a'+i)) + ".example.com"
		up := makeUpstreamCert(t, host, []string{host})
		if _, err := e.IssueLeafCert(up); err != nil {
			t.Fatalf("issue: %v", err)
		}
	}
	if got := e.CacheSize(); got != 7 {
		t.Errorf("cache size after large-batch eviction: want 7 (8 - 2 + 1), got %d", got)
	}
}

// TestIssueLeafCertByHostname_Concurrent pins that the cache is safe under
// concurrent issuers — the documented sync.RWMutex contract. No race must
// fire under -race; final cache size must be exactly 1 for one hostname.
func TestIssueLeafCertByHostname_Concurrent(t *testing.T) {
	e, _ := NewEngine(nil, nil, 100, time.Hour)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := e.IssueLeafCertByHostname("api.openai.com"); err != nil {
				t.Errorf("concurrent issue: %v", err)
			}
		}()
	}
	wg.Wait()
	if e.CacheSize() != 1 {
		t.Errorf("after concurrent issues for one host: want size 1, got %d", e.CacheSize())
	}
}

// TestLeafKey_IsECDSA_P256 pins that the leaf key is the documented
// P-256 curve — guards against a silent algorithm downgrade if the
// generation parameters drift.
func TestLeafKey_IsECDSA_P256(t *testing.T) {
	e, _ := NewEngine(nil, nil, 10, time.Hour)
	leaf, err := e.IssueLeafCertByHostname("api.openai.com")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if leaf.Key.Curve != elliptic.P256() {
		t.Errorf("leaf key curve: want P-256, got %v", leaf.Key.Curve)
	}
	// Also pin the generated CA uses P-256.
	caKey, ok := any(e.caKey).(*ecdsa.PrivateKey)
	if !ok || caKey.Curve != elliptic.P256() {
		t.Error("CA key curve: want P-256")
	}
	// And rand.Reader is real (not nil) — sanity guard against test
	// shenanigans that might have replaced it.
	if rand.Reader == nil {
		t.Fatal("rand.Reader must not be nil")
	}
}
