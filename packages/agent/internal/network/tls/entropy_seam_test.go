package tls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"math/big"
	"strings"
	"testing"
	"time"
)

// failReader is an io.Reader that always returns a sentinel error. It drives
// the one rand consumer that still reads the injected reader: rand.Int for
// serial numbers. Key generation and certificate signing take their
// randomness from the FIPS module, not from this reader, so their error
// branches are driven through the tlsGenerateKey / tlsCreateCertificate
// function seams instead.
type failReader struct{ err error }

func (f failReader) Read(_ []byte) (int, error) { return 0, f.err }

// swapTLSRandReader injects a reader, returning a restore func.
func swapTLSRandReader(t *testing.T, r io.Reader) func() {
	t.Helper()
	orig := tlsRandReader
	tlsRandReader = r
	return func() { tlsRandReader = orig }
}

// failKeyGen makes tlsGenerateKey fail, returning a restore func.
func failKeyGen(t *testing.T, err error) func() {
	t.Helper()
	orig := tlsGenerateKey
	tlsGenerateKey = func(elliptic.Curve, io.Reader) (*ecdsa.PrivateKey, error) { return nil, err }
	return func() { tlsGenerateKey = orig }
}

// failCertSign makes tlsCreateCertificate fail, returning a restore func.
func failCertSign(t *testing.T, err error) func() {
	t.Helper()
	orig := tlsCreateCertificate
	tlsCreateCertificate = func(io.Reader, *x509.Certificate, *x509.Certificate, any, any) ([]byte, error) {
		return nil, err
	}
	return func() { tlsCreateCertificate = orig }
}

func mustValidCA(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("CA key: %v", err)
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("CA cert: %v", err)
	}
	cert, _ := x509.ParseCertificate(der)
	return cert, caKey
}

// generateCA path — one test per failure arm, in call order.

func TestGenerateCA_KeyGenError(t *testing.T) {
	want := errors.New("key generation refused")
	defer failKeyGen(t, want)()

	_, _, err := generateCA()
	if err == nil {
		t.Fatal("generateCA must surface the key-generation error")
	}
	// generateCA returns the first arm's error verbatim (no wrap).
	if !errors.Is(err, want) {
		t.Errorf("err should carry the sentinel; got %q", err)
	}
}

func TestGenerateCA_SerialEntropyError(t *testing.T) {
	defer swapTLSRandReader(t, failReader{err: errors.New("starved")})()

	_, _, err := generateCA()
	if err == nil || !strings.Contains(err.Error(), "generate CA serial") {
		t.Fatalf("starved serial draw should wrap %q; got %v", "generate CA serial", err)
	}
}

func TestGenerateCA_CertSignError(t *testing.T) {
	want := errors.New("signing refused")
	defer failCertSign(t, want)()

	_, _, err := generateCA()
	if err == nil {
		t.Fatal("generateCA must surface the certificate-signing error")
	}
	if !errors.Is(err, want) {
		t.Errorf("err should carry the sentinel; got %q", err)
	}
}

func TestNewEngine_GenerateCAErrorWraps(t *testing.T) {
	defer swapTLSRandReader(t, failReader{err: errors.New("entropy starved")})()

	_, err := NewEngine(nil, nil, 0, 0)
	if err == nil {
		t.Fatal("NewEngine with nil CA + failing entropy must surface error")
	}
	if !strings.Contains(err.Error(), "generate CA") {
		t.Errorf("err should wrap 'generate CA'; got %q", err)
	}
}

func TestLoadOrGenerateCA_GenerateError(t *testing.T) {
	defer swapTLSRandReader(t, failReader{err: errors.New("entropy")})()

	dir := t.TempDir()
	cert, key, fresh, err := LoadOrGenerateCA(dir+"/ca.crt", dir+"/ca.key")
	if err == nil {
		t.Fatal("LoadOrGenerateCA missing files + failing entropy must error")
	}
	if cert != nil || key != nil || fresh {
		t.Errorf("error path must return (nil,nil,false,err); got cert=%v key=%v fresh=%v", cert, key, fresh)
	}
	if !strings.Contains(err.Error(), "generate CA") {
		t.Errorf("err should wrap 'generate CA'; got %q", err)
	}
}

// IssueLeafCertByHostname path — one test per failure arm, in call order.

func TestIssueLeafCertByHostname_GenerateKeyError(t *testing.T) {
	caCert, caKey := mustValidCA(t)
	eng, err := NewEngine(caCert, caKey, 10, time.Hour)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer failKeyGen(t, errors.New("key generation refused"))()

	_, err = eng.IssueLeafCertByHostname("example.com")
	if err == nil {
		t.Fatal("must surface the leaf key-generation error")
	}
	if !strings.Contains(err.Error(), "generate leaf key") {
		t.Errorf("err should wrap 'generate leaf key'; got %q", err)
	}
}

func TestIssueLeaf_SerialEntropyError(t *testing.T) {
	caCert, caKey := mustValidCA(t)
	eng, err := NewEngine(caCert, caKey, 10, time.Hour)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer swapTLSRandReader(t, failReader{err: errors.New("starved")})()

	if _, err := eng.IssueLeafCertByHostname("starved-serial.example"); err == nil ||
		!strings.Contains(err.Error(), "generate serial") {
		t.Fatalf("starved serial draw should wrap %q; got %v", "generate serial", err)
	}
}

func TestIssueLeaf_CertSignError(t *testing.T) {
	caCert, caKey := mustValidCA(t)
	eng, err := NewEngine(caCert, caKey, 10, time.Hour)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer failCertSign(t, errors.New("signing refused"))()

	_, err = eng.IssueLeafCertByHostname("starved-cert.example")
	if err == nil {
		t.Fatal("must surface the leaf certificate-signing error")
	}
	if !strings.Contains(err.Error(), "create leaf cert") {
		t.Errorf("err should wrap 'create leaf cert'; got %q", err)
	}
}

// Production-default pin: the package init wires the real primitives.

func TestTLSSeams_ProductionDefaults(t *testing.T) {
	if tlsRandReader == nil {
		t.Error("tlsRandReader must not be nil at package init")
	}
	if tlsGenerateKey == nil || tlsCreateCertificate == nil {
		t.Error("tlsGenerateKey / tlsCreateCertificate must not be nil at package init")
	}
}
