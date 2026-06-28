package updater

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/sync/hub"
)

// stubChecker implements UpdateChecker for tests.
type stubChecker struct {
	info       hub.UpdateInfo
	err        error
	httpClient *http.Client
}

func (s *stubChecker) CheckUpdate(ctx context.Context, currentVersion, osName string) (hub.UpdateInfo, error) {
	if s.err != nil {
		return hub.UpdateInfo{}, s.err
	}
	return s.info, nil
}

func (s *stubChecker) HTTPClient() *http.Client {
	if s.httpClient != nil {
		return s.httpClient
	}
	return http.DefaultClient
}

// signManifest creates the Ed25519 manifest signature over sha256(version+":"+sha256hex+":"+downloadURL).
func signManifest(t *testing.T, priv ed25519.PrivateKey, version, sha256hex, downloadURL string) string {
	t.Helper()
	canonical := version + ":" + sha256hex + ":" + downloadURL
	digest := sha256.Sum256([]byte(canonical))
	sig := ed25519.Sign(priv, digest[:])
	return base64.StdEncoding.EncodeToString(sig)
}

// signBinary creates the Ed25519 binary-content signature over sha256(binaryBytes).
func signBinary(t *testing.T, priv ed25519.PrivateKey, binaryContent []byte) (hashHex, sigB64 string) {
	t.Helper()
	h := sha256.Sum256(binaryContent)
	sig := ed25519.Sign(priv, h[:])
	return hex.EncodeToString(h[:]), base64.StdEncoding.EncodeToString(sig)
}

// makeSignedInfo builds a fully-signed UpdateInfo for the given binary content + version + URL.
func makeSignedInfo(t *testing.T, priv ed25519.PrivateKey, version string, binaryContent []byte, downloadURL string) hub.UpdateInfo {
	t.Helper()
	hashHex, binSigB64 := signBinary(t, priv, binaryContent)
	manifestSigB64 := signManifest(t, priv, version, hashHex, downloadURL)
	return hub.UpdateInfo{
		Available:       true,
		Version:         version,
		DownloadURL:     downloadURL,
		SHA256:          hashHex,
		Signature:       manifestSigB64,
		BinarySignature: binSigB64,
	}
}

func TestCheckAndUpdate_NoUpdate(t *testing.T) {
	client := &stubChecker{info: hub.UpdateInfo{Available: false}}
	u := NewUpdater(client, Config{Enabled: true, CheckInterval: time.Hour}, "1.0.0", "darwin", "/tmp/fake")

	updated, err := u.CheckAndUpdate(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if updated {
		t.Error("should not be updated when no update available")
	}
}

func TestCheckAndUpdate_Disabled(t *testing.T) {
	client := &stubChecker{err: errors.New("should not be called")}
	u := NewUpdater(client, Config{Enabled: false}, "1.0.0", "darwin", "/tmp/fake")

	updated, err := u.CheckAndUpdate(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if updated {
		t.Error("should not update when disabled")
	}
}

// TestCheckAndUpdate_Downgrade_Rejected verifies that an update whose version is
// less than the running version is rejected before any download is attempted.
func TestCheckAndUpdate_Downgrade_Rejected(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	binaryContent := []byte("old-version-binary")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Should not be reached — downgrade must be rejected before download.
		t.Error("download server must not be contacted for a downgrade attempt")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	info := makeSignedInfo(t, priv, "0.9.0", binaryContent, srv.URL+"/agent")
	client := &stubChecker{info: info}
	u := NewUpdater(client, Config{Enabled: true, CheckInterval: time.Hour, PublicKey: pub}, "1.0.0", "darwin", "/tmp/fake")

	_, err := u.CheckAndUpdate(context.Background())
	if err == nil {
		t.Fatal("expected error when update version < current version")
	}
	if !errContains(err, "does not supersede current") {
		t.Fatalf("error should mention version monotonicity; got: %v", err)
	}
}

// TestCheckAndUpdate_SameVersion_Rejected verifies that a same-version update is rejected.
func TestCheckAndUpdate_SameVersion_Rejected(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	binaryContent := []byte("same-version-binary")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("download server must not be contacted for a same-version attempt")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	info := makeSignedInfo(t, priv, "1.0.0", binaryContent, srv.URL+"/agent")
	client := &stubChecker{info: info}
	u := NewUpdater(client, Config{Enabled: true, CheckInterval: time.Hour, PublicKey: pub}, "1.0.0", "darwin", "/tmp/fake")

	_, err := u.CheckAndUpdate(context.Background())
	if err == nil {
		t.Fatal("expected error when update version == current version")
	}
	if !errContains(err, "does not supersede current") {
		t.Fatalf("error should mention version monotonicity; got: %v", err)
	}
}

// TestCheckAndUpdate_VersionFloor_Rejected verifies that the persisted floor
// blocks a version that supersedes the running version but not the floor.
func TestCheckAndUpdate_VersionFloor_Rejected(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	binaryContent := []byte("mid-version-binary")
	dir := t.TempDir()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("download server must not be contacted when floor blocks the update")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	// Floor is 1.5.0; current running version is 1.0.0; offered version is 1.2.0.
	// 1.2.0 > 1.0.0 (passes current check) but 1.2.0 < 1.5.0 (fails floor check).
	info := makeSignedInfo(t, priv, "1.2.0", binaryContent, srv.URL+"/agent")
	client := &stubChecker{info: info}
	u := NewUpdater(client, Config{
		Enabled:       true,
		CheckInterval: time.Hour,
		PublicKey:     pub,
		DataDir:       dir,
	}, "1.0.0", "darwin", filepath.Join(dir, "agent"))

	// Pre-write a floor of 1.5.0 as if a previous install happened.
	if err := u.writeVersionFloor("1.5.0"); err != nil {
		t.Fatalf("writeVersionFloor: %v", err)
	}

	_, err := u.CheckAndUpdate(context.Background())
	if err == nil {
		t.Fatal("expected error when offered version < version floor")
	}
	if !errContains(err, "does not supersede installed floor") {
		t.Fatalf("error should mention version floor; got: %v", err)
	}
}

// TestCheckAndUpdate_ManifestSignatureMismatch_Rejected verifies that a manifest
// with a tampered version (version string not matching the signed tuple) is rejected
// before downloading the binary.
func TestCheckAndUpdate_ManifestSignatureMismatch_Rejected(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	binaryContent := []byte("binary-payload")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("download server must not be contacted when manifest sig is bad")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	// Build a valid manifest for 2.0.0 then tamper the version to 3.0.0.
	// The manifest signature still covers "2.0.0:sha256:url" so the check fails.
	info := makeSignedInfo(t, priv, "2.0.0", binaryContent, srv.URL+"/agent")
	info.Version = "3.0.0" // tamper: version string no longer matches the signature

	client := &stubChecker{info: info}
	u := NewUpdater(client, Config{Enabled: true, CheckInterval: time.Hour, PublicKey: pub}, "1.0.0", "darwin", "/tmp/fake")

	_, err := u.CheckAndUpdate(context.Background())
	if err == nil {
		t.Fatal("expected manifest signature verification failure")
	}
	if !errContains(err, "manifest signature verification failed") {
		t.Fatalf("error should indicate manifest signature failure; got: %v", err)
	}
}

// TestCheckAndUpdate_ManifestSignatureWrongKey_Rejected verifies that a manifest
// signed with a different key is rejected.
func TestCheckAndUpdate_ManifestSignatureWrongKey_Rejected(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)
	_, otherPriv, _ := ed25519.GenerateKey(nil)
	binaryContent := []byte("binary-payload")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("download server must not be contacted when manifest sig is wrong key")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	// Sign the manifest with otherPriv but verify against pub → should fail.
	info := makeSignedInfo(t, otherPriv, "2.0.0", binaryContent, srv.URL+"/agent")
	client := &stubChecker{info: info}
	u := NewUpdater(client, Config{Enabled: true, CheckInterval: time.Hour, PublicKey: pub}, "1.0.0", "darwin", "/tmp/fake")

	_, err := u.CheckAndUpdate(context.Background())
	if err == nil {
		t.Fatal("expected manifest signature failure on wrong key")
	}
	if !errContains(err, "manifest signature verification failed") {
		t.Fatalf("error should indicate manifest signature failure; got: %v", err)
	}
}

// TestCheckAndUpdate_EmptyManifestSignature_Rejected verifies that a manifest with no
// Signature field is rejected before downloading.
func TestCheckAndUpdate_EmptyManifestSignature_Rejected(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("download server must not be contacted when manifest signature is absent")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	client := &stubChecker{info: hub.UpdateInfo{
		Available:   true,
		Version:     "2.0.0",
		DownloadURL: srv.URL + "/agent",
		SHA256:      "deadbeef",
		Signature:   "", // manifest sig absent
	}}
	u := NewUpdater(client, Config{Enabled: true, CheckInterval: time.Hour, PublicKey: pub}, "1.0.0", "darwin", "/tmp/fake")

	_, err := u.CheckAndUpdate(context.Background())
	if err == nil || !errContains(err, "Ed25519 signature") {
		t.Fatalf("must reject when manifest signature absent; got: %v", err)
	}
}

// TestCheckAndUpdate_EmptyBinarySignature_Rejected verifies that a binary with no
// BinarySignature field is rejected after SHA256 passes but before swap.
func TestCheckAndUpdate_EmptyBinarySignature_Rejected(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	body := []byte("binary-content")
	dir := t.TempDir()
	bin := filepath.Join(dir, "agent")
	_ = os.WriteFile(bin, []byte("old"), 0755)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	hashHex, _ := signBinary(t, priv, body)
	manifestSig := signManifest(t, priv, "2.0.0", hashHex, srv.URL+"/agent")
	client := &stubChecker{info: hub.UpdateInfo{
		Available:       true,
		Version:         "2.0.0",
		DownloadURL:     srv.URL + "/agent",
		SHA256:          hashHex,
		Signature:       manifestSig,
		BinarySignature: "", // binary sig absent
	}}
	u := NewUpdater(client, Config{Enabled: true, CheckInterval: time.Hour, PublicKey: pub}, "1.0.0", "darwin", bin)

	_, err := u.CheckAndUpdate(context.Background())
	if err == nil || !errContains(err, "binary Ed25519 signature") {
		t.Fatalf("must reject when binary signature absent; got: %v", err)
	}
	// Original binary preserved.
	content, _ := os.ReadFile(bin)
	if string(content) != "old" {
		t.Errorf("original binary should be preserved, got %q", string(content))
	}
}

func TestVerifySignature_Valid(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)

	dir := t.TempDir()
	binPath := filepath.Join(dir, "binary")
	content := []byte("hello world binary content")
	_ = os.WriteFile(binPath, content, 0755)

	h := sha256.Sum256(content)
	sig := ed25519.Sign(priv, h[:])
	sigB64 := base64.StdEncoding.EncodeToString(sig)

	client := &stubChecker{}
	u := NewUpdater(client, Config{Enabled: true, PublicKey: pub}, "1.0.0", "darwin", binPath)

	err := u.verifySignature(binPath, sigB64)
	if err != nil {
		t.Fatalf("valid signature should pass: %v", err)
	}
}

func TestVerifySignature_Invalid(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)

	dir := t.TempDir()
	binPath := filepath.Join(dir, "binary")
	_ = os.WriteFile(binPath, []byte("content"), 0755)

	fakeSig := base64.StdEncoding.EncodeToString(make([]byte, 64))

	client := &stubChecker{}
	u := NewUpdater(client, Config{Enabled: true, PublicKey: pub}, "1.0.0", "darwin", binPath)

	err := u.verifySignature(binPath, fakeSig)
	if err == nil {
		t.Fatal("invalid signature should fail")
	}
}

func TestFileSHA256(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test")
	content := []byte("test content for sha256")
	_ = os.WriteFile(path, content, 0644)

	got, err := fileSHA256(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := sha256.Sum256(content)
	expectedHex := hex.EncodeToString(expected[:])
	if got != expectedHex {
		t.Errorf("expected %s, got %s", expectedHex, got)
	}
}

func TestAtomicSwap(t *testing.T) {
	dir := t.TempDir()
	currentPath := filepath.Join(dir, "agent")
	newPath := filepath.Join(dir, "agent.tmp")

	_ = os.WriteFile(currentPath, []byte("old"), 0755)
	_ = os.WriteFile(newPath, []byte("new"), 0755)

	client := &stubChecker{}
	u := NewUpdater(client, Config{Enabled: true}, "1.0.0", "darwin", currentPath)

	err := u.atomicSwap(newPath)
	if err != nil {
		t.Fatalf("swap failed: %v", err)
	}

	content, _ := os.ReadFile(currentPath)
	if string(content) != "new" {
		t.Errorf("expected new content, got %s", string(content))
	}

	rollback, _ := os.ReadFile(currentPath + ".rollback")
	if string(rollback) != "old" {
		t.Errorf("expected old in rollback, got %s", string(rollback))
	}
}

func TestApplyUpdate_DispatchByOS(t *testing.T) {
	client := &stubChecker{}

	// Non-darwin → in-place binary swap.
	dir := t.TempDir()
	current := filepath.Join(dir, "agent")
	newer := filepath.Join(dir, "agent.tmp")
	_ = os.WriteFile(current, []byte("old"), 0755)
	_ = os.WriteFile(newer, []byte("new"), 0755)
	ul := NewUpdater(client, Config{Enabled: true}, "1.0.0", "linux", current)
	if err := ul.applyUpdate(newer); err != nil {
		t.Fatalf("applyUpdate(linux) should swap: %v", err)
	}
	if got, _ := os.ReadFile(current); string(got) != "new" {
		t.Errorf("expected swapped content 'new', got %q", got)
	}

	// darwin → pkg install. Fail-fast when the pkg artifact is missing.
	ud := NewUpdater(client, Config{Enabled: true}, "1.0.0", "darwin", current)
	if err := ud.applyUpdate(filepath.Join(dir, "missing.pkg")); err == nil {
		t.Fatal("applyUpdate(darwin) with a missing pkg should error")
	} else if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' error, got %v", err)
	}
}

func TestPkgInstallDarwin_LaunchesInstaller(t *testing.T) {
	// Override the installer command with a harmless always-succeeding process so
	// the happy path (Stat OK → launch detached → Release → return nil) runs
	// without ever invoking the real /usr/sbin/installer on the test host. The
	// stand-in must exist on the host running the test: `cmd /c exit 0` on
	// Windows, `/usr/bin/true` elsewhere.
	dir := t.TempDir()
	pkg := filepath.Join(dir, "update.pkg")
	_ = os.WriteFile(pkg, []byte("PKG"), 0644)

	var gotPkg string
	orig := installerCommand
	installerCommand = func(p string) *exec.Cmd {
		gotPkg = p
		if runtime.GOOS == "windows" {
			return exec.Command("cmd", "/c", "exit", "0")
		}
		return exec.Command("/usr/bin/true")
	}
	defer func() { installerCommand = orig }()

	u := NewUpdater(&stubChecker{}, Config{Enabled: true}, "1.0.0", "darwin", filepath.Join(dir, "agent"))
	if err := u.pkgInstallDarwin(pkg); err != nil {
		t.Fatalf("pkgInstallDarwin should launch the installer: %v", err)
	}
	if gotPkg != pkg {
		t.Errorf("installer should be invoked with %q, got %q", pkg, gotPkg)
	}
}

func TestDetectCrashLoop(t *testing.T) {
	dir := t.TempDir()
	binaryPath := filepath.Join(dir, "agent")
	statusFile := filepath.Join(dir, "status")
	rollbackPath := binaryPath + ".rollback"

	_ = os.WriteFile(binaryPath, []byte("new"), 0755)
	_ = os.WriteFile(rollbackPath, []byte("old"), 0755)

	_ = os.WriteFile(statusFile, []byte(time.Now().Add(-5*time.Second).Format(time.RFC3339)), 0600)

	if !DetectCrashLoop(binaryPath, statusFile, 30*time.Second) {
		t.Error("should detect crash loop")
	}

	content, _ := os.ReadFile(binaryPath)
	if string(content) != "old" {
		t.Errorf("expected rollback content, got %s", string(content))
	}
}

func TestDetectCrashLoop_NoLoop(t *testing.T) {
	dir := t.TempDir()
	statusFile := filepath.Join(dir, "status")

	_ = os.WriteFile(statusFile, []byte(time.Now().Add(-5*time.Minute).Format(time.RFC3339)), 0600)

	if DetectCrashLoop(filepath.Join(dir, "agent"), statusFile, 30*time.Second) {
		t.Error("should not detect crash loop for old start time")
	}
}

// TestCheckAndUpdate_FullFlow_WithDownloadAndSwap verifies the complete happy path:
// valid manifest signature (pre-download), version monotonicity, download, SHA256,
// binary signature, atomic swap, and version floor persistence.
func TestCheckAndUpdate_FullFlow_WithDownloadAndSwap(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	binaryContent := []byte("#!/bin/sh\necho new-version")

	// Download server
	dlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(binaryContent)
	}))
	defer dlSrv.Close()

	dir := t.TempDir()
	currentBin := filepath.Join(dir, "agent")
	_ = os.WriteFile(currentBin, []byte("old-binary"), 0755)

	info := makeSignedInfo(t, priv, "2.0.0", binaryContent, dlSrv.URL+"/agent")
	client := &stubChecker{info: info}
	// osName "linux" so this exercises the in-place binary-swap path it asserts
	// (rollback + content replacement). The macOS path installs a .pkg via
	// /usr/sbin/installer and must never run in a unit test — its dispatch +
	// error path are covered separately by TestApplyUpdate_DispatchByOS and
	// TestPkgInstallDarwin_LaunchesInstaller.
	u := NewUpdater(client, Config{
		Enabled:       true,
		CheckInterval: time.Hour,
		PublicKey:     pub,
		DataDir:       dir,
	}, "1.0.0", "linux", currentBin)

	updated, err := u.CheckAndUpdate(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !updated {
		t.Error("should be updated")
	}

	content, _ := os.ReadFile(currentBin)
	if string(content) != string(binaryContent) {
		t.Error("binary should be replaced with new content")
	}

	rollback, _ := os.ReadFile(currentBin + ".rollback")
	if string(rollback) != "old-binary" {
		t.Error("rollback should contain old binary")
	}

	// Verify the version floor was persisted.
	floor, err := u.readVersionFloor()
	if err != nil {
		t.Fatalf("readVersionFloor: %v", err)
	}
	if floor != "2.0.0" {
		t.Errorf("expected version floor 2.0.0, got %q", floor)
	}
}

func TestCheckAndUpdate_SHA256Mismatch(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	dlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("actual content"))
	}))
	defer dlSrv.Close()

	dir := t.TempDir()
	currentBin := filepath.Join(dir, "agent")
	_ = os.WriteFile(currentBin, []byte("old"), 0755)

	// Build a manifest that has a wrong SHA256 (all zeros).
	// We still need a valid manifest signature that covers this wrong hash.
	wrongHashHex := "0000000000000000000000000000000000000000000000000000000000000000"
	manifestSig := signManifest(t, priv, "2.0.0", wrongHashHex, dlSrv.URL+"/agent")
	// BinarySignature is irrelevant — we'll fail at SHA256 check before reaching it.
	client := &stubChecker{info: hub.UpdateInfo{
		Available:   true,
		Version:     "2.0.0",
		DownloadURL: dlSrv.URL + "/agent",
		SHA256:      wrongHashHex,
		Signature:   manifestSig,
		// BinarySignature deliberately absent to confirm we fail at SHA256 check first.
	}}
	u := NewUpdater(client, Config{Enabled: true, CheckInterval: time.Hour, PublicKey: pub}, "1.0.0", "darwin", currentBin)

	_, err := u.CheckAndUpdate(context.Background())
	if err == nil {
		t.Fatal("expected SHA-256 mismatch error")
	}
	if !errContains(err, "sha256 mismatch") {
		t.Fatalf("expected sha256 mismatch error, got: %v", err)
	}

	content, _ := os.ReadFile(currentBin)
	if string(content) != "old" {
		t.Error("original binary should be preserved on failure")
	}
}

func TestWriteStartStatus(t *testing.T) {
	dir := t.TempDir()
	statusFile := filepath.Join(dir, "status")

	err := WriteStartStatus(statusFile)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, _ := os.ReadFile(statusFile)
	_, err = time.Parse(time.RFC3339, string(data))
	if err != nil {
		t.Errorf("status file should contain RFC3339 time, got: %s", string(data))
	}
}

// TestSemverGT verifies the semver comparison helper covers all comparison branches.
func TestSemverGT(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"2.0.0", "1.9.9", true},
		{"1.1.0", "1.0.9", true},
		{"1.0.1", "1.0.0", true},
		{"1.0.0", "1.0.0", false},
		{"0.9.0", "1.0.0", false},
		{"1.0.0", "2.0.0", false},
		{"v2.0.0", "1.9.9", true}, // leading 'v' stripped
		{"v1.0.0", "v1.0.0", false},
		{"10.0.0", "9.9.9", true},
		{"1.10.0", "1.9.9", true},
		{"1.0.10", "1.0.9", true},
		{"2.0", "1.9.9", true},  // short form padded to .0
		{"1.0", "1.0.0", false}, // equal after padding
	}
	for _, tc := range cases {
		got := semverGT(tc.a, tc.b)
		if got != tc.want {
			t.Errorf("semverGT(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

// TestVersionFloor_ReadMissing verifies that a missing floor file returns "" without error.
func TestVersionFloor_ReadMissing(t *testing.T) {
	dir := t.TempDir()
	client := &stubChecker{}
	u := NewUpdater(client, Config{Enabled: true, DataDir: dir}, "1.0.0", "darwin", "/tmp/x")
	floor, err := u.readVersionFloor()
	if err != nil {
		t.Fatalf("expected nil error for missing floor file, got: %v", err)
	}
	if floor != "" {
		t.Errorf("expected empty floor for missing file, got %q", floor)
	}
}

// TestVersionFloor_WriteAndRead verifies round-trip persistence.
func TestVersionFloor_WriteAndRead(t *testing.T) {
	dir := t.TempDir()
	client := &stubChecker{}
	u := NewUpdater(client, Config{Enabled: true, DataDir: dir}, "1.0.0", "darwin", "/tmp/x")

	if err := u.writeVersionFloor("2.5.0"); err != nil {
		t.Fatalf("writeVersionFloor: %v", err)
	}
	floor, err := u.readVersionFloor()
	if err != nil {
		t.Fatalf("readVersionFloor: %v", err)
	}
	if floor != "2.5.0" {
		t.Errorf("expected floor 2.5.0, got %q", floor)
	}
}

// TestVersionFloor_NilDataDir verifies that an empty DataDir disables floor persistence silently.
func TestVersionFloor_NilDataDir(t *testing.T) {
	client := &stubChecker{}
	u := NewUpdater(client, Config{Enabled: true, DataDir: ""}, "1.0.0", "darwin", "/tmp/x")

	if err := u.writeVersionFloor("2.0.0"); err != nil {
		t.Fatalf("writeVersionFloor should be no-op when DataDir empty, got: %v", err)
	}
	floor, err := u.readVersionFloor()
	if err != nil {
		t.Fatalf("readVersionFloor should be no-op when DataDir empty, got: %v", err)
	}
	if floor != "" {
		t.Errorf("expected empty floor when DataDir empty, got %q", floor)
	}
}

// TestVerifyManifestSignature_Valid tests the standalone manifest verification helper.
func TestVerifyManifestSignature_Valid(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	info := hub.UpdateInfo{
		Version:     "2.0.0",
		SHA256:      "abc123",
		DownloadURL: "https://example.com/agent",
	}
	canonical := info.Version + ":" + info.SHA256 + ":" + info.DownloadURL
	digest := sha256.Sum256([]byte(canonical))
	sig := ed25519.Sign(priv, digest[:])
	info.Signature = base64.StdEncoding.EncodeToString(sig)

	if err := verifyManifestSignature(info, pub); err != nil {
		t.Fatalf("valid manifest signature should pass: %v", err)
	}
}

// TestVerifyManifestSignature_TamperedVersion tests that changing the version invalidates the sig.
func TestVerifyManifestSignature_TamperedVersion(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	info := hub.UpdateInfo{
		Version:     "2.0.0",
		SHA256:      "abc123",
		DownloadURL: "https://example.com/agent",
	}
	canonical := info.Version + ":" + info.SHA256 + ":" + info.DownloadURL
	digest := sha256.Sum256([]byte(canonical))
	sig := ed25519.Sign(priv, digest[:])
	info.Signature = base64.StdEncoding.EncodeToString(sig)

	// Tamper the version after signing.
	info.Version = "9.9.9"

	if err := verifyManifestSignature(info, pub); err == nil {
		t.Fatal("tampered version should cause manifest signature failure")
	}
}

// TestVerifyManifestSignature_TamperedSHA256 tests that changing the SHA256 invalidates the sig.
func TestVerifyManifestSignature_TamperedSHA256(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	info := hub.UpdateInfo{
		Version:     "2.0.0",
		SHA256:      "original-hash",
		DownloadURL: "https://example.com/agent",
	}
	canonical := info.Version + ":" + info.SHA256 + ":" + info.DownloadURL
	digest := sha256.Sum256([]byte(canonical))
	sig := ed25519.Sign(priv, digest[:])
	info.Signature = base64.StdEncoding.EncodeToString(sig)

	info.SHA256 = "tampered-hash"

	if err := verifyManifestSignature(info, pub); err == nil {
		t.Fatal("tampered SHA256 should cause manifest signature failure")
	}
}

// TestVerifyManifestSignature_TamperedURL tests that changing the URL invalidates the sig.
func TestVerifyManifestSignature_TamperedURL(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	info := hub.UpdateInfo{
		Version:     "2.0.0",
		SHA256:      "abc123",
		DownloadURL: "https://real.example.com/agent",
	}
	canonical := info.Version + ":" + info.SHA256 + ":" + info.DownloadURL
	digest := sha256.Sum256([]byte(canonical))
	sig := ed25519.Sign(priv, digest[:])
	info.Signature = base64.StdEncoding.EncodeToString(sig)

	info.DownloadURL = "https://attacker.example.com/malicious"

	if err := verifyManifestSignature(info, pub); err == nil {
		t.Fatal("tampered URL should cause manifest signature failure")
	}
}

// TestVerifyManifestSignature_BadBase64 tests the decode error branch.
func TestVerifyManifestSignature_BadBase64(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)
	info := hub.UpdateInfo{
		Version:     "2.0.0",
		SHA256:      "abc",
		DownloadURL: "https://example.com",
		Signature:   "!!!not-valid-base64!!!",
	}
	err := verifyManifestSignature(info, pub)
	if err == nil || !errContains(err, "decode manifest signature") {
		t.Fatalf("expected base64 decode error, got: %v", err)
	}
}

// TestSemverGT_NonNumericFallback verifies the lexicographic fallback branch for
// non-numeric version components (e.g. build metadata or pre-release labels).
func TestSemverGT_NonNumericFallback(t *testing.T) {
	// "beta" is non-numeric — triggers the a > b lexicographic fallback.
	if semverGT("2.beta.0", "1.alpha.0") != ("2.beta.0" > "1.alpha.0") {
		t.Error("non-numeric parts should fall back to lexicographic comparison")
	}
	// Ensure it returns false (not panic) when both sides are non-numeric and equal.
	if semverGT("2.beta.0", "2.beta.0") {
		t.Error("equal non-numeric versions must return false")
	}
}

// TestSemverGT_BPartsShort exercises the bParts padding loop (b has fewer than 3 parts).
func TestSemverGT_BPartsShort(t *testing.T) {
	// "1" → padded to "1.0.0"; "2" → padded to "2.0.0"; 1 < 2 → false.
	if semverGT("1", "2") {
		t.Error("semverGT(1, 2) should be false")
	}
	// "2" > "1.0.0" after padding "2" → "2.0.0".
	if !semverGT("2", "1.0.0") {
		t.Error("semverGT(2, 1.0.0) should be true")
	}
}

// TestCheckAndUpdate_BinarySignatureMismatch_Rejected verifies that a wrong binary sig
// (signed with a different key than the manifest) is caught after SHA256 passes.
func TestCheckAndUpdate_BinarySignatureMismatch_Rejected(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	_, otherPriv, _ := ed25519.GenerateKey(nil)
	body := []byte("binary-payload-for-binsig-test")
	dir := t.TempDir()
	bin := filepath.Join(dir, "agent")
	_ = os.WriteFile(bin, []byte("old"), 0755)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	// Manifest sig is valid (signed with priv), but binary sig is signed with otherPriv.
	hashHex, wrongBinSig := signBinary(t, otherPriv, body)
	manifestSig := signManifest(t, priv, "2.0.0", hashHex, srv.URL+"/agent")
	client := &stubChecker{info: hub.UpdateInfo{
		Available:       true,
		Version:         "2.0.0",
		DownloadURL:     srv.URL + "/agent",
		SHA256:          hashHex,
		Signature:       manifestSig,
		BinarySignature: wrongBinSig,
	}}
	u := NewUpdater(client, Config{Enabled: true, CheckInterval: time.Hour, PublicKey: pub}, "1.0.0", "darwin", bin)

	_, err := u.CheckAndUpdate(context.Background())
	if err == nil {
		t.Fatal("expected binary signature mismatch error")
	}
	if !errContains(err, "binary signature verification failed") {
		t.Fatalf("error should indicate binary signature failure; got: %v", err)
	}
	// Original binary preserved.
	content, _ := os.ReadFile(bin)
	if string(content) != "old" {
		t.Errorf("original binary should be preserved, got %q", string(content))
	}
	// .tmp cleaned up.
	if _, statErr := os.Stat(bin + ".tmp"); statErr == nil {
		t.Fatal(".tmp file should be removed on binary signature mismatch")
	}
}

// TestVersionFloor_WriteError verifies that writeVersionFloor surfaces an OS write error.
func TestVersionFloor_WriteError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("read-only-dir semantics differ on Windows")
	}
	dir := t.TempDir()
	roDir := filepath.Join(dir, "ro")
	if err := os.Mkdir(roDir, 0500); err != nil {
		t.Fatal(err)
	}

	client := &stubChecker{}
	u := NewUpdater(client, Config{Enabled: true, DataDir: roDir}, "1.0.0", "darwin", "/tmp/x")
	err := u.writeVersionFloor("2.0.0")
	if err == nil {
		t.Fatal("expected write error in read-only directory")
	}
}

// TestVersionFloor_ReadOSError verifies that a non-NotExist OS error from readVersionFloor
// is surfaced (tested via a directory masquerading as the floor file path).
func TestVersionFloor_ReadOSError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory-as-file read semantics differ on Windows")
	}
	dir := t.TempDir()
	// Create a subdirectory where the floor file would be — os.ReadFile on a dir gives an error.
	floorDir := filepath.Join(dir, "updater-floor.json")
	if err := os.Mkdir(floorDir, 0755); err != nil {
		t.Fatal(err)
	}
	client := &stubChecker{}
	u := NewUpdater(client, Config{Enabled: true, DataDir: dir}, "1.0.0", "darwin", "/tmp/x")
	_, err := u.readVersionFloor()
	if err == nil {
		t.Fatal("expected error when floor path is a directory, not a file")
	}
}

// TestVersionFloor_ReadUnmarshalError verifies that invalid JSON in the floor file returns an error.
func TestVersionFloor_ReadUnmarshalError(t *testing.T) {
	dir := t.TempDir()
	floorPath := filepath.Join(dir, "updater-floor.json")
	if err := os.WriteFile(floorPath, []byte("not-json!!!"), 0600); err != nil {
		t.Fatal(err)
	}
	client := &stubChecker{}
	u := NewUpdater(client, Config{Enabled: true, DataDir: dir}, "1.0.0", "darwin", "/tmp/x")
	_, err := u.readVersionFloor()
	if err == nil || !errContains(err, "parse version floor") {
		t.Fatalf("expected parse error for invalid floor JSON, got: %v", err)
	}
}

// TestCheckAndUpdate_FloorWriteError_DoesNotFail verifies that a floor write failure
// after a successful install logs a warning but does not cause CheckAndUpdate to return
// an error (floor persistence is defense-in-depth, not required for correctness).
func TestCheckAndUpdate_FloorWriteError_DoesNotFail(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("read-only-dir semantics differ on Windows")
	}
	pub, priv, _ := ed25519.GenerateKey(nil)
	body := []byte("v2-binary-content")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	dir := t.TempDir()
	bin := filepath.Join(dir, "agent")
	_ = os.WriteFile(bin, []byte("old"), 0755)

	// Make the dataDir read-only so writeVersionFloor fails.
	roDataDir := filepath.Join(dir, "ro-state")
	if err := os.Mkdir(roDataDir, 0500); err != nil {
		t.Fatal(err)
	}

	info := makeSignedInfo(t, priv, "2.0.0", body, srv.URL+"/agent")
	client := &stubChecker{info: info}
	// osName "linux" → in-place binary swap (this test asserts swapped content);
	// darwin would route to the .pkg installer, which must not run in a test.
	u := NewUpdater(client, Config{
		Enabled:       true,
		CheckInterval: time.Hour,
		PublicKey:     pub,
		DataDir:       roDataDir,
	}, "1.0.0", "linux", bin)

	// The update should still succeed even though floor persistence fails.
	updated, err := u.CheckAndUpdate(context.Background())
	if err != nil {
		t.Fatalf("floor write failure must not propagate as error: %v", err)
	}
	if !updated {
		t.Error("update should be applied even when floor write fails")
	}
	content, _ := os.ReadFile(bin)
	if string(content) != string(body) {
		t.Errorf("binary should be updated, got %q", string(content))
	}
}

func errContains(err error, sub string) bool {
	if err == nil {
		return false
	}
	return contains(err.Error(), sub)
}

func contains(haystack, needle string) bool {
	if needle == "" {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
