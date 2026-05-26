// Package enrollment handles the device enrollment lifecycle.
package enrollment

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/identity/attestation"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/sync/hub"
	metricsplatform "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/platform"
)

// The following package-level variables exist solely as test seams so unit
// tests can exercise the post-CreateTemp mid-write error arms in
// writeFileAtomic (Chmod/Write/Sync/Close) and the crypto/rand-failure arms
// in Enroll/Renew (ecdsa.GenerateKey, x509.CreateCertificateRequest,
// MarshalECPrivateKey). Production never reassigns them. Mirrors the
// established pattern in packages/agent/internal/identity/secretstore/fallback.go
// (osFile + createTempFn) and packages/agent/internal/network/tls/engine.go
// (tlsRandReader).
var (
	createTempFn = func(dir, pattern string) (osFile, error) {
		return os.CreateTemp(dir, pattern)
	}
	randReader io.Reader = rand.Reader
)

// osFile is the subset of *os.File methods writeFileAtomic uses, named as an
// interface so test seams can return a mock implementation. Production code
// receives the real *os.File from os.CreateTemp.
type osFile interface {
	Name() string
	Chmod(mode os.FileMode) error
	Write(p []byte) (int, error)
	Sync() error
	Close() error
}

// writeFileAtomic writes data to path via a temp file in the same directory
// + fsync + rename, so a crash mid-write never leaves a partial file or a
// stale-but-readable previous version interleaved with new bytes. The temp
// is created with the target permissions (caller passes 0600 for the
// device key + cert pair).
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := createTempFn(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}

// State represents the enrollment state.
type State int

const (
	StateNotEnrolled State = iota
	StateEnrolled
	StateNeedsReenroll
)

// CertRenewer is the minimal subset of hub.Client needed to renew a
// device cert. Defined at the consumer side so tests can inject fakes
// without wiring a full mTLS client.
type CertRenewer interface {
	RenewCert(ctx context.Context, thingID, csrPEM string) (*hub.RenewCertResponse, error)
}

// Manager handles enrollment lifecycle. All exported methods are safe for
// concurrent use.
type Manager struct {
	hubEnroller HubEnroller
	renewer     CertRenewer
	certDir     string
	mu          sync.RWMutex
	thingID     string
	state       State
}

// NewManager creates an enrollment manager. certDir is where device
// artifacts (device.pem, device-key.pem, gateway-ca.pem, device-id,
// device-token, thing-id) live. Options install the Hub enroller and the
// optional cert renewer.
func NewManager(certDir string, opts ...ManagerOption) *Manager {
	m := &Manager{certDir: certDir}
	for _, o := range opts {
		o(m)
	}
	return m
}

// ManagerOption configures optional Manager behaviour.
type ManagerOption func(*Manager)

// WithHubEnroller installs the Hub enrollment client. Required for
// Enroll / Unenroll to contact the Hub.
func WithHubEnroller(h HubEnroller) ManagerOption {
	return func(m *Manager) { m.hubEnroller = h }
}

// WithCertRenewer installs the cert renewer used by Renew. Optional; when
// unset, Renew returns an error.
func WithCertRenewer(r CertRenewer) ManagerOption {
	return func(m *Manager) { m.renewer = r }
}

// IsEnrolled checks if a valid enrollment exists on disk. Requires ALL three
// of: mTLS cert (device.pem), mTLS key (device-key.pem), and bearer token
// (device-token). The cert+key give the agent an identity; the token is the
// runtime credential for authenticated Hub calls. Without the token, the
// daemon cannot heartbeat, drain audit, or fetch shadow config — so a
// cert-but-no-token state must boot the daemon into pre-enrollment mode
// rather than the full-stack path that would crash on the missing token.
//
// This is the post-sign-out state by design: auth.ClearEnrollment removes
// device-token + thing-id and intentionally LEAVES the cert+key so a
// subsequent re-enrollment with the same machine identity is fast. Treating
// that state as "still enrolled" was the source of a launchd respawn-loop
// bug — see daemon main() boot dispatch.
//
// The file-system check and state write are performed atomically under
// the lock to prevent TOCTOU races with concurrent Enroll/Unenroll calls.
func (m *Manager) IsEnrolled() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	certPath := filepath.Join(m.certDir, "device.pem")
	keyPath := filepath.Join(m.certDir, "device-key.pem")
	tokenPath := filepath.Join(m.certDir, "device-token")
	_, certErr := os.Stat(certPath)
	_, keyErr := os.Stat(keyPath)
	_, tokenErr := os.Stat(tokenPath)
	if certErr == nil && keyErr == nil && tokenErr == nil {
		m.state = StateEnrolled
		return true
	}
	return false
}

// ThingID returns the enrolled Thing ID (read from disk).
func (m *Manager) ThingID() string {
	m.mu.RLock()
	if m.thingID != "" {
		m.mu.RUnlock()
		return m.thingID
	}
	m.mu.RUnlock()

	idPath := filepath.Join(m.certDir, "device-id")
	data, err := os.ReadFile(idPath)
	if err != nil {
		return ""
	}
	m.mu.Lock()
	m.thingID = string(data)
	m.mu.Unlock()
	return m.thingID
}

// CertPaths returns the paths to the device cert and key.
func (m *Manager) CertPaths() (certFile, keyFile string) {
	return filepath.Join(m.certDir, "device.pem"), filepath.Join(m.certDir, "device-key.pem")
}

// TrustLevel returns the last-known trust_level for this device (0–3),
// read from disk on first call and cached in memory. Returns 0 when no
// trust-level file is present (device that never completed enrollment).
//
// The on-disk value is refreshed whenever Hub returns a new trustLevel
// in an enroll/renew response; it is NOT a live mirror of Hub state, so
// the menu-bar UI should treat it as a hint that may lag by one heartbeat.
func (m *Manager) TrustLevel() int {
	path := filepath.Join(m.certDir, "trust-level")
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	var level int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &level); err != nil {
		return 0
	}
	return level
}

// SSOEmail returns the email address the user signed in with during
// the most recent SSO enrollment, read from the persisted
// `sso-email` file in the cert directory. Returns an empty string
// when the device was enrolled via the legacy X-Enrollment-Token
// path (mtls-only mode) or has never been enrolled. The menu bar
// renders the value as the "current SSO identity" row in the menu bar.
func (m *Manager) SSOEmail() string {
	path := filepath.Join(m.certDir, "sso-email")
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// PersistSSOEmail writes the email address acquired during the SSO
// enrollment flow to disk so the menu bar can surface it across
// agent restarts. Empty input is a no-op (the caller may pass the
// raw CP response unconditionally). Atomic via writeFileAtomic so
// a crash never leaves a half-written file.
func (m *Manager) PersistSSOEmail(email string) error {
	if email == "" {
		return nil
	}
	return writeFileAtomic(filepath.Join(m.certDir, "sso-email"), []byte(email), 0600)
}

// Enroll generates a local keypair, creates a CSR, and asks the Hub to
// sign it via POST /api/internal/things/enroll. The private key never
// leaves the device.
func (m *Manager) Enroll(ctx context.Context, token, hostname, osName, osVersion, agentVersion string) error {
	if m.hubEnroller == nil {
		return fmt.Errorf("enrollment: hub enroller is not configured")
	}

	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), randReader)
	if err != nil {
		return fmt.Errorf("generate keypair: %w", err)
	}

	csrTemplate := &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: fmt.Sprintf("device-%s", hostname)},
	}
	csrDER, err := x509.CreateCertificateRequest(randReader, csrTemplate, privateKey)
	if err != nil {
		return fmt.Errorf("create CSR: %w", err)
	}
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})

	if err := os.MkdirAll(m.certDir, 0700); err != nil {
		return fmt.Errorf("create cert dir: %w", err)
	}

	keyDER, err := x509.MarshalECPrivateKey(privateKey)
	if err != nil {
		return fmt.Errorf("marshal private key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	// Generate a parallel Ed25519 keypair for traffic attestation.
	// Fail-open — if any step fails we still enroll for mTLS (the
	// device works without attestation). The Ed25519 private key
	// never leaves the device; only the CSR is sent.
	attestCsrPEM, attestKeyPEM := generateAttestationKeyMaterial(hostname)

	hubResp, hubErr := m.hubEnroller.Enroll(ctx, token, HubEnrollRequest{
		Version:           agentVersion,
		CsrPEM:            string(csrPEM),
		Hostname:          hostname,
		OS:                osName,
		OSVersion:         osVersion,
		DeviceFingerprint: metricsplatform.ComputeDeviceFingerprint(),
		AttestationCsrPem: attestCsrPEM,
	})
	if hubErr != nil {
		return fmt.Errorf("enrollment failed: %w", hubErr)
	}
	return m.persistHubEnrollment(hubResp, keyPEM, attestKeyPEM)
}

// generateAttestationKeyMaterial produces the Ed25519 keypair + CSR the
// agent ships to Hub for the attestation cert. Fail-open: on any crypto
// error returns ("", nil) so the surrounding Enroll path still completes
// for the mTLS cert. Returns (csrPEM, privateKeyPEM) — both empty when
// generation failed; both non-empty on success.
func generateAttestationKeyMaterial(hostname string) (string, []byte) {
	_, priv, err := ed25519.GenerateKey(randReader)
	if err != nil {
		return "", nil
	}
	csrTmpl := &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: fmt.Sprintf("device-%s-attestation", hostname)},
	}
	der, err := x509.CreateCertificateRequest(randReader, csrTmpl, priv)
	if err != nil {
		return "", nil
	}
	csrPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))
	keyPEM, err := attestation.MarshalEd25519PrivateKeyPEM(priv)
	if err != nil {
		return "", nil
	}
	return csrPEM, keyPEM
}

// persistHubEnrollment writes Hub enrollment artifacts to disk atomically:
// each file is written to a *.tmp sibling, fsync'd, and renamed into place.
// A previous version iterated map[string]string in random order with a
// plain os.WriteFile per file, so a crash between writing device.pem and
// device-key.pem left a mismatched cert/key pair on disk and every
// subsequent Hub call failed at tls.X509KeyPair. The atomic-rename pattern
// matches the updater's swap and the secretstore writer.
func (m *Manager) persistHubEnrollment(resp *HubEnrollResponse, keyPEM []byte, attestKeyPEM []byte) error {
	files := []struct {
		name    string
		content string
	}{
		// Order matters: write the private key first, then the cert that
		// references it, then the CA, then the identity files. A crash
		// between any two leaves the prior pair consistent.
		{"device-key.pem", string(keyPEM)},
		{"device.pem", resp.CertPEM},
		{"gateway-ca.pem", resp.CaCertPEM},
		{"device-id", resp.ID},
		{"thing-id", resp.ID},
		{"device-token", resp.DeviceToken},
		{"trust-level", fmt.Sprintf("%d", resp.TrustLevel)},
		// Attestation artifacts. Both keyed empty when Hub didn't sign
		// the Ed25519 CSR; the writeFileAtomic loop skips empty entries
		// so absence-of-file remains the "attestation not available yet"
		// signal the signer reads.
		{"attestation-key.pem", string(attestKeyPEM)},
		{"attestation.pem", resp.AttestationCertPem},
	}
	for _, f := range files {
		if f.content == "" {
			continue
		}
		if err := writeFileAtomic(filepath.Join(m.certDir, f.name), []byte(f.content), 0600); err != nil {
			return fmt.Errorf("write %s: %w", f.name, err)
		}
	}

	m.mu.Lock()
	m.thingID = resp.ID
	m.state = StateEnrolled
	m.mu.Unlock()
	slog.Info("device enrolled via hub", "thing_id", resp.ID,
		"attestation_enrolled", resp.AttestationCertPem != "")
	return nil
}

// PersistEnrollment writes Hub enrollment artifacts to disk atomically.
// It is the public counterpart of persistHubEnrollment, used by the SSO
// enrollment path which calls Hub directly (without the token-based flow).
// The optional attestKeyPEM carries the agent-generated Ed25519 private
// key matching resp.AttestationCertPem; pass nil from call sites that
// did not initiate the attestation CSR side-channel.
func (m *Manager) PersistEnrollment(resp *HubEnrollResponse, keyPEM []byte, attestKeyPEM []byte) error {
	return m.persistHubEnrollment(resp, keyPEM, attestKeyPEM)
}

// Unenroll deletes local certs and notifies the Hub via
// POST /api/internal/things/deregister when a device token is present.
func (m *Manager) Unenroll(ctx context.Context) error {
	thingID := m.ThingID()

	if thingID != "" && m.hubEnroller != nil {
		tokenPath := filepath.Join(m.certDir, "device-token")
		if tokenData, err := os.ReadFile(tokenPath); err == nil {
			if err := m.hubEnroller.Deregister(ctx, string(tokenData), thingID, "user requested unenroll"); err != nil {
				slog.Warn("failed to deregister via hub", "error", err)
			}
		}
	}

	for _, f := range []string{"device.pem", "device-key.pem", "gateway-ca.pem", "device-id", "device-token", "thing-id", "attestation-key.pem", "attestation.pem"} {
		_ = os.Remove(filepath.Join(m.certDir, f))
	}

	m.mu.Lock()
	m.thingID = ""
	m.state = StateNotEnrolled
	m.mu.Unlock()
	slog.Info("device unenrolled")
	return nil
}

// Renew generates a new keypair and CSR, sends it to the Hub for signing
// via POST /api/internal/things/renew-cert, and replaces the local
// certificate files. The old private key is overwritten.
func (m *Manager) Renew(ctx context.Context) error {
	if m.renewer == nil {
		return fmt.Errorf("cert renewal: renewer is not configured")
	}
	thingID := m.ThingID()
	if thingID == "" {
		return fmt.Errorf("cert renewal: thing id is empty, cannot renew")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	newKey, err := ecdsa.GenerateKey(elliptic.P256(), randReader)
	if err != nil {
		return fmt.Errorf("generate renewal keypair: %w", err)
	}

	hostname, _ := os.Hostname()
	csrTemplate := &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: fmt.Sprintf("device-%s", hostname)},
	}
	csrDER, err := x509.CreateCertificateRequest(randReader, csrTemplate, newKey)
	if err != nil {
		return fmt.Errorf("create renewal CSR: %w", err)
	}
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})

	resp, err := m.renewer.RenewCert(ctx, thingID, string(csrPEM))
	if err != nil {
		return fmt.Errorf("renewal request: %w", err)
	}

	keyDER, err := x509.MarshalECPrivateKey(newKey)
	if err != nil {
		return fmt.Errorf("marshal renewal key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	// Same atomic-rename + key-first ordering as persistHubEnrollment so a
	// crash mid-renew does not leave the agent with a mismatched
	// cert/key pair that would fail every subsequent Hub call.
	renewedFiles := []struct {
		name    string
		content string
	}{
		{"device-key.pem", string(keyPEM)},
		{"device.pem", resp.Certificate},
		{"gateway-ca.pem", resp.GatewayCA},
	}
	for _, f := range renewedFiles {
		if err := writeFileAtomic(filepath.Join(m.certDir, f.name), []byte(f.content), 0600); err != nil {
			return fmt.Errorf("write renewed %s: %w", f.name, err)
		}
	}

	slog.Info("device cert renewed", "expiresAt", resp.ExpiresAt)
	return nil
}

// MarkNeedsReenroll transitions to re-enrollment state.
func (m *Manager) MarkNeedsReenroll() {
	m.mu.Lock()
	m.state = StateNeedsReenroll
	m.mu.Unlock()
	slog.Warn("device needs re-enrollment")
}

// GetState returns the current enrollment state.
func (m *Manager) GetState() State {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.state
}
