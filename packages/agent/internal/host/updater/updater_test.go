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

func TestCheckAndUpdate_FullFlow_WithDownloadAndSwap(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	binaryContent := []byte("#!/bin/sh\necho new-version")
	hash := sha256.Sum256(binaryContent)
	hashHex := hex.EncodeToString(hash[:])
	sig := ed25519.Sign(priv, hash[:])
	sigB64 := base64.StdEncoding.EncodeToString(sig)

	// Download server
	dlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(binaryContent)
	}))
	defer dlSrv.Close()

	dir := t.TempDir()
	currentBin := filepath.Join(dir, "agent")
	_ = os.WriteFile(currentBin, []byte("old-binary"), 0755)

	client := &stubChecker{info: hub.UpdateInfo{
		Available:   true,
		Version:     "2.0.0",
		DownloadURL: dlSrv.URL + "/agent",
		SHA256:      hashHex,
		Signature:   sigB64,
	}}
	u := NewUpdater(client, Config{Enabled: true, CheckInterval: time.Hour, PublicKey: pub}, "1.0.0", "darwin", currentBin)

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
}

func TestCheckAndUpdate_SHA256Mismatch(t *testing.T) {
	dlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("actual content"))
	}))
	defer dlSrv.Close()

	dir := t.TempDir()
	currentBin := filepath.Join(dir, "agent")
	_ = os.WriteFile(currentBin, []byte("old"), 0755)

	pub, _, _ := ed25519.GenerateKey(nil)
	client := &stubChecker{info: hub.UpdateInfo{
		Available:   true,
		Version:     "2.0.0",
		DownloadURL: dlSrv.URL + "/agent",
		SHA256:      "0000000000000000000000000000000000000000000000000000000000000000",
	}}
	u := NewUpdater(client, Config{Enabled: true, CheckInterval: time.Hour, PublicKey: pub}, "1.0.0", "darwin", currentBin)

	_, err := u.CheckAndUpdate(context.Background())
	if err == nil {
		t.Fatal("expected SHA-256 mismatch error")
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
