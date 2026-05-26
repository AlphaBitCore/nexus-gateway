// Package tls provides TLS inspection with a device CA and dynamic leaf certificates.
package tls

import (
	"cmp"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"slices"
	"sync"
	"time"
)

// tlsRandReader is the entropy source used by ECDSA key generation, serial-
// number selection, and x509 certificate creation in this package. It is a
// package-level variable solely so tests can substitute a failing reader and
// exercise the entropy-error branches; production code never reassigns it.
// Matches the same seam pattern used by packages/shared/identity/pkce and
// packages/nexus-hub/internal/identity/agentca.
var tlsRandReader io.Reader = rand.Reader

// Engine manages a device CA and generates dynamic leaf certificates for TLS inspection.
type Engine struct {
	caCert   *x509.Certificate
	caKey    *ecdsa.PrivateKey
	cache    map[string]*CachedCert // keyed by upstream cert fingerprint
	cacheMu  sync.RWMutex
	maxCache int
	cacheTTL time.Duration
}

// CachedCert is a cached leaf certificate.
type CachedCert struct {
	CertDER   []byte
	Key       *ecdsa.PrivateKey
	CertPEM   []byte
	KeyPEM    []byte
	CreatedAt time.Time
}

// NewEngine creates a TLS inspection engine. If caCert/caKey are nil, generates a self-signed CA.
func NewEngine(caCert *x509.Certificate, caKey *ecdsa.PrivateKey, maxCache int, cacheTTL time.Duration) (*Engine, error) {
	if caCert == nil || caKey == nil {
		var err error
		caCert, caKey, err = generateCA()
		if err != nil {
			return nil, fmt.Errorf("generate CA: %w", err)
		}
	}
	if maxCache == 0 {
		maxCache = 1000
	}
	if cacheTTL == 0 {
		cacheTTL = time.Hour
	}
	return &Engine{
		caCert:   caCert,
		caKey:    caKey,
		cache:    make(map[string]*CachedCert),
		maxCache: maxCache,
		cacheTTL: cacheTTL,
	}, nil
}

// LoadOrGenerateCA loads a device CA from disk if both files exist
// and are readable; otherwise generates a fresh self-signed CA and
// writes it to those paths. The certificate file is mode 0644 (the CA
// cert is public by design — it's what the OS trust store carries);
// the key file is mode 0600.
//
// This is the install-time entry point used by `nexus-agent install-ca`:
// the privileged install step creates + persists the CA once; the
// runtime daemon then loads it via NewEngine (with the loaded
// cert+key passed in) so the daemon never needs write access to
// /var/lib/nexus-agent at runtime.
//
// Returns the loaded-or-generated certificate, private key, and a
// boolean indicating whether the CA was freshly generated this call
// (so callers can decide whether to also (re-)install into the OS
// trust store).
func LoadOrGenerateCA(certPath, keyPath string) (*x509.Certificate, *ecdsa.PrivateKey, bool, error) {
	if certPath == "" || keyPath == "" {
		return nil, nil, false, errors.New("LoadOrGenerateCA: certPath and keyPath both required")
	}

	certBytes, certErr := os.ReadFile(certPath)
	keyBytes, keyErr := os.ReadFile(keyPath)
	if certErr == nil && keyErr == nil {
		// Both files present — parse and return.
		certBlock, _ := pem.Decode(certBytes)
		if certBlock == nil {
			return nil, nil, false, fmt.Errorf("decode cert PEM at %s", certPath)
		}
		cert, err := x509.ParseCertificate(certBlock.Bytes)
		if err != nil {
			return nil, nil, false, fmt.Errorf("parse cert %s: %w", certPath, err)
		}
		keyBlock, _ := pem.Decode(keyBytes)
		if keyBlock == nil {
			return nil, nil, false, fmt.Errorf("decode key PEM at %s", keyPath)
		}
		key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
		if err != nil {
			return nil, nil, false, fmt.Errorf("parse key %s: %w", keyPath, err)
		}
		return cert, key, false, nil
	}
	// At least one missing — generate fresh and persist.
	cert, key, err := generateCA()
	if err != nil {
		return nil, nil, false, fmt.Errorf("generate CA: %w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, false, fmt.Errorf("marshal key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	if err := os.WriteFile(certPath, certPEM, 0644); err != nil {
		return nil, nil, false, fmt.Errorf("write %s: %w", certPath, err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0600); err != nil {
		return nil, nil, false, fmt.Errorf("write %s: %w", keyPath, err)
	}
	return cert, key, true, nil
}

// CACert returns the CA certificate in PEM format.
func (e *Engine) CACertPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: e.caCert.Raw})
}

// CACert returns the raw CA certificate.
func (e *Engine) CACert() *x509.Certificate {
	return e.caCert
}

// IssueLeafCert generates a leaf certificate mimicking the upstream cert's Subject and SANs.
// IssueLeafCertByHostname mints a leaf certificate keyed only on the
// hostname — no upstream-cert probe required. Mirrors compliance-proxy's
// cert.Issuer.SignCert pattern: client trusts the device CA root, agent
// signs CN={hostname}, SAN={hostname}, 24h validity, done. The whole
// "fetch upstream's actual cert and clone its CN/SAN" dance that
// IssueLeafCert below requires is unnecessary — clients only validate
// SAN against the host they connected to, which is what we set here.
//
// Why this exists: the legacy IssueLeafCert path forced a pre-bump
// TLS probe of the upstream to get the cert. Strict-anti-bot upstreams
// (Cursor api2.cursor.sh, certain Cloudflare endpoints) reject vanilla
// Go TLS dials and forced agent into opaque-relay fallback —
// inspection lost. By mirroring cp's hostname-only minting, the bridge
// path skips the probe entirely and Cursor traffic flows through the
// proper bump pipeline (method/path/body/hooks all captured).
func (e *Engine) IssueLeafCertByHostname(hostname string) (*CachedCert, error) {
	// Cache by hostname (vs by upstream-cert fingerprint in the legacy
	// path). Same TTL bound; same eviction policy.
	cacheKey := "host:" + hostname
	e.cacheMu.RLock()
	if cached, ok := e.cache[cacheKey]; ok && time.Since(cached.CreatedAt) < e.cacheTTL {
		e.cacheMu.RUnlock()
		return cached, nil
	}
	e.cacheMu.RUnlock()

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), tlsRandReader)
	if err != nil {
		return nil, fmt.Errorf("generate leaf key: %w", err)
	}
	serial, err := rand.Int(tlsRandReader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("generate serial: %w", err)
	}

	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: hostname},
		DNSNames:     []string{hostname},
		NotBefore:    now.Add(-2 * time.Minute), // clock-skew tolerance
		NotAfter:     now.Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	certDER, err := x509.CreateCertificate(tlsRandReader, template, e.caCert, &leafKey.PublicKey, e.caKey)
	if err != nil {
		return nil, fmt.Errorf("create leaf cert: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		return nil, fmt.Errorf("marshal leaf key: %w", err)
	}
	cached := &CachedCert{
		CertDER:   certDER,
		Key:       leafKey,
		CertPEM:   pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER}),
		KeyPEM:    pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
		CreatedAt: now,
	}

	e.cacheMu.Lock()
	if len(e.cache) >= e.maxCache {
		// Same eviction policy as IssueLeafCert below.
		type aged struct {
			key string
			t   time.Time
		}
		entries := make([]aged, 0, len(e.cache))
		for k, v := range e.cache {
			entries = append(entries, aged{k, v.CreatedAt})
		}
		slices.SortFunc(entries, func(a, b aged) int {
			return cmp.Compare(a.t.UnixNano(), b.t.UnixNano())
		})
		evictCount := len(entries) / 4
		if evictCount < 1 {
			evictCount = 1
		}
		for i := range evictCount {
			delete(e.cache, entries[i].key)
		}
	}
	e.cache[cacheKey] = cached
	e.cacheMu.Unlock()
	return cached, nil
}

func (e *Engine) IssueLeafCert(upstreamCert *x509.Certificate) (*CachedCert, error) {
	sum := sha256.Sum256(upstreamCert.Raw)
	fingerprint := hex.EncodeToString(sum[:])

	// Check cache
	e.cacheMu.RLock()
	if cached, ok := e.cache[fingerprint]; ok && time.Since(cached.CreatedAt) < e.cacheTTL {
		e.cacheMu.RUnlock()
		return cached, nil
	}
	e.cacheMu.RUnlock()

	// Generate new leaf
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), tlsRandReader)
	if err != nil {
		return nil, fmt.Errorf("generate leaf key: %w", err)
	}

	serial, err := rand.Int(tlsRandReader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("generate serial: %w", err)
	}

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      upstreamCert.Subject,
		DNSNames:     upstreamCert.DNSNames,
		IPAddresses:  upstreamCert.IPAddresses,
		NotBefore:    time.Now().Add(-5 * time.Minute),
		NotAfter:     upstreamCert.NotAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}

	certDER, err := x509.CreateCertificate(tlsRandReader, template, e.caCert, &leafKey.PublicKey, e.caKey)
	if err != nil {
		return nil, fmt.Errorf("create leaf cert: %w", err)
	}

	keyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		return nil, fmt.Errorf("marshal leaf key: %w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	cached := &CachedCert{
		CertDER:   certDER,
		Key:       leafKey,
		CertPEM:   certPEM,
		KeyPEM:    keyPEM,
		CreatedAt: time.Now(),
	}

	// Store in cache
	e.cacheMu.Lock()
	if len(e.cache) >= e.maxCache {
		// Evict oldest 25% to avoid thundering-herd of simultaneous cache
		// misses that would occur if we cleared the entire cache at once.
		type aged struct {
			key string
			t   time.Time
		}
		entries := make([]aged, 0, len(e.cache))
		for k, v := range e.cache {
			entries = append(entries, aged{k, v.CreatedAt})
		}
		slices.SortFunc(entries, func(a, b aged) int {
			return cmp.Compare(a.t.UnixNano(), b.t.UnixNano())
		})
		evictCount := len(entries) / 4
		if evictCount < 1 {
			evictCount = 1
		}
		for i := range evictCount {
			delete(e.cache, entries[i].key)
		}
	}
	e.cache[fingerprint] = cached
	e.cacheMu.Unlock()

	return cached, nil
}

// CacheSize returns the current cache size.
func (e *Engine) CacheSize() int {
	e.cacheMu.RLock()
	defer e.cacheMu.RUnlock()
	return len(e.cache)
}

func generateCA() (*x509.Certificate, *ecdsa.PrivateKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), tlsRandReader)
	if err != nil {
		return nil, nil, err
	}

	serial, err := rand.Int(tlsRandReader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("generate CA serial: %w", err)
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "Nexus Agent Device CA", Organization: []string{"Nexus Gateway"}},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}

	certDER, err := x509.CreateCertificate(tlsRandReader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}

	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, nil, err
	}

	return cert, key, nil
}
