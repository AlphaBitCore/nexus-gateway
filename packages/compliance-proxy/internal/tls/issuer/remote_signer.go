package issuer

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"golang.org/x/crypto/hkdf"
)

// CommandSigner implements crypto.Signer by shelling out to an external
// KMS sign command. The CA private key never leaves the KMS — every
// x509.CreateCertificate call proxies through this signer.
//
// The command receives the digest as a temp file via {file} and is expected
// to write the raw DER signature to stdout. Same {file} placeholder pattern
// as KMS CommandProvider and SIEM CommandSink.
//
// Public() returns the CA's public key (loaded from the on-disk cert) so
// x509.CreateCertificate can encode it into the leaf's authority key ID.
type CommandSigner struct {
	pub     crypto.PublicKey
	args    []string
	timeout time.Duration
}

// NewCommandSigner creates a remote signer. The pubKey is extracted from
// the CA certificate (not the private key — which never exists locally
// in remote-signing mode). The args list may contain {file} for the
// digest temp file path.
func NewCommandSigner(pubKey crypto.PublicKey, args []string, timeout time.Duration) (*CommandSigner, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("cert/remote-signer: command args required")
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &CommandSigner{
		pub:     pubKey,
		args:    args,
		timeout: timeout,
	}, nil
}

// Public returns the CA's public key.
func (s *CommandSigner) Public() crypto.PublicKey {
	return s.pub
}

// Sign shells out to the configured KMS sign command. The digest is
// written to a temp file; the command's stdout is the raw DER signature.
//
// opts.HashFunc() is ignored — the external KMS is assumed to know the
// correct algorithm from its key configuration.
func (s *CommandSigner) Sign(_ io.Reader, digest []byte, _ crypto.SignerOpts) ([]byte, error) {
	tmp, err := os.CreateTemp("", "nexus-sign-digest-*.bin")
	if err != nil {
		return nil, fmt.Errorf("cert/remote-signer: create temp: %w", err)
	}
	defer os.Remove(tmp.Name()) //nolint:errcheck
	if _, err := tempWriteSignFn(tmp, digest); err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("cert/remote-signer: write digest: %w", err)
	}
	_ = tmp.Close()

	resolved := make([]string, len(s.args))
	for i, a := range s.args {
		resolved[i] = strings.ReplaceAll(a, "{file}", tmp.Name())
	}

	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, resolved[0], resolved[1:]...)
	out, err := cmd.Output()
	if err != nil {
		stderr := ""
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.Stderr != nil {
			stderr = strings.TrimSpace(string(exitErr.Stderr))
		}
		return nil, fmt.Errorf("cert/remote-signer: %s failed: %w (stderr: %s)",
			s.args[0], err, stderr)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("cert/remote-signer: command produced empty signature")
	}
	return out, nil
}

// NewIssuerWithRemoteSigner constructs an Issuer that uses a remote
// crypto.Signer instead of a local private key. The AES key for the cert
// cache is derived from the CA cert's raw bytes instead of the private key
// (which does not exist locally in remote-signing mode).
func NewIssuerWithRemoteSigner(caCertPath string, signer crypto.Signer) (*Issuer, error) {
	certPEM, err := os.ReadFile(caCertPath)
	if err != nil {
		return nil, fmt.Errorf("cert: read CA cert %s: %w", caCertPath, err)
	}
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("cert: no CERTIFICATE PEM block in %s", caCertPath)
	}
	caCert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("cert: parse CA cert: %w", err)
	}

	// Derive AES key from the cert raw bytes (no private key available).
	// This means the cache encryption key changes if the CA cert is
	// rotated, which is the correct behaviour — a new cert should not
	// be able to decrypt caches encrypted under the old one.
	aesKey := make([]byte, 32)
	hkdfReader := hkdfFromBytes(caCert.Raw)
	if _, err := hkdfReadRemoteFn(hkdfReader, aesKey); err != nil {
		return nil, fmt.Errorf("cert: HKDF derive AES key from cert: %w", err)
	}

	return &Issuer{
		caCert:       caCert,
		caKey:        nil, // no local private key
		remoteSigner: signer,
		aesKey:       aesKey,
	}, nil
}

// hkdfFromBytes is a small helper wrapping the same HKDF derivation used
// by the standard NewIssuer path but from arbitrary seed bytes.
func hkdfFromBytes(seed []byte) io.Reader {
	return hkdf.New(sha256.New, seed, []byte("nexus-cert-cache"), []byte("aes-256-gcm"))
}

// tempWriteSignFn wraps the (*os.File).Write call inside Sign so tests can
// inject a failing variant to exercise the error-handling arm that fires
// after os.CreateTemp succeeds. On a healthy POSIX filesystem Write to a
// just-opened temp file never fails; only fault-injection can reach this
// branch in production. Test-only override; production never reassigns this
// variable.
var tempWriteSignFn = func(f interface{ Write([]byte) (int, error) }, b []byte) (int, error) {
	return f.Write(b)
}

// hkdfReadRemoteFn wraps io.ReadFull for the HKDF key-derivation step in
// NewIssuerWithRemoteSigner. An HKDF reader constructed from valid cert bytes
// never errors; this seam lets tests inject a failure to exercise the
// error-wrapping branch. Test-only override; production never reassigns.
var hkdfReadRemoteFn = io.ReadFull

// Ensure CommandSigner satisfies crypto.Signer at compile time.
var _ crypto.Signer = (*CommandSigner)(nil)

// Ensure ecdsa.PrivateKey is still a valid signer reference (backwards compat).
var _ crypto.Signer = (*ecdsa.PrivateKey)(nil)

// Ensure P256 curve is accessible (used by SignCert).
var _ = elliptic.P256
