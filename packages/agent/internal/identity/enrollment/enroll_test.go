package enrollment

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"github.com/goccy/go-json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/identity/attestation"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/identity/keystore"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/tlsbump"
)

func TestCertPaths_FormsPathsUnderCertDir(t *testing.T) {
	dir := "/foo/bar"
	mgr := NewManager(dir)
	cert, key := mgr.CertPaths()
	// filepath.Join keeps the expectation separator-agnostic (\ on Windows).
	if cert != filepath.Join(dir, "device.pem") {
		t.Errorf("certFile: %q (want under %q)", cert, dir)
	}
	if key != filepath.Join(dir, "device-key.pem") {
		t.Errorf("keyFile: %q (want under %q)", key, dir)
	}
}

func TestTrustLevel_MissingFileReturnsZero(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(dir)
	if got := mgr.TrustLevel(); got != 0 {
		t.Errorf("missing file: got %d, want 0", got)
	}
}

func TestTrustLevel_ParsesValidNumber(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "trust-level"), []byte("2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	mgr := NewManager(dir)
	if got := mgr.TrustLevel(); got != 2 {
		t.Errorf("got %d, want 2", got)
	}
}

func TestTrustLevel_MalformedFileReturnsZero(t *testing.T) {
	// Defensive: a corrupted trust-level file must NOT crash the menu bar
	// nor surface as a fake-high trust value.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "trust-level"), []byte("not-a-number"), 0600); err != nil {
		t.Fatal(err)
	}
	mgr := NewManager(dir)
	if got := mgr.TrustLevel(); got != 0 {
		t.Errorf("malformed file: got %d, want 0", got)
	}
}

func TestSSOEmail_MissingFileReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(dir)
	if got := mgr.SSOEmail(); got != "" {
		t.Errorf("missing file: got %q, want empty", got)
	}
}

func TestSSOEmail_ReadsAndTrims(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sso-email"), []byte("alice@example.com\n"), 0600); err != nil {
		t.Fatal(err)
	}
	mgr := NewManager(dir)
	if got := mgr.SSOEmail(); got != "alice@example.com" {
		t.Errorf("got %q", got)
	}
}

func TestPersistSSOEmail_EmptyIsNoOp(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(dir)
	if err := mgr.PersistSSOEmail(""); err != nil {
		t.Errorf("empty input: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "sso-email")); !os.IsNotExist(err) {
		t.Errorf("empty input should not create file; stat err = %v", err)
	}
}

func TestPersistSSOEmail_WritesAtomically(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(dir)
	if err := mgr.PersistSSOEmail("bob@example.com"); err != nil {
		t.Fatalf("persist: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "sso-email"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "bob@example.com" {
		t.Errorf("written content: %q", string(data))
	}
}

func TestSSOEmail_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(dir)
	const want = "carol@example.com"
	if err := mgr.PersistSSOEmail(want); err != nil {
		t.Fatal(err)
	}
	if got := mgr.SSOEmail(); got != want {
		t.Errorf("roundtrip: got %q, want %q", got, want)
	}
}

func TestGenerateAttestationKeyMaterial_RoundTrip(t *testing.T) {
	// generateAttestationKeyMaterial must produce a parseable CSR with
	// an Ed25519 public key + a PEM-encoded private key the attestation
	// signer can later read back.
	csrPEM, keyPEM := generateAttestationKeyMaterial("test-host")
	if csrPEM == "" || len(keyPEM) == 0 {
		t.Fatal("expected non-empty CSR + key")
	}
	if !strings.Contains(csrPEM, "BEGIN CERTIFICATE REQUEST") {
		t.Errorf("CSR PEM missing header: %s", csrPEM)
	}
	if !strings.Contains(string(keyPEM), "BEGIN PRIVATE KEY") {
		t.Errorf("key PEM missing PKCS8 header: %s", string(keyPEM))
	}
}

func TestGenerateAttestationKeyMaterial_EntropyFailure_ReturnsEmpty(t *testing.T) {
	// Starve the package-level entropy reader to force the early-
	// return fail-open path. The function must return ("", nil) so
	// the surrounding Enroll path skips the AttestationCsrPem field
	// rather than abort the whole enrollment.
	orig := randReader
	randReader = &failingReader{}
	t.Cleanup(func() { randReader = orig })

	csrPEM, keyPEM := generateAttestationKeyMaterial("host")
	if csrPEM != "" || keyPEM != nil {
		t.Errorf("expected empty fail-open; got csr=%q key-len=%d", csrPEM, len(keyPEM))
	}
}

// failingReader fakes a starved entropy source for the seam tests
// above. Mirrors the io.Reader stub pattern in attestation/signer_test.go
// without creating a cross-package dep.
type failingReader struct{}

func (failingReader) Read(_ []byte) (int, error) {
	return 0, errors.New("entropy starved")
}

func TestPersistHubEnrollment_WritesAttestationArtifactsWhenPresent(t *testing.T) {
	// The attestation CERT (public) is written under certDir, but the
	// attestation PRIVATE KEY goes to the platform keystore,
	// NOT a plaintext PEM on disk. An in-memory store keeps the test off the
	// real Keychain.
	dir := t.TempDir()
	store := keystore.NewMemoryStore()
	mgr := NewManager(dir, WithKeyStore(store))

	resp := &HubEnrollResponse{
		ID:                 "thing-e60-1",
		DeviceToken:        "dt-1",
		AttestationCertPem: "-----BEGIN CERTIFICATE-----\nATTEST\n-----END CERTIFICATE-----\n",
	}
	keyPEM := []byte("-----BEGIN EC PRIVATE KEY-----\nMTLS\n-----END EC PRIVATE KEY-----\n")
	certPEM := []byte("-----BEGIN CERTIFICATE-----\nABC\n-----END CERTIFICATE-----\n")
	attestKeyPEM := []byte("-----BEGIN PRIVATE KEY-----\nED25519\n-----END PRIVATE KEY-----\n")

	if err := mgr.PersistEnrollment(resp, keyPEM, certPEM, attestKeyPEM); err != nil {
		t.Fatalf("PersistEnrollment: %v", err)
	}
	// The public attestation cert is on disk.
	if data, err := os.ReadFile(filepath.Join(dir, "attestation.pem")); err != nil || len(data) == 0 {
		t.Errorf("attestation.pem should be present+non-empty: data=%d err=%v", len(data), err)
	}
	// The private key MUST NOT be on disk.
	if _, err := os.Stat(filepath.Join(dir, "attestation-key.pem")); !os.IsNotExist(err) {
		t.Errorf("attestation-key.pem MUST NOT be on disk; stat err = %v", err)
	}
	// The private key MUST be in the keystore, byte-for-byte.
	got, err := store.Get(keystore.AttestationKeyName)
	if err != nil {
		t.Fatalf("keystore get: %v", err)
	}
	if !bytes.Equal(got, attestKeyPEM) {
		t.Errorf("keystore key = %q; want %q", got, attestKeyPEM)
	}
}

func TestPersistHubEnrollment_SkipsAttestationArtifactsWhenAbsent(t *testing.T) {
	// Older Hub without attestation: no AttestationCertPem in response,
	// no attestKeyPEM from caller — must NOT create the attestation cert on
	// disk nor a key in the keystore, so the signer's "absent ↔ feature not
	// available" contract holds.
	dir := t.TempDir()
	store := keystore.NewMemoryStore()
	mgr := NewManager(dir, WithKeyStore(store))

	resp := &HubEnrollResponse{
		ID: "thing-legacy",
	}
	if err := mgr.PersistEnrollment(resp, []byte("key"), []byte("-----BEGIN CERTIFICATE-----\nLEG\n-----END CERTIFICATE-----\n"), nil); err != nil {
		t.Fatalf("PersistEnrollment: %v", err)
	}
	for _, f := range []string{"attestation.pem", "attestation-key.pem"} {
		if _, err := os.Stat(filepath.Join(dir, f)); !os.IsNotExist(err) {
			t.Errorf("%s should be absent, got stat err = %v", f, err)
		}
	}
	if got, _ := store.Get(keystore.AttestationKeyName); got != nil {
		t.Errorf("keystore should hold no attestation key when none issued; got %d bytes", len(got))
	}
}

func TestPersistEnrollment_PublicWrapperRoundTrips(t *testing.T) {
	// PersistEnrollment is the public counterpart of persistHubEnrollment
	// used by SSO flow. It must write the device identity artifacts
	// (device.pem, device-key.pem, device-id / device-token) the same way
	// the token-enrollment path does. Hub no longer returns its CA in the
	// enroll response, so gateway-ca.pem is NOT an enrollment artifact.
	dir := t.TempDir()
	mgr := NewManager(dir)

	resp := &HubEnrollResponse{
		ID:          "thing-sso-1",
		DeviceToken: "dev-token-xyz",
		TrustLevel:  2,
	}
	keyPEM := []byte("-----BEGIN EC PRIVATE KEY-----\nKEY\n-----END EC PRIVATE KEY-----\n")
	certPEM := []byte("-----BEGIN CERTIFICATE-----\nABC\n-----END CERTIFICATE-----\n")

	if err := mgr.PersistEnrollment(resp, keyPEM, certPEM, nil); err != nil {
		t.Fatalf("PersistEnrollment: %v", err)
	}

	for _, f := range []string{"device.pem", "device-key.pem", "device-id"} {
		data, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			t.Errorf("expected %s present: %v", f, err)
		}
		if len(data) == 0 {
			t.Errorf("expected %s non-empty", f)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "gateway-ca.pem")); !os.IsNotExist(err) {
		t.Errorf("gateway-ca.pem should NOT be written by enrollment; stat err=%v", err)
	}
}

func TestEnroll_RequiresHubEnroller(t *testing.T) {
	mgr := NewManager(t.TempDir())
	err := mgr.Enroll(context.Background(), "tok", "host", "darwin", "14", "1.0.0")
	if err == nil {
		t.Fatal("expected error when no hub enroller is configured")
	}
}

func TestHubEnroll_PersistsAllArtifacts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/internal/things/enroll" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("X-Enrollment-Token") == "" {
			t.Error("missing X-Enrollment-Token header")
		}

		var req HubEnrollRequest
		_ = json.NewDecoder(r.Body).Decode(&req)

		_ = json.NewEncoder(w).Encode(HubEnrollResponse{
			ID:          "agent-abc123",
			DeviceToken: strings.Repeat("a", 64),
		})
	}))
	defer srv.Close()

	certDir := t.TempDir()
	hubClient, err := NewHubEnrollClient(srv.URL, "")
	if err != nil {
		t.Fatalf("NewHubEnrollClient: %v", err)
	}
	mgr := NewManager(certDir, WithHubEnroller(hubClient), WithKeyStore(keystore.NewMemoryStore()))

	err = mgr.Enroll(context.Background(), "tok-123", "myhost", "darwin", "15.0", "2.0.0")
	if err != nil {
		t.Fatalf("hub enrollment failed: %v", err)
	}

	expectedFiles := []string{"device.pem", "device-key.pem", "device-id", "device-token", "thing-id"}
	for _, f := range expectedFiles {
		data, err := os.ReadFile(filepath.Join(certDir, f))
		if err != nil {
			t.Errorf("expected %s to exist: %v", f, err)
			continue
		}
		if len(data) == 0 {
			t.Errorf("expected %s to be non-empty", f)
		}
	}

	if mgr.ThingID() != "agent-abc123" {
		t.Errorf("expected thing ID agent-abc123, got %s", mgr.ThingID())
	}
	if !mgr.IsEnrolled() {
		t.Error("should be enrolled after hub enrollment")
	}
}

func TestHubEnroll_InvalidToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid token"}`))
	}))
	defer srv.Close()

	certDir := t.TempDir()
	hubClient, err := NewHubEnrollClient(srv.URL, "")
	if err != nil {
		t.Fatalf("NewHubEnrollClient: %v", err)
	}
	mgr := NewManager(certDir, WithHubEnroller(hubClient), WithKeyStore(keystore.NewMemoryStore()))

	err = mgr.Enroll(context.Background(), "bad", "host", "darwin", "14", "1.0.0")
	if err == nil {
		t.Fatal("expected error when hub rejects")
	}
}

func TestHubUnenroll_DeregistersViaHub(t *testing.T) {
	var deregisterCalled atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/internal/things/enroll" {
			_ = json.NewEncoder(w).Encode(HubEnrollResponse{
				ID:          "agent-xyz",
				DeviceToken: strings.Repeat("b", 64),
			})
			return
		}
		if r.URL.Path == "/api/internal/things/deregister" {
			deregisterCalled.Store(true)
			var req HubDeregisterRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.ID != "agent-xyz" {
				t.Errorf("expected deregister ID agent-xyz, got %s", req.ID)
			}
			_ = json.NewEncoder(w).Encode(map[string]bool{"ack": true})
			return
		}
		t.Errorf("unexpected path: %s", r.URL.Path)
	}))
	defer srv.Close()

	certDir := t.TempDir()
	hubClient, err := NewHubEnrollClient(srv.URL, "")
	if err != nil {
		t.Fatalf("NewHubEnrollClient: %v", err)
	}
	store := keystore.NewMemoryStore()
	mgr := NewManager(certDir, WithHubEnroller(hubClient), WithKeyStore(store))

	_ = mgr.Enroll(context.Background(), "tok", "host", "darwin", "15", "2.0.0")
	// Enroll generated + stored the attestation key in the keystore.
	if got, _ := store.Get(keystore.AttestationKeyName); got == nil {
		t.Error("expected attestation key in keystore after enroll")
	}
	_ = mgr.Unenroll(context.Background())

	if !deregisterCalled.Load() {
		t.Error("expected hub deregister to be called")
	}
	if mgr.IsEnrolled() {
		t.Error("should not be enrolled after unenroll")
	}

	for _, f := range []string{"device.pem", "device-key.pem", "gateway-ca.pem", "device-id", "device-token", "thing-id"} {
		if _, err := os.Stat(filepath.Join(certDir, f)); err == nil {
			t.Errorf("expected %s to be deleted after unenroll", f)
		}
	}
	// Unenroll must delete the attestation key from the keystore
	// so a decommission leaves no usable signing key behind.
	if got, _ := store.Get(keystore.AttestationKeyName); got != nil {
		t.Errorf("attestation key must be deleted from keystore on unenroll; got %d bytes", len(got))
	}
}

func TestMarkNeedsReenroll(t *testing.T) {
	mgr := NewManager(t.TempDir())
	mgr.MarkNeedsReenroll()
	if mgr.GetState() != StateNeedsReenroll {
		t.Errorf("expected StateNeedsReenroll, got %d", mgr.GetState())
	}
}

// TestEnrollment_AttestationKeyRoundTripsThroughKeystore verifies that
// the enrollment manager persists the Ed25519 attestation
// private key to the platform keystore (never a plaintext PEM under
// certDir), and the attestation signer loads that SAME key back from the
// keystore and produces a header whose signature verifies against the
// key's public half. This pins the enroll→keystore→signer wiring and the
// "no signing key on disk" invariant in one test.
func TestEnrollment_AttestationKeyRoundTripsThroughKeystore(t *testing.T) {
	dir := t.TempDir()
	store := keystore.NewMemoryStore()
	mgr := NewManager(dir, WithKeyStore(store))

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	attestKeyPEM, err := attestation.MarshalEd25519PrivateKeyPEM(priv)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	resp := &HubEnrollResponse{
		ID:                 "thing-rt-1",
		DeviceToken:        "dt",
		AttestationCertPem: "-----BEGIN CERTIFICATE-----\nC\n-----END CERTIFICATE-----\n",
	}
	certPEM := []byte("-----BEGIN CERTIFICATE-----\nC\n-----END CERTIFICATE-----\n")
	if err := mgr.PersistEnrollment(resp, []byte("k"), certPEM, attestKeyPEM); err != nil {
		t.Fatalf("persist: %v", err)
	}

	// Invariant: no plaintext signing key on disk.
	if _, err := os.Stat(filepath.Join(dir, "attestation-key.pem")); !os.IsNotExist(err) {
		t.Fatalf("attestation-key.pem must not exist on disk (SEC-M4-02); err=%v", err)
	}

	// The signer loads the SAME key from the keystore and signs verifiably.
	signer := attestation.NewSigner(store, keystore.AttestationKeyName, "thing-rt-1",
		func() bool { return true }, nil)
	hdr, err := signer.Sign()
	if err != nil {
		t.Fatalf("Sign from keystore key: %v", err)
	}
	parsed, err := tlsbump.ParseAttestationHeader(hdr)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parsed.Signature)
	if err != nil {
		t.Fatalf("sig decode: %v", err)
	}
	if !ed25519.Verify(pub, parsed.SignatureInput(), sig) {
		t.Fatal("signature from keystore-loaded key failed to verify — enroll↔signer keystore round-trip broken")
	}
}
