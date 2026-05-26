package enrollment

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/sync/hub"
)

func TestCertPaths_FormsPathsUnderCertDir(t *testing.T) {
	mgr := NewManager("/foo/bar")
	cert, key := mgr.CertPaths()
	if cert != "/foo/bar/device.pem" {
		t.Errorf("certFile: %q", cert)
	}
	if key != "/foo/bar/device-key.pem" {
		t.Errorf("keyFile: %q", key)
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
	dir := t.TempDir()
	mgr := NewManager(dir)

	resp := &HubEnrollResponse{
		ID:                 "thing-e60-1",
		CertPEM:            "-----BEGIN CERTIFICATE-----\nABC\n-----END CERTIFICATE-----\n",
		CaCertPEM:          "-----BEGIN CERTIFICATE-----\nCA\n-----END CERTIFICATE-----\n",
		DeviceToken:        "dt-1",
		AttestationCertPem: "-----BEGIN CERTIFICATE-----\nATTEST\n-----END CERTIFICATE-----\n",
	}
	keyPEM := []byte("-----BEGIN EC PRIVATE KEY-----\nMTLS\n-----END EC PRIVATE KEY-----\n")
	attestKeyPEM := []byte("-----BEGIN PRIVATE KEY-----\nED25519\n-----END PRIVATE KEY-----\n")

	if err := mgr.PersistEnrollment(resp, keyPEM, attestKeyPEM); err != nil {
		t.Fatalf("PersistEnrollment: %v", err)
	}
	for _, f := range []string{"attestation.pem", "attestation-key.pem"} {
		data, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			t.Errorf("missing %s: %v", f, err)
			continue
		}
		if len(data) == 0 {
			t.Errorf("%s empty", f)
		}
	}
}

func TestPersistHubEnrollment_SkipsAttestationArtifactsWhenAbsent(t *testing.T) {
	// Older Hub without attestation: no AttestationCertPem in response,
	// no attestKeyPEM from caller — must NOT create the attestation
	// files on disk so the signer's "file absent ↔ feature not
	// available" contract holds.
	dir := t.TempDir()
	mgr := NewManager(dir)

	resp := &HubEnrollResponse{
		ID:        "thing-legacy",
		CertPEM:   "-----BEGIN CERTIFICATE-----\nLEG\n-----END CERTIFICATE-----\n",
		CaCertPEM: "-----BEGIN CERTIFICATE-----\nCA\n-----END CERTIFICATE-----\n",
	}
	if err := mgr.PersistEnrollment(resp, []byte("key"), nil); err != nil {
		t.Fatalf("PersistEnrollment: %v", err)
	}
	for _, f := range []string{"attestation.pem", "attestation-key.pem"} {
		if _, err := os.Stat(filepath.Join(dir, f)); !os.IsNotExist(err) {
			t.Errorf("%s should be absent, got stat err = %v", f, err)
		}
	}
}

func TestPersistEnrollment_PublicWrapperRoundTrips(t *testing.T) {
	// PersistEnrollment is the public counterpart of persistHubEnrollment
	// used by SSO flow. It must write all four artifacts (device.pem,
	// device-key.pem, gateway-ca.pem, device.token / device-id) the same
	// way the token-enrollment path does.
	dir := t.TempDir()
	mgr := NewManager(dir)

	resp := &HubEnrollResponse{
		ID:          "thing-sso-1",
		CertPEM:     "-----BEGIN CERTIFICATE-----\nABC\n-----END CERTIFICATE-----\n",
		CaCertPEM:   "-----BEGIN CERTIFICATE-----\nCA1\n-----END CERTIFICATE-----\n",
		DeviceToken: "dev-token-xyz",
		CertExpires: time.Now().Add(24 * time.Hour).Format(time.RFC3339),
		CertSerial:  "serial-sso",
		TrustLevel:  2,
	}
	keyPEM := []byte("-----BEGIN EC PRIVATE KEY-----\nKEY\n-----END EC PRIVATE KEY-----\n")

	if err := mgr.PersistEnrollment(resp, keyPEM, nil); err != nil {
		t.Fatalf("PersistEnrollment: %v", err)
	}

	for _, f := range []string{"device.pem", "device-key.pem", "gateway-ca.pem"} {
		data, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			t.Errorf("expected %s present: %v", f, err)
		}
		if len(data) == 0 {
			t.Errorf("expected %s non-empty", f)
		}
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
		if req.CsrPEM == "" {
			t.Error("missing csrPem in request")
		}

		_ = json.NewEncoder(w).Encode(HubEnrollResponse{
			ID:          "agent-abc123",
			DeviceToken: strings.Repeat("a", 64),
			CertPEM:     "-----BEGIN CERTIFICATE-----\nHUBCERT\n-----END CERTIFICATE-----",
			CaCertPEM:   "-----BEGIN CERTIFICATE-----\nHUBCA\n-----END CERTIFICATE-----",
			CertSerial:  "serial-1",
			CertExpires: time.Now().Add(365 * 24 * time.Hour).Format(time.RFC3339),
		})
	}))
	defer srv.Close()

	certDir := t.TempDir()
	hubClient, err := NewHubEnrollClient(srv.URL, "")
	if err != nil {
		t.Fatalf("NewHubEnrollClient: %v", err)
	}
	mgr := NewManager(certDir, WithHubEnroller(hubClient))

	err = mgr.Enroll(context.Background(), "tok-123", "myhost", "darwin", "15.0", "2.0.0")
	if err != nil {
		t.Fatalf("hub enrollment failed: %v", err)
	}

	expectedFiles := []string{"device.pem", "device-key.pem", "gateway-ca.pem", "device-id", "device-token", "thing-id"}
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
	mgr := NewManager(certDir, WithHubEnroller(hubClient))

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
				CertPEM:     "cert",
				CaCertPEM:   "ca",
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
	mgr := NewManager(certDir, WithHubEnroller(hubClient))

	_ = mgr.Enroll(context.Background(), "tok", "host", "darwin", "15", "2.0.0")
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
}

func TestMarkNeedsReenroll(t *testing.T) {
	mgr := NewManager(t.TempDir())
	mgr.MarkNeedsReenroll()
	if mgr.GetState() != StateNeedsReenroll {
		t.Errorf("expected StateNeedsReenroll, got %d", mgr.GetState())
	}
}

// stubRenewer is a CertRenewer test double.
type stubRenewer struct {
	resp *hub.RenewCertResponse
	err  error
}

func (s *stubRenewer) RenewCert(ctx context.Context, deviceID, csrPEM string) (*hub.RenewCertResponse, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.resp, nil
}

func TestRenew_NoRenewer(t *testing.T) {
	mgr := NewManager(t.TempDir())
	if err := mgr.Renew(context.Background()); err == nil {
		t.Fatal("expected error when renewer is nil")
	}
}

func TestRenew_NoDeviceID(t *testing.T) {
	mgr := NewManager(t.TempDir(), WithCertRenewer(&stubRenewer{err: errors.New("should not be called")}))
	if err := mgr.Renew(context.Background()); err == nil {
		t.Fatal("expected error when device id is empty")
	}
}

func TestRenew_PersistsNewCert(t *testing.T) {
	certDir := t.TempDir()
	// Seed an existing enrollment's device-id.
	if err := os.WriteFile(filepath.Join(certDir, "device-id"), []byte("dev-1"), 0600); err != nil {
		t.Fatalf("seed device-id: %v", err)
	}

	renewer := &stubRenewer{resp: &hub.RenewCertResponse{
		Certificate: "-----BEGIN CERTIFICATE-----\nRENEWED\n-----END CERTIFICATE-----",
		GatewayCA:   "-----BEGIN CERTIFICATE-----\nCA\n-----END CERTIFICATE-----",
		ExpiresAt:   time.Now().Add(365 * 24 * time.Hour).Format(time.RFC3339),
		Serial:      "serial-2",
	}}
	mgr := NewManager(certDir, WithCertRenewer(renewer))

	if err := mgr.Renew(context.Background()); err != nil {
		t.Fatalf("Renew: %v", err)
	}

	for _, f := range []string{"device.pem", "device-key.pem", "gateway-ca.pem"} {
		data, err := os.ReadFile(filepath.Join(certDir, f))
		if err != nil {
			t.Errorf("expected %s to exist: %v", f, err)
		}
		if len(data) == 0 {
			t.Errorf("expected %s non-empty", f)
		}
	}
}
