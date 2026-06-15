package enrollment

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

// writeFileAtomic — failure paths

// writeFileAtomic must surface CreateTemp's error when the target directory
// does not exist. Critical because the function is used by every artifact
// write in the enrollment flow; a silent swallow would leave a partial
// enrollment on disk with no error returned to the caller.
func TestWriteFileAtomic_CreateTempFailsOnMissingDir(t *testing.T) {
	missingDir := filepath.Join(t.TempDir(), "does-not-exist")
	err := writeFileAtomic(filepath.Join(missingDir, "foo"), []byte("x"), 0600)
	if err == nil {
		t.Fatal("expected error when target directory does not exist")
	}
}

// writeFileAtomic must overwrite an existing destination file (rename-replace
// semantics) — used by Renew on a re-enrolled device so the previous cert
// gets superseded.
func TestWriteFileAtomic_OverwritesExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target")
	if err := os.WriteFile(path, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(path, []byte("new"), 0600); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new" {
		t.Errorf("contents: got %q, want %q", string(got), "new")
	}
}

// writeFileAtomic must surface os.Rename's error and also clean up the
// temp file (cleanup=true defer path). We trigger Rename failure by
// placing a non-empty DIRECTORY at the destination path — rename of a
// regular file over a non-empty directory fails on POSIX.
func TestWriteFileAtomic_RenameFailureCleansUpTemp(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("rename-over-dir behaves differently on Windows")
	}
	dir := t.TempDir()
	dest := filepath.Join(dir, "target")
	if err := os.MkdirAll(dest, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, "blocker"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}

	err := writeFileAtomic(dest, []byte("payload"), 0600)
	if err == nil {
		t.Fatal("expected rename failure")
	}
	// No leftover *.tmp-* sibling — cleanup=true defer must have removed it.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "target.tmp-") {
			t.Errorf("temp file %q leaked despite rename failure", e.Name())
		}
	}
}

// writeFileAtomic must apply the requested file mode atomically — token
// material is written 0600 so other local users can't lift the bearer
// token.
func TestWriteFileAtomic_AppliesRequestedPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix-only permission bits")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "creds")
	if err := writeFileAtomic(path, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("perm: got %#o, want %#o", perm, 0600)
	}
}

// NewHubEnrollClient — CA-pinning branches

// NewHubEnrollClient must fail closed when the operator points at a CA
// file that does not exist. Per the contract: "an agent with no device
// cert yet relies on this bootstrap CA to prevent MITM of the
// X-Enrollment-Token, so read/parse failures of an explicitly
// configured CA are fatal".
func TestNewHubEnrollClient_MissingCAFileFailsClosed(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-ca.pem")
	_, err := NewHubEnrollClient("https://hub.example.test", missing)
	if err == nil {
		t.Fatal("expected error when CA file missing")
	}
	if !strings.Contains(err.Error(), "read CA file") {
		t.Errorf("error should mention read CA file: %v", err)
	}
}

// NewHubEnrollClient must reject a CA file whose contents contain zero
// valid PEM certificates. Same fail-closed rationale: silently
// downgrading to system trust would defeat the pin.
func TestNewHubEnrollClient_InvalidCAPEMFailsClosed(t *testing.T) {
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caPath, []byte("not a pem at all"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := NewHubEnrollClient("https://hub.example.test", caPath)
	if err == nil {
		t.Fatal("expected error for unparseable CA PEM")
	}
	if !strings.Contains(err.Error(), "parse CA PEM") {
		t.Errorf("error should mention parse CA PEM: %v", err)
	}
}

// NewHubEnrollClient must successfully install a valid CA so the
// "happy CA-pin" branch is observable (RootCAs assigned). Uses the
// CA cert that httptest.NewTLSServer mints for itself so the PEM
// path actually parses.
func TestNewHubEnrollClient_ValidCAInstalledOnTransport(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(HubEnrollResponse{ID: "ok"})
	}))
	defer srv.Close()

	// Persist srv's certificate so NewHubEnrollClient has a real PEM to load.
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.pem")
	pemBytes := certPEM(t, srv)
	if err := os.WriteFile(caPath, pemBytes, 0600); err != nil {
		t.Fatal(err)
	}

	client, err := NewHubEnrollClient(srv.URL, caPath)
	if err != nil {
		t.Fatalf("NewHubEnrollClient: %v", err)
	}
	if client == nil || client.HTTPClient == nil {
		t.Fatal("expected non-nil client + HTTPClient")
	}
	if client.BaseURL != srv.URL {
		t.Errorf("BaseURL: got %q, want %q", client.BaseURL, srv.URL)
	}

	// Round-trip a request — proves the pinned CA pool actually verifies
	// the server cert (would fail with "x509: certificate signed by unknown
	// authority" if the CA install no-op'd).
	resp, err := client.Enroll(context.Background(), "tok", HubEnrollRequest{Version: "1"})
	if err != nil {
		t.Fatalf("Enroll over pinned-CA TLS: %v", err)
	}
	if resp.ID != "ok" {
		t.Errorf("ID: got %q, want %q", resp.ID, "ok")
	}
}

// certPEM extracts the test server's certificate as PEM bytes.
func certPEM(t *testing.T, srv *httptest.Server) []byte {
	t.Helper()
	cert := srv.Certificate()
	if cert == nil {
		t.Fatal("test server has no certificate")
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
}

// EnrollWithJWT — entire function path

// EnrollWithJWT must POST to the same enroll path with an
// "Authorization: Bearer <jwt>" header, NOT X-Enrollment-Token.
// Confirms enterprise-login mode shares the same TLS-pinned client
// without duplicating wire logic.
func TestEnrollWithJWT_SendsBearerAuthorization(t *testing.T) {
	var capturedAuth string
	var capturedXTok string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		capturedXTok = r.Header.Get("X-Enrollment-Token")
		_ = json.NewEncoder(w).Encode(HubEnrollResponse{ID: "sso-id", DeviceToken: "tok"})
	}))
	defer srv.Close()

	client, err := NewHubEnrollClient(srv.URL, "")
	if err != nil {
		t.Fatalf("NewHubEnrollClient: %v", err)
	}

	resp, err := client.EnrollWithJWT(context.Background(), "my.jwt.payload", HubEnrollRequest{Version: "1"})
	if err != nil {
		t.Fatalf("EnrollWithJWT: %v", err)
	}
	if resp.ID != "sso-id" {
		t.Errorf("ID: %q", resp.ID)
	}
	if capturedAuth != "Bearer my.jwt.payload" {
		t.Errorf("Authorization header: got %q", capturedAuth)
	}
	if capturedXTok != "" {
		t.Errorf("X-Enrollment-Token must not be set in JWT mode, got %q", capturedXTok)
	}
}

// EnrollWithJWT must propagate non-200 errors the same way Enroll does
// (uses the same doEnroll helper).
func TestEnrollWithJWT_NonOKSurfacedAsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"sso disabled"}`))
	}))
	defer srv.Close()

	client, err := NewHubEnrollClient(srv.URL, "")
	if err != nil {
		t.Fatalf("NewHubEnrollClient: %v", err)
	}
	_, err = client.EnrollWithJWT(context.Background(), "jwt", HubEnrollRequest{})
	if err == nil {
		t.Fatal("expected error on 403")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error should carry status: %v", err)
	}
}

// doEnroll — additional error paths

// doEnroll must wrap a non-200, non-401 status with the response body so
// the operator can diagnose Hub-side validation failures (400/422/500).
func TestDoEnroll_NonOKNon401StatusWrapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"bad csr"}`))
	}))
	defer srv.Close()

	client, err := NewHubEnrollClient(srv.URL, "")
	if err != nil {
		t.Fatalf("NewHubEnrollClient: %v", err)
	}
	_, err = client.Enroll(context.Background(), "tok", HubEnrollRequest{})
	if err == nil {
		t.Fatal("expected error on 400")
	}
	if !strings.Contains(err.Error(), "hub enrollment failed (400)") {
		t.Errorf("error should mention status: %v", err)
	}
	if !strings.Contains(err.Error(), "bad csr") {
		t.Errorf("error should carry response body: %v", err)
	}
}

// doEnroll must surface a JSON decode error when Hub returns a 200 with
// a body that isn't a valid HubEnrollResponse. Without this branch the
// caller would get a zero-valued response and silently persist empty
// artifacts.
func TestDoEnroll_MalformedJSONReturnsDecodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not json at all"))
	}))
	defer srv.Close()

	client, err := NewHubEnrollClient(srv.URL, "")
	if err != nil {
		t.Fatalf("NewHubEnrollClient: %v", err)
	}
	_, err = client.Enroll(context.Background(), "tok", HubEnrollRequest{})
	if err == nil {
		t.Fatal("expected decode error")
	}
	if !strings.Contains(err.Error(), "decode hub enroll response") {
		t.Errorf("error should be a decode error: %v", err)
	}
}

// doEnroll must surface a transport-level error when Hub is unreachable
// — verifies the http.Client.Do error branch.
func TestDoEnroll_TransportErrorWrapped(t *testing.T) {
	// Pick a port that is guaranteed closed: bind a listener then close it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	client, err := NewHubEnrollClient("http://"+addr, "")
	if err != nil {
		t.Fatalf("NewHubEnrollClient: %v", err)
	}
	_, err = client.Enroll(context.Background(), "tok", HubEnrollRequest{})
	if err == nil {
		t.Fatal("expected transport error against closed port")
	}
	if !strings.Contains(err.Error(), "hub enroll request") {
		t.Errorf("error should be wrapped as hub enroll request: %v", err)
	}
}

// doEnroll must propagate ctx cancellation — important for shutdown:
// the daemon must not block on an enroll call when the agent is being
// stopped.
func TestDoEnroll_ContextCanceledShortCircuits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Block until client cancels.
		<-r.Context().Done()
	}))
	defer srv.Close()

	client, err := NewHubEnrollClient(srv.URL, "")
	if err != nil {
		t.Fatalf("NewHubEnrollClient: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already canceled

	_, err = client.Enroll(ctx, "tok", HubEnrollRequest{})
	if err == nil {
		t.Fatal("expected error from canceled context")
	}
	if !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "context canceled") {
		t.Errorf("error should mention context cancellation: %v", err)
	}
}

// Deregister — error paths

// Deregister must return a wrapped error for any non-200 status — the
// caller logs but does not fail enrollment on this, but it MUST get a
// real error to log.
func TestDeregister_NonOKStatusReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"db down"}`))
	}))
	defer srv.Close()

	client, err := NewHubEnrollClient(srv.URL, "")
	if err != nil {
		t.Fatalf("NewHubEnrollClient: %v", err)
	}
	err = client.Deregister(context.Background(), "dev-tok", "thing-1", "test")
	if err == nil {
		t.Fatal("expected error on 500")
	}
	if !strings.Contains(err.Error(), "hub deregister failed (500)") {
		t.Errorf("error should carry status: %v", err)
	}
	if !strings.Contains(err.Error(), "db down") {
		t.Errorf("error should carry response body: %v", err)
	}
}

// Deregister must wrap a transport error.
func TestDeregister_TransportErrorWrapped(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	client, err := NewHubEnrollClient("http://"+addr, "")
	if err != nil {
		t.Fatalf("NewHubEnrollClient: %v", err)
	}
	err = client.Deregister(context.Background(), "dev-tok", "thing-1", "test")
	if err == nil {
		t.Fatal("expected transport error")
	}
	if !strings.Contains(err.Error(), "hub deregister request") {
		t.Errorf("error should be wrapped: %v", err)
	}
}

// Deregister sends Authorization: Bearer <deviceToken> and a JSON body
// with the thing id + reason. Cross-checked because the wire shape is
// what Hub's audit logger keys off.
func TestDeregister_WireShapeBearerAndBody(t *testing.T) {
	type capture struct {
		auth string
		req  HubDeregisterRequest
	}
	var got capture
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.auth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&got.req)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client, err := NewHubEnrollClient(srv.URL, "")
	if err != nil {
		t.Fatalf("NewHubEnrollClient: %v", err)
	}
	if err := client.Deregister(context.Background(), "tok-XY", "thing-99", "user opt out"); err != nil {
		t.Fatalf("Deregister: %v", err)
	}
	if got.auth != "Bearer tok-XY" {
		t.Errorf("auth: %q", got.auth)
	}
	if got.req.ID != "thing-99" || got.req.Reason != "user opt out" {
		t.Errorf("body: %+v", got.req)
	}
}

// Enroll — failure paths

// Enroll must surface MkdirAll's error when certDir points at an
// existing FILE (not a directory). Without this we'd silently proceed
// to write artifacts that never land.
func TestEnroll_MkdirAllFailsOnFilePath(t *testing.T) {
	// Create a regular file and use it as certDir — MkdirAll will refuse
	// to convert it into a directory.
	parent := t.TempDir()
	filePath := filepath.Join(parent, "not-a-dir")
	if err := os.WriteFile(filePath, []byte("blocker"), 0600); err != nil {
		t.Fatal(err)
	}
	mgr := NewManager(filePath, WithHubEnroller(&stubHubEnroller{}))
	err := mgr.Enroll(context.Background(), "tok", "host", "darwin", "14", "1.0")
	if err == nil {
		t.Fatal("expected error when certDir is a file")
	}
	if !strings.Contains(err.Error(), "create cert dir") {
		t.Errorf("error should mention create cert dir: %v", err)
	}
}

// Enroll must propagate the wrapped Hub error so callers can
// distinguish "enrollment refused" from "local crypto failed".
func TestEnroll_HubErrorWrapped(t *testing.T) {
	mgr := NewManager(t.TempDir(), WithHubEnroller(&stubHubEnroller{err: errors.New("nope")}))
	err := mgr.Enroll(context.Background(), "tok", "host", "darwin", "14", "1.0")
	if err == nil {
		t.Fatal("expected hub error to surface")
	}
	if !strings.Contains(err.Error(), "enrollment failed") {
		t.Errorf("error should be wrapped: %v", err)
	}
}

// Enroll persists trust-level when Hub returns a non-zero value (covers
// the f.content != "" branch for the trust-level entry).
func TestEnroll_PersistsTrustLevelWhenReturned(t *testing.T) {
	stub := &stubHubEnroller{resp: &HubEnrollResponse{
		ID:          "t-99",
		DeviceToken: strings.Repeat("a", 32),
		TrustLevel:  3,
	}}
	dir := t.TempDir()
	mgr := NewManager(dir, WithHubEnroller(stub))

	if err := mgr.Enroll(context.Background(), "tok", "host", "darwin", "15", "2.0"); err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "trust-level"))
	if err != nil {
		t.Fatalf("trust-level file missing: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != "3" {
		t.Errorf("trust-level: got %q, want %q", got, "3")
	}
	if mgr.TrustLevel() != 3 {
		t.Errorf("TrustLevel(): %d", mgr.TrustLevel())
	}
}

// Enrollment writes the locally-generated device identity (device.pem +
// device-key.pem) and never a gateway-ca.pem — the Hub CA is an operator
// pin dropped into StateDir, not an enrollment artifact. persistHubEnrollment
// also skips empty-content entries (e.g. absent attestation), so trust-level=0
// still stamps "0" while attestation files stay absent.
func TestEnroll_WritesLocalIdentityNoGatewayCA(t *testing.T) {
	stub := &stubHubEnroller{resp: &HubEnrollResponse{
		ID:          "t-1",
		DeviceToken: "tok",
		TrustLevel:  0,
	}}
	dir := t.TempDir()
	mgr := NewManager(dir, WithHubEnroller(stub))

	if err := mgr.Enroll(context.Background(), "tok", "host", "darwin", "15", "2.0"); err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	// Locally self-signed device identity is persisted.
	if _, err := os.Stat(filepath.Join(dir, "device.pem")); err != nil {
		t.Errorf("device.pem should exist after enrollment: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "device-key.pem")); err != nil {
		t.Errorf("device-key.pem should exist after enrollment: %v", err)
	}
	// Hub no longer returns its CA in the enroll response, so no gateway-ca.pem.
	if _, err := os.Stat(filepath.Join(dir, "gateway-ca.pem")); !os.IsNotExist(err) {
		t.Errorf("gateway-ca.pem should NOT be written by enrollment; stat err=%v", err)
	}
	// trust-level=0 stamps "0" (non-empty string), so the file IS created.
	if _, err := os.Stat(filepath.Join(dir, "trust-level")); err != nil {
		t.Errorf("trust-level (=\"0\") should exist: %v", err)
	}
}

// Unenroll — branches

// Unenroll deletes local artifacts even when no hub enroller is wired
// (offline cleanup path) — important for the "user uninstalled while
// Hub unreachable" case.
func TestUnenroll_NoHubEnrollerStillDeletesFiles(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"device.pem", "device-key.pem", "thing-id", "device-id"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	mgr := NewManager(dir) // no WithHubEnroller
	if err := mgr.Unenroll(context.Background()); err != nil {
		t.Fatalf("Unenroll: %v", err)
	}
	for _, f := range []string{"device.pem", "device-key.pem", "thing-id", "device-id"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			t.Errorf("%s should be deleted", f)
		}
	}
	if mgr.GetState() != StateNotEnrolled {
		t.Errorf("state should be StateNotEnrolled, got %d", mgr.GetState())
	}
}

// Unenroll proceeds even when Hub deregister fails — the local files
// must still be removed so the user can re-enroll fresh. Confirms the
// "log + continue" branch on Deregister failure.
func TestUnenroll_ContinuesWhenHubDeregisterFails(t *testing.T) {
	dir := t.TempDir()
	// Seed thing-id + device-token so Deregister is attempted.
	if err := os.WriteFile(filepath.Join(dir, "thing-id"), []byte("t-1"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "device-id"), []byte("t-1"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "device-token"), []byte("tok"), 0600); err != nil {
		t.Fatal(err)
	}

	var called atomic.Bool
	stub := &stubHubEnroller{deregisterErr: errors.New("hub down"), onDeregister: func() { called.Store(true) }}
	mgr := NewManager(dir, WithHubEnroller(stub))

	if err := mgr.Unenroll(context.Background()); err != nil {
		t.Fatalf("Unenroll: %v", err)
	}
	if !called.Load() {
		t.Error("Deregister should have been attempted")
	}
	if _, err := os.Stat(filepath.Join(dir, "device-token")); err == nil {
		t.Error("device-token should still be deleted after Hub error")
	}
}

// Unenroll skips Hub Deregister when no device-token is on disk (the
// post-sign-out state) — verifies the tokenPath read-error branch.
func TestUnenroll_SkipsHubWhenNoDeviceToken(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "thing-id"), []byte("t-1"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "device-id"), []byte("t-1"), 0600); err != nil {
		t.Fatal(err)
	}
	// Deliberately no device-token file.

	var called atomic.Bool
	stub := &stubHubEnroller{onDeregister: func() { called.Store(true) }}
	mgr := NewManager(dir, WithHubEnroller(stub))

	if err := mgr.Unenroll(context.Background()); err != nil {
		t.Fatalf("Unenroll: %v", err)
	}
	if called.Load() {
		t.Error("Deregister must NOT be called when device-token absent")
	}
}

// Unenroll skips Hub when thing-id is empty — first-boot device that
// crashed before writing the id file.
func TestUnenroll_SkipsHubWhenNoThingID(t *testing.T) {
	dir := t.TempDir()
	// No device-id / thing-id present.
	var called atomic.Bool
	stub := &stubHubEnroller{onDeregister: func() { called.Store(true) }}
	mgr := NewManager(dir, WithHubEnroller(stub))

	if err := mgr.Unenroll(context.Background()); err != nil {
		t.Fatalf("Unenroll: %v", err)
	}
	if called.Load() {
		t.Error("Deregister must NOT be called when thing id is empty")
	}
}

// persistHubEnrollment must surface a writeFileAtomic failure as a
// wrapped "write <file>: ..." error. We can't easily mid-flight fail
// after MkdirAll succeeds, but we can call PersistEnrollment directly
// against a read-only certDir to exercise the same code path used by
// the SSO ingress.
func TestPersistEnrollment_WriteFailureWrapped(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix-only permission semantics")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0700) })

	mgr := NewManager(dir)
	resp := &HubEnrollResponse{
		ID:          "t-rw",
		DeviceToken: "tok",
	}
	err := mgr.PersistEnrollment(resp, []byte("KEY"), []byte("cert"), nil)
	if err == nil {
		t.Fatal("expected write failure on read-only dir")
	}
	if !strings.Contains(err.Error(), "write ") {
		t.Errorf("error should wrap 'write <file>': %v", err)
	}
}

// doEnroll must wrap io.ReadAll failures. We trigger this via a
// hijacking handler that closes the underlying TCP connection after
// announcing a Content-Length larger than what it actually writes.
func TestDoEnroll_BodyReadFailureWrapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "no hijack", http.StatusInternalServerError)
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			return
		}
		// Announce a longer body than we send, then close — io.ReadAll
		// surfaces unexpected EOF.
		_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\nshort"))
		_ = conn.Close()
	}))
	defer srv.Close()

	client, err := NewHubEnrollClient(srv.URL, "")
	if err != nil {
		t.Fatalf("NewHubEnrollClient: %v", err)
	}
	_, err = client.Enroll(context.Background(), "tok", HubEnrollRequest{})
	if err == nil {
		t.Fatal("expected body-read or transport error")
	}
	// Either path is acceptable; both are wrapped — accept either.
	msg := err.Error()
	if !strings.Contains(msg, "read hub enroll response") && !strings.Contains(msg, "hub enroll request") {
		t.Errorf("error should be wrapped: %v", err)
	}
}

// IsEnrolled — negative cases

// IsEnrolled returns false when only the cert+key are present (the
// post-sign-out state). Critical to block the launchd respawn-loop bug
// noted in the IsEnrolled docstring.
func TestIsEnrolled_CertAndKeyButNoTokenReturnsFalse(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "device.pem"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "device-key.pem"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	// Deliberately omit device-token.
	mgr := NewManager(dir)
	if mgr.IsEnrolled() {
		t.Error("IsEnrolled should be false without device-token (post-sign-out state)")
	}
}

// Test helpers

type stubHubEnroller struct {
	resp          *HubEnrollResponse
	err           error
	deregisterErr error
	onDeregister  func()
}

func (s *stubHubEnroller) Enroll(_ context.Context, _ string, _ HubEnrollRequest) (*HubEnrollResponse, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.resp != nil {
		return s.resp, nil
	}
	return &HubEnrollResponse{ID: "stub-id", DeviceToken: "stub-tok"}, nil
}

func (s *stubHubEnroller) EnrollWithJWT(_ context.Context, _ string, _ HubEnrollRequest) (*HubEnrollResponse, error) {
	return s.Enroll(context.Background(), "", HubEnrollRequest{})
}

func (s *stubHubEnroller) Deregister(_ context.Context, _, _, _ string) error {
	if s.onDeregister != nil {
		s.onDeregister()
	}
	return s.deregisterErr
}

// writeFileAtomic seam-driven mid-write error arms
//
// These tests inject a FakeFile through the createTempFn seam so the
// Chmod/Write/Sync/Close error branches — unreachable when CreateTemp
// returns a real *os.File on a healthy disk — are still asserted. Pattern
// mirrors packages/agent/internal/identity/secretstore/fallback_seam_test.go.

// fakeFile is an osFile test double that lets each per-arm test choose
// which mid-write call returns an error.
type fakeFile struct {
	nameVal  string
	chmodErr error
	writeErr error
	syncErr  error
	closeErr error
}

func (f *fakeFile) Name() string                { return f.nameVal }
func (f *fakeFile) Chmod(_ os.FileMode) error   { return f.chmodErr }
func (f *fakeFile) Write(p []byte) (int, error) { return len(p), f.writeErr }
func (f *fakeFile) Sync() error                 { return f.syncErr }
func (f *fakeFile) Close() error                { return f.closeErr }

// installFakeFile replaces createTempFn so it returns ff. Returns a
// restore func suitable for `defer`.
func installFakeFile(t *testing.T, ff *fakeFile) func() {
	t.Helper()
	prev := createTempFn
	createTempFn = func(_, _ string) (osFile, error) {
		return ff, nil
	}
	return func() { createTempFn = prev }
}

// writeFileAtomic must surface Chmod's error verbatim and not proceed to
// write the unsecured contents. Critical because the file holds the device
// bearer token at 0600.
func TestWriteFileAtomic_ChmodErrorSurfacedViaSeam(t *testing.T) {
	dir := t.TempDir()
	tmpPath := filepath.Join(dir, "fake.tmp-1")
	defer installFakeFile(t, &fakeFile{nameVal: tmpPath, chmodErr: errors.New("chmod boom")})()

	err := writeFileAtomic(filepath.Join(dir, "target"), []byte("x"), 0600)
	if err == nil || !strings.Contains(err.Error(), "chmod boom") {
		t.Fatalf("expected chmod error, got %v", err)
	}
	// Real file at "target" must NOT have been created — chmod failure
	// aborts before the rename step.
	if _, statErr := os.Stat(filepath.Join(dir, "target")); !os.IsNotExist(statErr) {
		t.Errorf("target file should not exist after chmod failure; statErr=%v", statErr)
	}
}

// writeFileAtomic must surface Write's error so a disk-full mid-write
// never silently truncates the key/cert artifact.
func TestWriteFileAtomic_WriteErrorSurfacedViaSeam(t *testing.T) {
	dir := t.TempDir()
	tmpPath := filepath.Join(dir, "fake.tmp-2")
	defer installFakeFile(t, &fakeFile{nameVal: tmpPath, writeErr: errors.New("write boom")})()

	err := writeFileAtomic(filepath.Join(dir, "target"), []byte("x"), 0600)
	if err == nil || !strings.Contains(err.Error(), "write boom") {
		t.Fatalf("expected write error, got %v", err)
	}
}

// writeFileAtomic must surface Sync's error so an fsync failure (e.g.
// underlying block-device EIO) is not swallowed; otherwise the rename
// can publish an unflushed file that vanishes on power loss.
func TestWriteFileAtomic_SyncErrorSurfacedViaSeam(t *testing.T) {
	dir := t.TempDir()
	tmpPath := filepath.Join(dir, "fake.tmp-3")
	defer installFakeFile(t, &fakeFile{nameVal: tmpPath, syncErr: errors.New("sync boom")})()

	err := writeFileAtomic(filepath.Join(dir, "target"), []byte("x"), 0600)
	if err == nil || !strings.Contains(err.Error(), "sync boom") {
		t.Fatalf("expected sync error, got %v", err)
	}
}

// writeFileAtomic must surface Close's error so deferred-flush failures
// on some filesystems (NFS write-back, encrypted FUSE) abort the
// atomic-write before the rename step.
func TestWriteFileAtomic_CloseErrorSurfacedViaSeam(t *testing.T) {
	dir := t.TempDir()
	tmpPath := filepath.Join(dir, "fake.tmp-4")
	defer installFakeFile(t, &fakeFile{nameVal: tmpPath, closeErr: errors.New("close boom")})()

	err := writeFileAtomic(filepath.Join(dir, "target"), []byte("x"), 0600)
	if err == nil || !strings.Contains(err.Error(), "close boom") {
		t.Fatalf("expected close error, got %v", err)
	}
}

// Enroll / Renew crypto-rand failure arms via randReader seam
//
// On a healthy host crypto/rand.Reader never returns an error; the
// keypair-generation and CSR-creation failure branches in Enroll +
// Renew need a fault-injecting reader to be observable. Mirrors
// packages/agent/internal/network/tls/entropy_seam_test.go.

// failReader is an io.Reader that always returns a sentinel error.
type failReader struct{ err error }

func (f failReader) Read(_ []byte) (int, error) { return 0, f.err }

// byteBudget delegates reads to inner up to remaining bytes then fails
// with err. Used to step past ecdsa.GenerateKey (~32 bytes for the P256
// scalar) and force x509.CreateCertificateRequest's signing-randomness
// read to fail. Per-call counter approaches are unreliable because
// GenerateKey may retry the scalar a non-deterministic number of times;
// counting bytes is deterministic since the operations consume a fixed
// byte total.
type byteBudget struct {
	inner     io.Reader
	err       error
	remaining int
}

func (b *byteBudget) Read(p []byte) (int, error) {
	if b.remaining <= 0 {
		return 0, b.err
	}
	want := len(p)
	if want > b.remaining {
		want = b.remaining
	}
	n, err := b.inner.Read(p[:want])
	b.remaining -= n
	return n, err
}

// installRandReader replaces randReader with r. Returns a restore func.
func installRandReader(t *testing.T, r io.Reader) func() {
	t.Helper()
	prev := randReader
	randReader = r
	return func() { randReader = prev }
}

// Enroll must surface a wrapped "generate keypair" error when crypto/rand
// fails on the very first read (ecdsa.GenerateKey arm).
func TestEnroll_GenerateKeypairFailsOnRandStarvation(t *testing.T) {
	defer installRandReader(t, failReader{err: errors.New("entropy starved")})()

	mgr := NewManager(t.TempDir(), WithHubEnroller(&stubHubEnroller{}))
	err := mgr.Enroll(context.Background(), "tok", "host", "darwin", "14", "1.0")
	if err == nil || !strings.Contains(err.Error(), "generate keypair") {
		t.Fatalf("expected generate-keypair error, got %v", err)
	}
}

// Enroll must surface a wrapped "create device cert" error when crypto/rand
// succeeds for the keypair + serial but starves during cert signing.
func TestEnroll_CreateDeviceCertFailsOnRandStarvation(t *testing.T) {
	// 60-byte budget is enough for P256 GenerateKey (~33 bytes) plus the
	// 128-bit serial (~16 bytes) but always exhausts before the self-signed
	// cert's ECDSA signature completes.
	defer installRandReader(t, &byteBudget{
		inner:     rand.Reader,
		err:       errors.New("entropy starved mid-cert"),
		remaining: 60,
	})()

	mgr := NewManager(t.TempDir(), WithHubEnroller(&stubHubEnroller{}))
	err := mgr.Enroll(context.Background(), "tok", "host", "darwin", "14", "1.0")
	if err == nil || !strings.Contains(err.Error(), "create device cert") {
		t.Fatalf("expected create-device-cert error, got %v", err)
	}
}
