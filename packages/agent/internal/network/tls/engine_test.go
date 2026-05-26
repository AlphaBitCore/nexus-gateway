package tls

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

func makeUpstreamCert(t *testing.T, cn string, sans []string) *x509.Certificate {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: cn},
		DNSNames:     sans,
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(365 * 24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create upstream cert: %v", err)
	}
	cert, _ := x509.ParseCertificate(der)
	return cert
}

func TestNewEngine_GeneratesCA(t *testing.T) {
	e, err := NewEngine(nil, nil, 10, time.Hour)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if e.CACert() == nil {
		t.Fatal("CA cert should not be nil")
	}
	if !e.CACert().IsCA {
		t.Error("CA cert should have IsCA=true")
	}
	pem := e.CACertPEM()
	if len(pem) == 0 {
		t.Error("CA PEM should not be empty")
	}
}

func TestIssueLeafCert_MatchesUpstream(t *testing.T) {
	e, _ := NewEngine(nil, nil, 10, time.Hour)
	upstream := makeUpstreamCert(t, "api.openai.com", []string{"api.openai.com", "*.openai.com"})

	leaf, err := e.IssueLeafCert(upstream)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	leafCert, err := x509.ParseCertificate(leaf.CertDER)
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}

	if leafCert.Subject.CommonName != "api.openai.com" {
		t.Errorf("expected CN api.openai.com, got %s", leafCert.Subject.CommonName)
	}
	if len(leafCert.DNSNames) != 2 {
		t.Errorf("expected 2 SANs, got %d", len(leafCert.DNSNames))
	}
}

func TestIssueLeafCert_SignedByCA(t *testing.T) {
	e, _ := NewEngine(nil, nil, 10, time.Hour)
	upstream := makeUpstreamCert(t, "api.openai.com", []string{"api.openai.com"})

	leaf, _ := e.IssueLeafCert(upstream)
	leafCert, _ := x509.ParseCertificate(leaf.CertDER)

	pool := x509.NewCertPool()
	pool.AddCert(e.CACert())
	if _, err := leafCert.Verify(x509.VerifyOptions{Roots: pool}); err != nil {
		t.Errorf("leaf should be verified by CA: %v", err)
	}
}

func TestLRUCache_Hit(t *testing.T) {
	e, _ := NewEngine(nil, nil, 10, time.Hour)
	upstream := makeUpstreamCert(t, "api.openai.com", []string{"api.openai.com"})

	leaf1, _ := e.IssueLeafCert(upstream)
	leaf2, _ := e.IssueLeafCert(upstream)

	// Same cert object should be returned from cache
	if leaf1 != leaf2 {
		t.Error("expected cache hit to return same pointer")
	}
	if e.CacheSize() != 1 {
		t.Errorf("expected cache size 1, got %d", e.CacheSize())
	}
}

func TestCache_ClearOnFull(t *testing.T) {
	e, _ := NewEngine(nil, nil, 2, time.Hour)

	for i := range 3 {
		upstream := makeUpstreamCert(t, "host"+string(rune('a'+i))+".com", []string{"host" + string(rune('a'+i)) + ".com"})
		_, _ = e.IssueLeafCert(upstream)
	}

	// Eviction removes 25% (min 1) when full, then adds the new entry: 2 - 1 + 1 = 2.
	if e.CacheSize() != 2 {
		t.Errorf("expected cache size 2 after evict-on-full, got %d", e.CacheSize())
	}
}

func TestLeafCert_HasPEM(t *testing.T) {
	e, _ := NewEngine(nil, nil, 10, time.Hour)
	upstream := makeUpstreamCert(t, "api.openai.com", []string{"api.openai.com"})

	leaf, _ := e.IssueLeafCert(upstream)
	if len(leaf.CertPEM) == 0 {
		t.Error("CertPEM should not be empty")
	}
	if len(leaf.KeyPEM) == 0 {
		t.Error("KeyPEM should not be empty")
	}
}
