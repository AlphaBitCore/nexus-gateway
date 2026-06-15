// Package testutil provides helpers for generating test certificates and CAs.
package testutil

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// GenerateTestCA creates a self-signed ECDSA P-256 CA certificate and key for testing.
// Returns the parsed CA certificate, CA private key, and PEM-encoded bytes for both.
func GenerateTestCA() (caCert *x509.Certificate, caKey *ecdsa.PrivateKey, certPEM, keyPEM []byte, err error) {
	caKey, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, nil, nil, err
	}

	template := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"Nexus Gateway Test CA"},
			CommonName:   "Nexus Test CA",
		},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		// pathlen:0 mirrors the production proxy-CA recipes: the CA only ever
		// signs leaf certs, and the constraint stops a stolen CA key from
		// minting a subordinate CA. The issuer warns at load when the
		// constraint is absent, so the shared fixture carries it to keep that
		// warning a real signal in every test that builds an issuer.
		MaxPathLen:     0,
		MaxPathLenZero: true,
	}

	caDER, err := x509.CreateCertificate(rand.Reader, template, template, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	caCert, err = x509.ParseCertificate(caDER)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	certPEM = pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: caDER,
	})

	keyDER, err := x509.MarshalECPrivateKey(caKey)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{
		Type:  "EC PRIVATE KEY",
		Bytes: keyDER,
	})

	return caCert, caKey, certPEM, keyPEM, nil
}

// WriteTestCA writes test CA cert and key PEM files to the given directory.
// Returns paths to the cert and key files.
func WriteTestCA(dir string) (certPath, keyPath string, err error) {
	_, _, certPEM, keyPEM, err := GenerateTestCA()
	if err != nil {
		return "", "", err
	}

	certPath = filepath.Join(dir, "ca-cert.pem")
	keyPath = filepath.Join(dir, "ca-key.pem")

	if err := os.WriteFile(certPath, certPEM, 0600); err != nil {
		return "", "", err
	}
	if err := os.WriteFile(keyPath, keyPEM, 0600); err != nil {
		return "", "", err
	}

	return certPath, keyPath, nil
}
