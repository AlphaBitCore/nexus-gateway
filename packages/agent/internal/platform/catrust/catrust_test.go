package catrust

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"testing"
	"time"
)

// generateTestCA builds a minimal self-signed CA certificate for unit tests.
// It does NOT touch the OS trust store.
func generateTestCA(t *testing.T) *x509.Certificate {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-nexus-device-ca"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	return cert
}

// TestSystemPoolExcluding_ReturnsNonNilPool verifies that SystemPoolExcluding
// never panics and returns a usable, non-nil pool on success. On success
// (err == nil) every platform returns a non-nil pool. On the error path the
// linux variant returns (nil, err) so the caller (hub/client.go) falls back to
// the unfiltered system pool rather than silently receiving a pool that trusts
// no roots; the darwin/windows variants return a best-effort empty pool because
// their OS pool is opaque. The test therefore only asserts the success-path
// non-nil guarantee, which holds on every platform. It does not verify that the
// excluded cert is absent — that would require modifying the live cert store.
func TestSystemPoolExcluding_ReturnsNonNilPool(t *testing.T) {
	ca := generateTestCA(t)
	pool, err := SystemPoolExcluding(ca)
	if err != nil {
		// Acceptable: some CI environments have no system cert bundle.
		t.Logf("SystemPoolExcluding returned err (may be expected in sandboxed CI): %v", err)
		return
	}
	if pool == nil {
		t.Error("SystemPoolExcluding must return a non-nil pool on success (err == nil)")
	}
}

// TestSystemPoolExcluding_NilArgPanicsGracefully documents that passing a nil
// certToExclude is not a valid call — callers must always load the device CA
// before calling this function. This test verifies the function's interface
// contract (non-nil cert required) by confirming that a non-nil cert is accepted.
func TestSystemPoolExcluding_AcceptsValidCert(t *testing.T) {
	ca := generateTestCA(t)
	// Must not panic. On success (err == nil) the pool is non-nil; on the
	// no-bundle/SystemCertPool-failure path it is (nil, err) by contract.
	pool, err := SystemPoolExcluding(ca)
	if err == nil && pool == nil {
		t.Error("returned pool must be non-nil on success")
	}
}
