// Package issuer provides certificate issuance for the compliance-proxy TLS
// interception (bump) layer — ECDSA signing, AES-GCM key encryption, and
// optional remote signing via a crypto.Signer.
package issuer

import (
	"context"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"os"
	"time"

	"golang.org/x/crypto/hkdf"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/metrics"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/tls/kms"
)

// certRandReader is the entropy source used by ECDSA key generation, serial
// number selection, x509.CreateCertificate signing randomization, and GCM
// nonce derivation. Production keeps it pointed at crypto/rand.Reader; tests
// swap it via the package-level variable to exercise entropy-failure branches
// in SignCert / EncryptPrivateKey that the standard library never surfaces
// in normal operation. Same seam pattern as agent/core/network/tls.
var certRandReader io.Reader = rand.Reader

// marshalECPrivKeyFn wraps x509.MarshalECPrivateKey so tests can inject a
// failing variant to exercise the error-handling arm inside NewIssuer that
// fires after a successful ecdsa.GenerateKey — a path the stdlib never
// surfaces in production because MarshalECPrivateKey only errors on
// unrecognised curves, which cannot happen for P-256.
// Test-only override; production never reassigns this variable.
var marshalECPrivKeyFn = x509.MarshalECPrivateKey

// newGCMFn wraps cipher.NewGCM so tests can inject a failing variant to
// exercise the error branches in EncryptPrivateKey and DecryptPrivateKey.
// cipher.NewGCM only errors when the block cipher's block size is not 16
// bytes, which cannot happen for a valid AES-256 key in production.
// Test-only override; production never reassigns this variable.
var newGCMFn = cipher.NewGCM

// hkdfReadFn wraps io.ReadFull for the HKDF key-derivation step in
// NewIssuer. An HKDF reader constructed from valid params never errors; this
// seam lets tests inject a failure to exercise the error-wrapping branch.
// Test-only override; production never reassigns this variable.
var hkdfReadFn = io.ReadFull

// Issuer signs leaf certificates using an enterprise CA.
type Issuer struct {
	caCert       *x509.Certificate
	caKey        *ecdsa.PrivateKey // nil when using remote signing
	remoteSigner crypto.Signer     // non-nil when using remote signing
	aesKey       []byte            // 32 bytes, derived from CA key or cert via HKDF
}

// NewIssuer loads CA cert and key from PEM files and derives the AES-256
// encryption key via HKDF for cache-layer encryption. The kms argument is
// optional: when nil or NoopProvider{}, the on-disk key file is expected to
// contain raw PEM bytes. When a real KMS provider is supplied, the file is
// treated as ciphertext and Decrypt is called once to unwrap before PEM parsing.
func NewIssuer(caCertPath, caKeyPath string, kmsProvider kms.KMSProvider) (*Issuer, error) {
	// Load CA certificate
	certPEM, err := os.ReadFile(caCertPath)
	if err != nil {
		return nil, fmt.Errorf("cert: read CA cert %s: %w", caCertPath, err)
	}
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("cert: no CERTIFICATE PEM block found in %s", caCertPath)
	}
	caCert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("cert: parse CA cert: %w", err)
	}

	// Load CA private key (optionally KMS-wrapped on disk).
	keyBlob, err := os.ReadFile(caKeyPath)
	if err != nil {
		return nil, fmt.Errorf("cert: read CA key %s: %w", caKeyPath, err)
	}
	if kmsProvider == nil {
		kmsProvider = kms.NoopProvider{}
	}
	keyPEM, err := kmsProvider.Decrypt(context.Background(), keyBlob)
	if err != nil {
		return nil, fmt.Errorf("cert: kms %s decrypt: %w", kmsProvider.Name(), err)
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil || keyBlock.Type != "EC PRIVATE KEY" {
		return nil, fmt.Errorf("cert: no EC PRIVATE KEY PEM block found in %s after %s decrypt", caKeyPath, kmsProvider.Name())
	}
	caKey, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("cert: parse CA key: %w", err)
	}

	// Derive AES-256 key via HKDF
	caKeyDER, err := marshalECPrivKeyFn(caKey)
	if err != nil {
		return nil, fmt.Errorf("cert: marshal CA key for HKDF: %w", err)
	}
	hkdfReader := hkdf.New(sha256.New, caKeyDER, []byte("nexus-cert-cache"), []byte("aes-256-gcm"))
	aesKey := make([]byte, 32)
	if _, err := hkdfReadFn(hkdfReader, aesKey); err != nil {
		return nil, fmt.Errorf("cert: HKDF derive AES key: %w", err)
	}

	return &Issuer{
		caCert: caCert,
		caKey:  caKey,
		aesKey: aesKey,
	}, nil
}

// SignCert creates a new ECDSA P-256 leaf certificate for the given hostname.
// The certificate has 24h validity, SAN set to hostname, and is signed by the CA.
func (i *Issuer) SignCert(hostname string) (*tls.Certificate, error) {
	start := time.Now()
	defer func() {
		if metrics.CertSignMs != nil {
			metrics.CertSignMs.With().Observe(float64(time.Since(start).Milliseconds()))
		}
	}()

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), certRandReader)
	if err != nil {
		return nil, fmt.Errorf("cert: generate leaf key: %w", err)
	}

	serialNumber, err := rand.Int(certRandReader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("cert: generate serial number: %w", err)
	}

	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName: hostname,
		},
		DNSNames:    []string{hostname},
		NotBefore:   now.Add(-2 * time.Minute), // back-date for clock skew tolerance
		NotAfter:    now.Add(24 * time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}

	// Use remote signer when available, otherwise fall back to local private key.
	var signer crypto.Signer
	if i.remoteSigner != nil {
		signer = i.remoteSigner
	} else {
		signer = i.caKey
	}
	leafDER, err := x509.CreateCertificate(certRandReader, template, i.caCert, &leafKey.PublicKey, signer)
	if err != nil {
		return nil, fmt.Errorf("cert: sign leaf cert: %w", err)
	}

	return &tls.Certificate{
		Certificate: [][]byte{leafDER, i.caCert.Raw},
		PrivateKey:  leafKey,
	}, nil
}

// EncryptPrivateKey encrypts an ECDSA private key using AES-256-GCM.
func (i *Issuer) EncryptPrivateKey(key *ecdsa.PrivateKey) (ciphertext, nonce []byte, err error) {
	plaintext, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("cert: marshal key for encryption: %w", err)
	}

	block, err := aes.NewCipher(i.aesKey)
	if err != nil {
		return nil, nil, fmt.Errorf("cert: create AES cipher: %w", err)
	}

	gcm, err := newGCMFn(block)
	if err != nil {
		return nil, nil, fmt.Errorf("cert: create GCM: %w", err)
	}

	nonce = make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(certRandReader, nonce); err != nil {
		return nil, nil, fmt.Errorf("cert: generate nonce: %w", err)
	}

	ciphertext = gcm.Seal(nil, nonce, plaintext, nil)
	return ciphertext, nonce, nil
}

// DecryptPrivateKey decrypts an AES-256-GCM encrypted ECDSA private key.
func (i *Issuer) DecryptPrivateKey(ciphertext, nonce []byte) (*ecdsa.PrivateKey, error) {
	block, err := aes.NewCipher(i.aesKey)
	if err != nil {
		return nil, fmt.Errorf("cert: create AES cipher: %w", err)
	}

	gcm, err := newGCMFn(block)
	if err != nil {
		return nil, fmt.Errorf("cert: create GCM: %w", err)
	}

	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("cert: decrypt key: %w", err)
	}

	key, err := x509.ParseECPrivateKey(plaintext)
	if err != nil {
		return nil, fmt.Errorf("cert: parse decrypted key: %w", err)
	}

	return key, nil
}

// CACert returns the loaded CA certificate for trust-pool construction
// and chain verification. The returned value is a reference to the
// in-memory cert; callers must not mutate it.
func (i *Issuer) CACert() *x509.Certificate {
	return i.caCert
}

// CACertPEM returns the CA certificate in PEM format for distribution.
// Only the public certificate is returned; the private key never leaves the Issuer.
func (i *Issuer) CACertPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: i.caCert.Raw})
}

// CACertExpiry returns the NotAfter time of the loaded CA certificate.
func (i *Issuer) CACertExpiry() time.Time {
	return i.caCert.NotAfter
}

// AESKey returns the derived AES key (for the cache layer).
func (i *Issuer) AESKey() []byte {
	dst := make([]byte, len(i.aesKey))
	copy(dst, i.aesKey)
	return dst
}
