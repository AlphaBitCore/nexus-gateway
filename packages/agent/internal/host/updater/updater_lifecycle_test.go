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
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/sync/hub"
)

// recordingChecker is a richer UpdateChecker that records calls and supports
// per-call response sequencing (used to drive the periodic Run loop).
type recordingChecker struct {
	mu         sync.Mutex
	calls      int
	responses  []hub.UpdateInfo // played in order; sticks on last
	errs       []error          // played in order; sticks on last
	httpClient *http.Client
}

func (r *recordingChecker) CheckUpdate(_ context.Context, _, _ string) (hub.UpdateInfo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	idx := r.calls
	r.calls++
	var info hub.UpdateInfo
	if len(r.responses) > 0 {
		if idx >= len(r.responses) {
			idx = len(r.responses) - 1
		}
		info = r.responses[idx]
	}
	var err error
	if len(r.errs) > 0 {
		ei := r.calls - 1
		if ei >= len(r.errs) {
			ei = len(r.errs) - 1
		}
		err = r.errs[ei]
	}
	return info, err
}

func (r *recordingChecker) HTTPClient() *http.Client {
	if r.httpClient != nil {
		return r.httpClient
	}
	return http.DefaultClient
}

func (r *recordingChecker) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// signBinaryContent produces (sha256-hex, ed25519-binary-sig-base64) for the given content.
// The binary signature covers sha256(content) and is stored in BinarySignature.
func signBinaryContent(t *testing.T, priv ed25519.PrivateKey, content []byte) (string, string) {
	t.Helper()
	h := sha256.Sum256(content)
	sig := ed25519.Sign(priv, h[:])
	return hex.EncodeToString(h[:]), base64.StdEncoding.EncodeToString(sig)
}

// signManifestFields produces the Ed25519 manifest signature over
// sha256(version+":"+sha256hex+":"+downloadURL).
func signManifestFields(t *testing.T, priv ed25519.PrivateKey, version, sha256hex, downloadURL string) string {
	t.Helper()
	canonical := version + ":" + sha256hex + ":" + downloadURL
	digest := sha256.Sum256([]byte(canonical))
	sig := ed25519.Sign(priv, digest[:])
	return base64.StdEncoding.EncodeToString(sig)
}

// makeFullInfo builds a fully-signed UpdateInfo (both Signature and BinarySignature).
func makeFullInfo(t *testing.T, priv ed25519.PrivateKey, version string, body []byte, downloadURL string) hub.UpdateInfo {
	t.Helper()
	hashHex, binSig := signBinaryContent(t, priv, body)
	manifestSig := signManifestFields(t, priv, version, hashHex, downloadURL)
	return hub.UpdateInfo{
		Available:       true,
		Version:         version,
		DownloadURL:     downloadURL,
		SHA256:          hashHex,
		Signature:       manifestSig,
		BinarySignature: binSig,
	}
}

// NewUpdater — enabled-without-public-key downgrade to disabled

func TestNewUpdater_EnabledWithoutPublicKey_DisablesItself(t *testing.T) {
	client := &stubChecker{}
	u := NewUpdater(client, Config{Enabled: true, CheckInterval: time.Hour, PublicKey: nil}, "1.0.0", "darwin", "/tmp/x")
	if u.cfg.Enabled {
		t.Fatal("updater must be force-disabled when no public key is configured")
	}
	// And CheckAndUpdate must short-circuit without touching the client.
	failClient := &stubChecker{err: errors.New("must not be called")}
	u2 := NewUpdater(failClient, Config{Enabled: true, PublicKey: nil}, "1.0.0", "darwin", "/tmp/x")
	updated, err := u2.CheckAndUpdate(context.Background())
	if err != nil || updated {
		t.Fatalf("disabled updater must short-circuit; got updated=%v err=%v", updated, err)
	}
}

// CheckAndUpdate — every named error branch in turn

func TestCheckAndUpdate_CheckerError(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)
	client := &stubChecker{err: errors.New("hub down")}
	u := NewUpdater(client, Config{Enabled: true, CheckInterval: time.Hour, PublicKey: pub}, "1.0.0", "darwin", "/tmp/x")

	updated, err := u.CheckAndUpdate(context.Background())
	if err == nil {
		t.Fatal("expected error when checker fails")
	}
	if updated {
		t.Fatal("updated must be false on checker error")
	}
	if !errContains(err, "check update") {
		t.Fatalf("error should be wrapped with 'check update': %v", err)
	}
}

func TestCheckAndUpdate_DownloadFails(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	// Server that always 500s.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	dir := t.TempDir()
	bin := filepath.Join(dir, "agent")
	if err := os.WriteFile(bin, []byte("old"), 0755); err != nil {
		t.Fatal(err)
	}

	// Use a placeholder body hash for the manifest sig — the download will fail
	// before we get to binary verification, but the manifest sig must be valid
	// to pass the pre-download gate. Both version and URL must exactly match what
	// is in hub.UpdateInfo (same string values, including the version).
	const testVersion = "2.0.0"
	placeholderHash := "deadbeef00000000000000000000000000000000000000000000000000000000"
	manifestSig := signManifestFields(t, priv, testVersion, placeholderHash, srv.URL)
	client := &stubChecker{info: hub.UpdateInfo{
		Available:       true,
		Version:         testVersion,
		DownloadURL:     srv.URL,
		SHA256:          placeholderHash,
		Signature:       manifestSig,
		BinarySignature: base64.StdEncoding.EncodeToString(make([]byte, 64)),
	}}
	u := NewUpdater(client, Config{Enabled: true, PublicKey: pub}, "1.0.0", "darwin", bin)

	updated, err := u.CheckAndUpdate(context.Background())
	if err == nil || updated {
		t.Fatalf("download failure must surface; updated=%v err=%v", updated, err)
	}
	if !errContains(err, "download") {
		t.Fatalf("error should mention download: %v", err)
	}
	// tmp file must have been cleaned up.
	if _, statErr := os.Stat(bin + ".tmp"); statErr == nil {
		t.Fatal(".tmp file should be removed on download failure")
	}
	// Original binary intact.
	got, _ := os.ReadFile(bin)
	if string(got) != "old" {
		t.Fatalf("original binary must be untouched, got %q", string(got))
	}
}

func TestCheckAndUpdate_EmptySHA256_Rejected(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("body"))
	}))
	defer srv.Close()

	dir := t.TempDir()
	bin := filepath.Join(dir, "agent")
	_ = os.WriteFile(bin, []byte("old"), 0755)

	// SHA256 is empty; we sign the empty SHA256 into the manifest so the manifest
	// check passes, and the SHA256-empty rejection fires after download.
	// Both version and URL must exactly match the info fields.
	const testVersion = "2.0.0"
	dlURL := srv.URL + "/agent"
	manifestSig := signManifestFields(t, priv, testVersion, "", dlURL)
	client := &stubChecker{info: hub.UpdateInfo{
		Available:   true,
		Version:     testVersion,
		DownloadURL: dlURL,
		SHA256:      "", // missing — triggers rejection after download
		Signature:   manifestSig,
	}}
	u := NewUpdater(client, Config{Enabled: true, PublicKey: pub}, "1.0.0", "darwin", bin)

	_, err := u.CheckAndUpdate(context.Background())
	if err == nil || !errContains(err, "SHA256") {
		t.Fatalf("must reject when server omits SHA256: %v", err)
	}
	if _, statErr := os.Stat(bin + ".tmp"); statErr == nil {
		t.Fatal(".tmp file should be removed when SHA256 is missing")
	}
}

func TestCheckAndUpdate_EmptySignature_Rejected(t *testing.T) {
	// The manifest Signature field is empty — rejected before download starts.
	pub, _, _ := ed25519.GenerateKey(nil)

	dir := t.TempDir()
	bin := filepath.Join(dir, "agent")
	_ = os.WriteFile(bin, []byte("old"), 0755)

	client := &stubChecker{info: hub.UpdateInfo{
		Available:   true,
		Version:     "2.0",
		DownloadURL: "http://localhost/agent",
		SHA256:      "abc123",
		Signature:   "", // manifest sig absent
	}}
	u := NewUpdater(client, Config{Enabled: true, PublicKey: pub}, "1.0.0", "darwin", bin)

	_, err := u.CheckAndUpdate(context.Background())
	if err == nil || !errContains(err, "signature") {
		t.Fatalf("must reject when server omits manifest signature: %v", err)
	}
	// No download should have been attempted — .tmp must not exist.
	if _, statErr := os.Stat(bin + ".tmp"); statErr == nil {
		t.Fatal(".tmp file should not be created when manifest signature is missing")
	}
}

func TestCheckAndUpdate_BadSignatureBase64_Rejected(t *testing.T) {
	// Manifest Signature is invalid base64 — rejected before download starts.
	pub, _, _ := ed25519.GenerateKey(nil)

	dir := t.TempDir()
	bin := filepath.Join(dir, "agent")
	_ = os.WriteFile(bin, []byte("old"), 0755)

	client := &stubChecker{info: hub.UpdateInfo{
		Available:   true,
		Version:     "2.0",
		DownloadURL: "http://localhost/agent",
		SHA256:      "abc123",
		Signature:   "!!!not-valid-base64!!!",
	}}
	u := NewUpdater(client, Config{Enabled: true, PublicKey: pub}, "1.0.0", "darwin", bin)

	_, err := u.CheckAndUpdate(context.Background())
	if err == nil || !errContains(err, "manifest signature verification failed") {
		t.Fatalf("invalid base64 must surface via manifest-signature-verification wrapper: %v", err)
	}
	if _, statErr := os.Stat(bin + ".tmp"); statErr == nil {
		t.Fatal(".tmp file should not be created on manifest signature decode error")
	}
}

func TestCheckAndUpdate_SignatureMismatch_Rejected(t *testing.T) {
	// Manifest signature is signed with a different key — rejected before download.
	pub, _, _ := ed25519.GenerateKey(nil)
	_, otherPriv, _ := ed25519.GenerateKey(nil)

	dir := t.TempDir()
	bin := filepath.Join(dir, "agent")
	_ = os.WriteFile(bin, []byte("old"), 0755)

	// Sign manifest with otherPriv but verify against pub → must fail.
	hashHex := "abc123def456" + "0000000000000000000000000000000000000000000000000000"
	manifestSig := signManifestFields(t, otherPriv, "2.0.0", hashHex, "http://localhost/agent")
	client := &stubChecker{info: hub.UpdateInfo{
		Available:   true,
		Version:     "2.0.0",
		DownloadURL: "http://localhost/agent",
		SHA256:      hashHex,
		Signature:   manifestSig,
	}}
	u := NewUpdater(client, Config{Enabled: true, PublicKey: pub}, "1.0.0", "darwin", bin)

	_, err := u.CheckAndUpdate(context.Background())
	if err == nil || !errContains(err, "manifest signature verification failed") {
		t.Fatalf("wrong-key manifest signature must be rejected: %v", err)
	}
	got, _ := os.ReadFile(bin)
	if string(got) != "old" {
		t.Fatalf("original binary must be preserved on manifest signature mismatch, got %q", string(got))
	}
}

// download() — http.NewRequest error + HTTP transport error + file open error

func TestDownload_InvalidURL_ReqError(t *testing.T) {
	client := &stubChecker{}
	u := NewUpdater(client, Config{Enabled: true, PublicKey: ed25519.PublicKey(make([]byte, 32))}, "1.0.0", "darwin", "/tmp/x")
	dir := t.TempDir()
	dest := filepath.Join(dir, "out")
	// Control characters in URL → http.NewRequestWithContext fails before any I/O.
	err := u.download(context.Background(), "http://exa\x7fmple.com", dest)
	if err == nil {
		t.Fatal("expected NewRequest error on invalid URL")
	}
	if _, statErr := os.Stat(dest); statErr == nil {
		t.Fatal("dest file should not exist on req-construct error")
	}
}

func TestDownload_TransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	srv.Close() // close immediately so connection refuses

	client := &stubChecker{}
	u := NewUpdater(client, Config{Enabled: true, PublicKey: ed25519.PublicKey(make([]byte, 32))}, "1.0.0", "darwin", "/tmp/x")
	dir := t.TempDir()
	dest := filepath.Join(dir, "out")
	err := u.download(context.Background(), srv.URL, dest)
	if err == nil {
		t.Fatal("expected transport error against closed server")
	}
}

func TestDownload_FileOpenError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("body"))
	}))
	defer srv.Close()

	client := &stubChecker{}
	u := NewUpdater(client, Config{Enabled: true, PublicKey: ed25519.PublicKey(make([]byte, 32))}, "1.0.0", "darwin", "/tmp/x")

	// Point dest at a path whose parent dir does not exist.
	dir := t.TempDir()
	dest := filepath.Join(dir, "nope", "agent.tmp")
	err := u.download(context.Background(), srv.URL, dest)
	if err == nil {
		t.Fatal("expected OpenFile error for non-existent parent dir")
	}
}

func TestDownload_OK(t *testing.T) {
	body := []byte("payload-bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	client := &stubChecker{}
	u := NewUpdater(client, Config{Enabled: true, PublicKey: ed25519.PublicKey(make([]byte, 32))}, "1.0.0", "darwin", "/tmp/x")
	dir := t.TempDir()
	dest := filepath.Join(dir, "agent.tmp")
	if err := u.download(context.Background(), srv.URL, dest); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read dest: %v", err)
	}
	if string(got) != string(body) {
		t.Fatalf("dest content mismatch: got %q want %q", string(got), string(body))
	}
}

// verifySignature() — base64 decode error + fileSHA256 (open) error

func TestVerifySignature_Base64DecodeError(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)
	dir := t.TempDir()
	bin := filepath.Join(dir, "b")
	_ = os.WriteFile(bin, []byte("x"), 0644)

	client := &stubChecker{}
	u := NewUpdater(client, Config{Enabled: true, PublicKey: pub}, "1.0.0", "darwin", bin)
	err := u.verifySignature(bin, "!!!not-base64!!!")
	if err == nil || !errContains(err, "decode signature") {
		t.Fatalf("expected base64 decode error, got %v", err)
	}
}

func TestVerifySignature_OpenError(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)
	client := &stubChecker{}
	u := NewUpdater(client, Config{Enabled: true, PublicKey: pub}, "1.0.0", "darwin", "/tmp/x")
	sig := base64.StdEncoding.EncodeToString(make([]byte, 64))
	dir := t.TempDir()
	err := u.verifySignature(filepath.Join(dir, "missing"), sig)
	if err == nil {
		t.Fatal("expected error opening non-existent file")
	}
}

// atomicSwap() — backup-rename error path; rollback-on-install-failure path

func TestAtomicSwap_BackupRenameFails_OriginalMissing(t *testing.T) {
	dir := t.TempDir()
	// binaryPath does not exist → first os.Rename (current → rollback) fails.
	currentBin := filepath.Join(dir, "agent-does-not-exist")
	newBin := filepath.Join(dir, "new")
	_ = os.WriteFile(newBin, []byte("new"), 0755)

	client := &stubChecker{}
	u := NewUpdater(client, Config{Enabled: true, PublicKey: ed25519.PublicKey(make([]byte, 32))}, "1.0.0", "darwin", currentBin)

	err := u.atomicSwap(newBin)
	if err == nil || !errContains(err, "backup current binary") {
		t.Fatalf("expected backup-rename error, got %v", err)
	}
}

func TestAtomicSwap_InstallNewFails_RollbackRestoredSamePathTrick(t *testing.T) {
	// Force the SECOND rename (new → current) to fail by passing
	// newBinaryPath == binaryPath. Sequence:
	//   1) rename binaryPath → rollback   succeeds (file is moved away)
	//   2) rename newBinaryPath (== binaryPath) → binaryPath fails because
	//      the source no longer exists
	//   3) restore branch: rename rollback → binaryPath succeeds
	// Exercises both the install-new-fails error wrap AND the rollback
	// restoration path.
	dir := t.TempDir()
	currentBin := filepath.Join(dir, "agent")
	if err := os.WriteFile(currentBin, []byte("original"), 0755); err != nil {
		t.Fatal(err)
	}

	client := &stubChecker{}
	u := NewUpdater(client, Config{Enabled: true, PublicKey: ed25519.PublicKey(make([]byte, 32))}, "1.0.0", "darwin", currentBin)

	err := u.atomicSwap(currentBin) // same path = self-rename collision
	if err == nil {
		t.Fatal("expected install-new error when newBinaryPath == binaryPath")
	}
	if !errContains(err, "install new binary") {
		t.Fatalf("expected install-new-binary error wrap, got %v", err)
	}
	// Restoration path must have re-created binaryPath with original content.
	got, statErr := os.ReadFile(currentBin)
	if statErr != nil {
		t.Fatalf("binary should be restored from rollback: %v", statErr)
	}
	if string(got) != "original" {
		t.Fatalf("restored binary should hold original content, got %q", string(got))
	}
	// .rollback should be gone (consumed by the restore).
	if _, statErr := os.Stat(currentBin + ".rollback"); statErr == nil {
		t.Fatal(".rollback should have been consumed by restore-rename")
	}
}

func TestAtomicSwap_InstallNewFails_RollbackRestoredOnExistingPath(t *testing.T) {
	// On most filesystems os.Rename(src, dst) where dst is an existing directory fails.
	// We arrange for the second Rename (new → current) to fail by deleting newBin
	// after the first Rename succeeds — but that path is not reachable from the
	// public API. Instead we simulate by making binaryPath an existing directory
	// path so the second Rename fails (rename over a dir replaces semantics depend
	// on OS — on linux/darwin it errors when the target is a non-empty dir).
	dir := t.TempDir()
	currentBin := filepath.Join(dir, "agent")
	_ = os.WriteFile(currentBin, []byte("old"), 0755)

	// newBin is a directory, NOT a file → second rename fails on darwin/linux
	// because os.Rename can't rename a non-empty dir to a path where the dir
	// itself contains entries that would collide. We create it as a non-empty dir.
	newBin := filepath.Join(dir, "newdir")
	if err := os.Mkdir(newBin, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(newBin, "child"), []byte("c"), 0644); err != nil {
		t.Fatal(err)
	}

	// Pre-create a rollback file so the first Remove removes it (exercises that line).
	_ = os.WriteFile(currentBin+".rollback", []byte("stale"), 0644)

	client := &stubChecker{}
	u := NewUpdater(client, Config{Enabled: true, PublicKey: ed25519.PublicKey(make([]byte, 32))}, "1.0.0", "darwin", currentBin)

	err := u.atomicSwap(newBin)
	if err == nil {
		// On some filesystems this may succeed (rename a dir over a file).
		// Skip rather than fail — coverage still exercises the happy path.
		t.Skip("OS allowed renaming a non-empty dir over a file; install-new-fails branch not reachable here")
	}
	if !errContains(err, "install new binary") {
		t.Fatalf("expected install-new-binary error, got %v", err)
	}
	// After failure, binaryPath should be restored from .rollback (which holds
	// "old" — the backup of the original "old" content).
	if _, statErr := os.Stat(currentBin); statErr != nil {
		t.Fatalf("original binary should have been rolled back: %v", statErr)
	}
	got, _ := os.ReadFile(currentBin)
	if string(got) != "old" {
		t.Fatalf("rolled-back binary should hold original 'old' content, got %q", string(got))
	}
}

// fileSHA256 — open error

func TestFileSHA256_OpenError(t *testing.T) {
	_, err := fileSHA256("/nonexistent/path/that/does/not/exist/x")
	if err == nil {
		t.Fatal("expected open error")
	}
}

func TestFileSHA256_ReadError(t *testing.T) {
	// On Unix, os.Open succeeds on a directory but io.Copy reading from
	// the *os.File fails with "is a directory". Exercises the io.Copy
	// error branch in fileSHA256.
	if runtime.GOOS == "windows" {
		t.Skip("os.Open on a directory behaves differently on Windows")
	}
	dir := t.TempDir()
	_, err := fileSHA256(dir)
	if err == nil {
		t.Fatal("expected io.Copy error when reading a directory")
	}
}

// DetectCrashLoop — three negative branches and rollback-missing branch

func TestDetectCrashLoop_StatusFileMissing(t *testing.T) {
	dir := t.TempDir()
	if DetectCrashLoop(filepath.Join(dir, "agent"), filepath.Join(dir, "no-status"), time.Minute) {
		t.Fatal("must return false when status file missing")
	}
}

func TestDetectCrashLoop_StatusUnparseable(t *testing.T) {
	dir := t.TempDir()
	statusFile := filepath.Join(dir, "status")
	_ = os.WriteFile(statusFile, []byte("not-a-timestamp"), 0600)
	if DetectCrashLoop(filepath.Join(dir, "agent"), statusFile, time.Minute) {
		t.Fatal("must return false when status timestamp can't be parsed")
	}
}

func TestDetectCrashLoop_RecentStartButNoRollback(t *testing.T) {
	dir := t.TempDir()
	binaryPath := filepath.Join(dir, "agent")
	statusFile := filepath.Join(dir, "status")
	_ = os.WriteFile(binaryPath, []byte("new"), 0755)
	// recent start (within threshold), but NO rollback file exists.
	_ = os.WriteFile(statusFile, []byte(time.Now().Add(-1*time.Second).Format(time.RFC3339)), 0600)

	if DetectCrashLoop(binaryPath, statusFile, time.Minute) {
		t.Fatal("must return false when rollback binary is missing")
	}
	// Binary must NOT have been touched.
	got, _ := os.ReadFile(binaryPath)
	if string(got) != "new" {
		t.Fatalf("binary should be untouched when no rollback exists, got %q", string(got))
	}
}

// checkAvailabilityOnce — nil callback (no-op) + error path

func TestCheckAvailabilityOnce_NilCallback_NoOp(t *testing.T) {
	client := &recordingChecker{}
	u := NewUpdater(client, Config{Enabled: false, CheckInterval: time.Hour}, "1.0.0", "darwin", "/tmp/x")
	u.checkAvailabilityOnce(context.Background(), nil)
	if client.callCount() != 0 {
		t.Fatal("nil callback must short-circuit before any RPC")
	}
}

func TestCheckAvailabilityOnce_CheckerError_SilentSkip(t *testing.T) {
	client := &recordingChecker{errs: []error{errors.New("hub down")}}
	u := NewUpdater(client, Config{Enabled: false, CheckInterval: time.Hour}, "1.0.0", "darwin", "/tmp/x")

	var gotCalled atomic.Bool
	u.checkAvailabilityOnce(context.Background(), func(_ bool) {
		gotCalled.Store(true)
	})
	if gotCalled.Load() {
		t.Fatal("availableFn must NOT be invoked when CheckUpdate returns an error")
	}
	if client.callCount() != 1 {
		t.Fatalf("expected exactly 1 CheckUpdate call, got %d", client.callCount())
	}
}

func TestCheckAvailabilityOnce_Available_ForwardsToCallback(t *testing.T) {
	client := &recordingChecker{responses: []hub.UpdateInfo{{Available: true, Version: "2.0"}}}
	u := NewUpdater(client, Config{Enabled: false, CheckInterval: time.Hour}, "1.0.0", "darwin", "/tmp/x")

	var got atomic.Bool
	u.checkAvailabilityOnce(context.Background(), func(av bool) {
		got.Store(av)
	})
	if !got.Load() {
		t.Fatal("availableFn must receive true when Hub reports Available")
	}
}

// Run / RunWithAvailabilityCallback — periodic loop + ctx-cancel termination

func TestRun_DisabledCfg_CtxCancelTerminates(t *testing.T) {
	// disabled cfg → loop only calls checkAvailabilityOnce on ticks.
	client := &recordingChecker{responses: []hub.UpdateInfo{{Available: false}}}
	u := NewUpdater(client, Config{Enabled: false, CheckInterval: 10 * time.Millisecond}, "1.0.0", "darwin", "/tmp/x")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		u.Run(ctx) // exercises the nil-availableFn delegation path
		close(done)
	}()
	// Let at least one tick fire after the immediate check.
	time.Sleep(40 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

func TestRunWithAvailabilityCallback_AvailabilityForwardedOnTick(t *testing.T) {
	// Track availability calls.
	var availCalls atomic.Int32
	var lastVal atomic.Bool
	client := &recordingChecker{responses: []hub.UpdateInfo{{Available: true}}}
	u := NewUpdater(client, Config{Enabled: false, CheckInterval: 15 * time.Millisecond}, "1.0.0", "darwin", "/tmp/x")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		u.RunWithAvailabilityCallback(ctx, func(av bool) {
			availCalls.Add(1)
			lastVal.Store(av)
		})
		close(done)
	}()
	// Wait long enough for the immediate check + at least one tick.
	time.Sleep(60 * time.Millisecond)
	cancel()
	<-done

	if availCalls.Load() < 2 {
		t.Fatalf("expected ≥2 availability callbacks (immediate + tick), got %d", availCalls.Load())
	}
	if !lastVal.Load() {
		t.Fatal("availability callback must receive true when Hub reports Available")
	}
}

func TestRunWithAvailabilityCallback_EnabledAutoInstallReturnsAfterApply(t *testing.T) {
	// Set up full happy-path apply: signed binary, OK download, valid PublicKey.
	pub, priv, _ := ed25519.GenerateKey(nil)
	body := []byte("v2-binary-bytes")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	dir := t.TempDir()
	bin := filepath.Join(dir, "agent")
	_ = os.WriteFile(bin, []byte("v1"), 0755)

	// Immediate check (before ticker) returns Available=false so the loop
	// doesn't apply during the eager fire; ticker call returns Available=true.
	tickInfo := makeFullInfo(t, priv, "2.0", body, srv.URL)
	client := &recordingChecker{
		responses: []hub.UpdateInfo{
			{Available: false},
			tickInfo,
		},
	}
	// osName "linux" → in-place binary swap (this test asserts swapped content).
	u := NewUpdater(client, Config{Enabled: true, CheckInterval: 20 * time.Millisecond, PublicKey: pub}, "1.0.0", "linux", bin)

	// Track every availability call. The implementation contract: after a
	// successful auto-install, the loop calls availableFn(false) exactly
	// once before returning. So the LAST callback value before Run returns
	// must be false.
	var mu sync.Mutex
	var lastAvail bool
	var callCount int
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		u.RunWithAvailabilityCallback(ctx, func(av bool) {
			mu.Lock()
			lastAvail = av
			callCount++
			mu.Unlock()
		})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("Run did not return after a successful auto-install")
	}

	// After return, the new binary should be in place.
	got, _ := os.ReadFile(bin)
	if string(got) != string(body) {
		t.Fatalf("binary should be replaced post-apply, got %q", string(got))
	}
	// And availableFn(false) must have been the LAST call per the
	// implementation contract (post-apply downshift before return).
	mu.Lock()
	defer mu.Unlock()
	if callCount == 0 {
		t.Fatal("expected at least one availability callback")
	}
	if lastAvail {
		t.Fatalf("last availability callback should be false (post-apply downshift), got true after %d calls", callCount)
	}
}

func TestRunWithAvailabilityCallback_EnabledTickReportsNoUpdate(t *testing.T) {
	// Enabled + valid public key. Every CheckUpdate returns Available=false
	// so CheckAndUpdate returns (false, nil) on each tick — exercising the
	// `else { u.checkAvailabilityOnce(...) }` branch inside the enabled arm
	// of the periodic loop (the path that keeps the UI banner fresh when
	// auto-install is on but no update is queued).
	pub, _, _ := ed25519.GenerateKey(nil)
	client := &recordingChecker{responses: []hub.UpdateInfo{{Available: false}}}
	u := NewUpdater(client, Config{Enabled: true, CheckInterval: 10 * time.Millisecond, PublicKey: pub}, "1.0.0", "darwin", "/tmp/never-touched")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	var availCalls atomic.Int32
	go func() {
		u.RunWithAvailabilityCallback(ctx, func(_ bool) {
			availCalls.Add(1)
		})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx timeout")
	}
	// Immediate + at least one tick → ≥2 availability callbacks.
	if availCalls.Load() < 2 {
		t.Fatalf("expected ≥2 availability callbacks (immediate + ≥1 tick), got %d", availCalls.Load())
	}
	// CheckUpdate is called once per immediate + once per tick + once per
	// CheckAndUpdate-during-tick (Enabled arm calls it before falling to
	// checkAvailabilityOnce). So expect ≥3.
	if client.callCount() < 3 {
		t.Fatalf("expected ≥3 CheckUpdate calls (immediate + CheckAndUpdate + checkAvailabilityOnce per tick), got %d", client.callCount())
	}
}

func TestRunWithAvailabilityCallback_EnabledCheckErrorLoggedAndContinues(t *testing.T) {
	// Enabled + every CheckUpdate returns error → CheckAndUpdate fails →
	// loop logs and falls through to checkAvailabilityOnce. We just verify
	// the loop survives several ticks then exits on ctx cancel.
	pub, _, _ := ed25519.GenerateKey(nil)
	client := &recordingChecker{errs: []error{errors.New("hub down")}}
	u := NewUpdater(client, Config{Enabled: true, CheckInterval: 10 * time.Millisecond, PublicKey: pub}, "1.0.0", "darwin", "/tmp/never-touched")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		u.RunWithAvailabilityCallback(ctx, func(_ bool) {})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx timeout")
	}
	// At least one CheckUpdate call should have happened (the immediate one).
	if client.callCount() < 1 {
		t.Fatalf("expected at least one CheckUpdate call, got %d", client.callCount())
	}
}

// CheckAndUpdate — extra branches: !Available with enabled cfg, PublicKey==nil
// guard, atomicSwap failure inside the orchestrator.

func TestCheckAndUpdate_EnabledNoAvailable(t *testing.T) {
	// Enabled + valid public key, but Hub reports no update → early
	// return (false, nil). Distinct from TestCheckAndUpdate_NoUpdate
	// (which has PublicKey=nil and gets force-disabled by NewUpdater so
	// the path exits at the cfg.Enabled guard, not the !Available guard).
	pub, _, _ := ed25519.GenerateKey(nil)
	client := &stubChecker{info: hub.UpdateInfo{Available: false}}
	u := NewUpdater(client, Config{Enabled: true, CheckInterval: time.Hour, PublicKey: pub}, "1.0.0", "darwin", "/tmp/never-touched")
	updated, err := u.CheckAndUpdate(context.Background())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if updated {
		t.Fatal("must not be updated when Hub reports Available=false")
	}
}

func TestCheckAndUpdate_PublicKeyNil_Rejected(t *testing.T) {
	// NewUpdater force-disables when PublicKey is nil, so reach the
	// PublicKey-nil guard inside CheckAndUpdate by constructing the
	// struct directly (white-box test) with Enabled=true + PublicKey=nil.
	// The public key check now fires at the manifest-sig gate (pre-download),
	// so no .tmp file should be created.
	dir := t.TempDir()
	bin := filepath.Join(dir, "agent")
	_ = os.WriteFile(bin, []byte("old"), 0755)
	client := &stubChecker{info: hub.UpdateInfo{
		Available:   true,
		Version:     "2.0",
		DownloadURL: "http://localhost/agent",
		SHA256:      "abc123",
		Signature:   base64.StdEncoding.EncodeToString(make([]byte, 64)),
	}}
	u := &Updater{
		client:       client,
		downloadHTTP: client.HTTPClient(),
		cfg:          Config{Enabled: true, PublicKey: nil}, // bypass NewUpdater downgrade
		version:      "1.0.0",
		osName:       "darwin",
		binaryPath:   bin,
	}
	_, err := u.CheckAndUpdate(context.Background())
	if err == nil || !errContains(err, "no Ed25519 public key configured") {
		t.Fatalf("must reject with public-key-nil error; got %v", err)
	}
	// Original binary preserved.
	got, _ := os.ReadFile(bin)
	if string(got) != "old" {
		t.Fatalf("binary should be untouched, got %q", string(got))
	}
	// No download should have occurred — .tmp must not exist.
	if _, statErr := os.Stat(bin + ".tmp"); statErr == nil {
		t.Fatal(".tmp must not be created when public key is missing (pre-download gate)")
	}
}

func TestCheckAndUpdate_AtomicSwapFails(t *testing.T) {
	// Drive the full happy path until atomicSwap. We force the swap to
	// fail by pointing binaryPath at a non-existent file — atomicSwap's
	// first os.Rename (current → rollback) then errors, returning
	// "backup current binary" which CheckAndUpdate wraps as "apply update".
	// osName "linux" so applyUpdate routes to atomicSwap (the macOS path
	// would install a .pkg, which must never run in a test).
	pub, priv, _ := ed25519.GenerateKey(nil)
	body := []byte("v2-body")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	dir := t.TempDir()
	// binaryPath is INSIDE the temp dir but the file does not exist →
	// the .tmp download succeeds (parent dir exists), but the first
	// rename in atomicSwap fails because src doesn't exist.
	missingBin := filepath.Join(dir, "agent-never-installed")
	info := makeFullInfo(t, priv, "2.0.0", body, srv.URL)
	info.DownloadURL = srv.URL
	// Recompute manifest sig with corrected URL.
	hashHex, binSig := signBinaryContent(t, priv, body)
	manifestSig := signManifestFields(t, priv, "2.0.0", hashHex, srv.URL)
	client := &stubChecker{info: hub.UpdateInfo{
		Available:       true,
		Version:         "2.0.0",
		DownloadURL:     srv.URL,
		SHA256:          hashHex,
		Signature:       manifestSig,
		BinarySignature: binSig,
	}}
	u := NewUpdater(client, Config{Enabled: true, PublicKey: pub}, "1.0.0", "linux", missingBin)

	_, err := u.CheckAndUpdate(context.Background())
	if err == nil || !errContains(err, "apply update") {
		t.Fatalf("expected apply-update wrapper; got %v", err)
	}
	// tmp must be cleaned up.
	if _, statErr := os.Stat(missingBin + ".tmp"); statErr == nil {
		t.Fatal(".tmp file must be removed after atomic-swap failure")
	}
}

// WriteStartStatus — write error path (read-only dir)

func TestWriteStartStatus_WriteError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("read-only-dir semantics differ on Windows")
	}
	dir := t.TempDir()
	roDir := filepath.Join(dir, "ro")
	if err := os.Mkdir(roDir, 0500); err != nil {
		t.Fatal(err)
	}
	// Path inside a read-only directory → WriteFile fails.
	err := WriteStartStatus(filepath.Join(roDir, "status"))
	if err == nil {
		t.Fatal("expected write error in read-only directory")
	}
}
