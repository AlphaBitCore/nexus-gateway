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
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/identity/attestation"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/identity/keystore"
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

// TokenRenewer is the minimal subset of hub.Client needed to rotate the device
// bearer token. The call authenticates with the current (still-valid)
// token via the client's injected DeviceToken/ThingID headers, so it carries no
// arguments. Defined at the consumer side for fake injection in tests.
type TokenRenewer interface {
	RenewDeviceToken(ctx context.Context) (*hub.RenewTokenResponse, error)
}

// DeviceTokenRenewWindow is how long before expiry the agent rotates its device
// token. With Hub's 30-day TTL this leaves a multi-day window of renewal
// attempts before the token could lapse, so a transient Hub outage cannot strand
// the agent on an expired token; rotation happens while the current token is
// still valid (the refresh-while-valid discipline Hub's renew-token endpoint
// depends on).
const DeviceTokenRenewWindow = 7 * 24 * time.Hour

// Manager handles enrollment lifecycle. All exported methods are safe for
// concurrent use.
type Manager struct {
	hubEnroller  HubEnroller
	tokenRenewer TokenRenewer
	certDir      string
	store        keystore.Store // platform keystore for the attestation key
	mu           sync.RWMutex
	thingID      string
	state        State
}

// NewManager creates an enrollment manager. certDir is where device
// artifacts (device.pem, device-key.pem, device-id, device-token,
// thing-id) live. Options install the Hub enroller and the optional
// device-token renewer. The attestation private key is held in the
// keystore injected via WithKeyStore, NOT under certDir. Callers MUST
// inject one: production passes the platform store from its composition
// root, tests pass keystore.NewMemoryStore(). There is deliberately no
// platform-store default here — constructing the real Keychain/DPAPI
// store outside the composition root makes `go test` prompt for OS
// authorization (enforced by scripts/check-keystore-seam.sh).
func NewManager(certDir string, opts ...ManagerOption) *Manager {
	m := &Manager{certDir: certDir}
	for _, o := range opts {
		o(m)
	}
	if m.store == nil {
		m.store = keystore.NewMemoryStore()
	}
	return m
}

// ManagerOption configures optional Manager behaviour.
type ManagerOption func(*Manager)

// WithKeyStore overrides the platform keystore the manager Sets/Deletes
// the attestation private key in. Production leaves this unset (the real
// platform store); tests inject keystore.NewMemoryStore() so they never
// touch the host Keychain/DPAPI.
func WithKeyStore(store keystore.Store) ManagerOption {
	return func(m *Manager) { m.store = store }
}

// WithHubEnroller installs the Hub enrollment client. Required for
// Enroll / Unenroll to contact the Hub.
func WithHubEnroller(h HubEnroller) ManagerOption {
	return func(m *Manager) { m.hubEnroller = h }
}

// WithTokenRenewer installs the device-token renewer used by RenewDeviceToken.
// Optional; when unset, RenewDeviceToken returns an error.
func WithTokenRenewer(r TokenRenewer) ManagerOption {
	return func(m *Manager) { m.tokenRenewer = r }
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

// Enroll generates a local device keypair + self-signed identity cert and
// registers the device with the Hub via POST /api/internal/things/enroll.
// The private key never leaves the device. Hub authentication is by the
// device bearer token Hub mints in the response; the device cert is a
// local identity artifact only (Hub does not verify it).
func (m *Manager) Enroll(ctx context.Context, token, hostname, osName, osVersion, agentVersion string) error {
	if m.hubEnroller == nil {
		return fmt.Errorf("enrollment: hub enroller is not configured")
	}

	keyPEM, certPEM, err := generateDeviceIdentity(hostname)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(m.certDir, 0700); err != nil {
		return fmt.Errorf("create cert dir: %w", err)
	}

	// Generate a parallel Ed25519 keypair for traffic attestation.
	// Fail-open — if any step fails we still enroll (the device works
	// without attestation). The Ed25519 private key never leaves the
	// device; only the CSR is sent.
	attestCsrPEM, attestKeyPEM := generateAttestationKeyMaterial(hostname)

	hubResp, hubErr := m.hubEnroller.Enroll(ctx, token, HubEnrollRequest{
		Version:           agentVersion,
		Hostname:          hostname,
		OS:                osName,
		OSVersion:         osVersion,
		DeviceFingerprint: metricsplatform.ComputeDeviceFingerprint(),
		AttestationCsrPem: attestCsrPEM,
	})
	if hubErr != nil {
		return fmt.Errorf("enrollment failed: %w", hubErr)
	}
	return m.persistHubEnrollment(hubResp, keyPEM, certPEM, attestKeyPEM)
}

// generateDeviceIdentity creates the device's P-256 keypair and a
// self-signed X.509 identity certificate. The cert is the agent's local
// identity artifact (used as the mTLS client cert on outbound Hub calls
// and surfaced in the device status UI); Hub does not verify it, so it is
// self-signed rather than CA-signed. Returns (keyPEM, certPEM).
func generateDeviceIdentity(hostname string) (keyPEM, certPEM []byte, err error) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), randReader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate keypair: %w", err)
	}

	serial, err := rand.Int(randReader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("generate cert serial: %w", err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: fmt.Sprintf("device-%s", hostname)},
		NotBefore:    now,
		NotAfter:     now.AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	certDER, err := x509.CreateCertificate(randReader, tmpl, tmpl, &privateKey.PublicKey, privateKey)
	if err != nil {
		return nil, nil, fmt.Errorf("create device cert: %w", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})

	keyDER, err := x509.MarshalECPrivateKey(privateKey)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal private key: %w", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return keyPEM, certPEM, nil
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
func (m *Manager) persistHubEnrollment(resp *HubEnrollResponse, keyPEM, certPEM []byte, attestKeyPEM []byte) error {
	files := []struct {
		name    string
		content string
	}{
		// Order matters: write the private key first, then the cert that
		// references it, then the identity files. A crash between any two
		// leaves the prior pair consistent.
		{"device-key.pem", string(keyPEM)},
		{"device.pem", string(certPEM)},
		{"device-id", resp.ID},
		{"thing-id", resp.ID},
		{"device-token", resp.DeviceToken},
		// Device-token expiry. The loop skips empty entries, so a
		// Hub that predates token expiry leaves no file and the renewal
		// scheduler treats the token as "renew now" on the first tick.
		{"device-token-expires", resp.DeviceTokenExpiresAt},
		{"trust-level", fmt.Sprintf("%d", resp.TrustLevel)},
		// Attestation CERT (public). Empty when Hub didn't sign the Ed25519
		// CSR; the writeFileAtomic loop skips empty entries. The attestation
		// PRIVATE KEY is NOT in this on-disk batch — see below.
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

	// The Ed25519 attestation private key — whose possession
	// alone forges traffic attestation and bypasses compliance inspection —
	// is held in the platform keystore (macOS Keychain / Windows DPAPI /
	// Linux 0600 file), NOT as a plaintext PEM under certDir, so a host or
	// backup filesystem read does not hand over the signing key. Skip when
	// Hub didn't issue an Ed25519 cert (older Hub / attestation off); the
	// signer reads keystore-absence as "attestation not available yet". A
	// Set failure fails the enrollment (it is retried) rather than silently
	// leaving the key unpersisted.
	if len(attestKeyPEM) > 0 {
		if err := m.store.Set(keystore.AttestationKeyName, attestKeyPEM); err != nil {
			return fmt.Errorf("persist attestation key to keystore: %w", err)
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
// keyPEM / certPEM are the device identity keypair + self-signed cert.
// The optional attestKeyPEM carries the agent-generated Ed25519 private
// key matching resp.AttestationCertPem; pass nil from call sites that
// did not initiate the attestation CSR side-channel.
func (m *Manager) PersistEnrollment(resp *HubEnrollResponse, keyPEM, certPEM []byte, attestKeyPEM []byte) error {
	return m.persistHubEnrollment(resp, keyPEM, certPEM, attestKeyPEM)
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

	for _, f := range []string{"device.pem", "device-key.pem", "gateway-ca.pem", "device-id", "device-token", "device-token-expires", "thing-id", "attestation.pem"} {
		_ = os.Remove(filepath.Join(m.certDir, f))
	}
	// The attestation private key lives in the platform keystore,
	// not under certDir — delete it there so a decommission leaves no usable
	// signing key behind. Best-effort, mirroring the file removes above.
	if m.store != nil {
		_ = m.store.Delete(keystore.AttestationKeyName)
	}

	m.mu.Lock()
	m.thingID = ""
	m.state = StateNotEnrolled
	m.mu.Unlock()
	slog.Info("device unenrolled")
	return nil
}

// DeviceTokenExpiry reads the persisted device-token expiry from disk. Returns
// the zero time and an error when the expiry file is absent or unparseable —
// which the renewal scheduler treats as "renew now" so a legacy enrollment
// without a recorded expiry self-heals onto a bounded token.
func (m *Manager) DeviceTokenExpiry() (time.Time, error) {
	path := filepath.Join(m.certDir, "device-token-expires")
	data, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}, fmt.Errorf("read device token expiry: %w", err)
	}
	exp, err := time.Parse(time.RFC3339, strings.TrimSpace(string(data)))
	if err != nil {
		return time.Time{}, fmt.Errorf("parse device token expiry: %w", err)
	}
	return exp, nil
}

// DeviceTokenNeedsRenewal reports whether the device token is within
// DeviceTokenRenewWindow of expiry (or its expiry is unknown), as of `now`.
// True means the renewal scheduler should rotate the token. A missing or
// unparseable expiry returns true so the agent rotates a legacy/unbounded token
// rather than running it indefinitely.
func (m *Manager) DeviceTokenNeedsRenewal(now time.Time) bool {
	exp, err := m.DeviceTokenExpiry()
	if err != nil {
		return true
	}
	return !now.Before(exp.Add(-DeviceTokenRenewWindow))
}

// RenewDeviceToken rotates the device bearer token: it asks Hub for a fresh
// token (authenticated by the current still-valid token), then atomically
// replaces the on-disk `device-token` + `device-token-expires` files. Hub
// overwrites the stored hash as part of the same call, so the previous token is
// invalidated server-side the moment Hub responds — a stolen token's replay
// window is bounded by the rotation period.
//
// The token file is written before the expiry file: if a crash interleaves the
// two, the agent is left with the new token (the security-critical artifact) and
// at worst a stale-shorter expiry, which only triggers an earlier, harmless
// re-rotation — never a longer-lived token than Hub granted.
func (m *Manager) RenewDeviceToken(ctx context.Context) error {
	if m.tokenRenewer == nil {
		return fmt.Errorf("device token renewal: token renewer is not configured")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	resp, err := m.tokenRenewer.RenewDeviceToken(ctx)
	if err != nil {
		return fmt.Errorf("device token renewal request: %w", err)
	}
	if resp == nil || resp.DeviceToken == "" {
		return fmt.Errorf("device token renewal: hub returned an empty token")
	}

	if err := writeFileAtomic(filepath.Join(m.certDir, "device-token"), []byte(resp.DeviceToken), 0600); err != nil {
		return fmt.Errorf("write rotated device token: %w", err)
	}
	if resp.DeviceTokenExpiresAt != "" {
		if err := writeFileAtomic(filepath.Join(m.certDir, "device-token-expires"), []byte(resp.DeviceTokenExpiresAt), 0600); err != nil {
			return fmt.Errorf("write rotated device token expiry: %w", err)
		}
	}

	slog.Info("device token rotated", "expiresAt", resp.DeviceTokenExpiresAt)
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
