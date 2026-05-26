package auth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadDeviceToken_HappyPath(t *testing.T) {
	dir := t.TempDir()
	token := "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	if err := os.WriteFile(filepath.Join(dir, "device-token"), []byte(token), 0600); err != nil {
		t.Fatalf("seed device-token: %v", err)
	}
	got, err := LoadDeviceToken(dir)
	if err != nil {
		t.Fatalf("LoadDeviceToken: %v", err)
	}
	if got != token {
		t.Fatalf("got %q, want %q", got, token)
	}
}

func TestLoadDeviceToken_Missing(t *testing.T) {
	dir := t.TempDir()
	_, err := LoadDeviceToken(dir)
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestLoadDeviceToken_InvalidLength(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "device-token"), []byte("tooshort"), 0600)
	_, err := LoadDeviceToken(dir)
	if err == nil {
		t.Fatal("expected error for invalid length")
	}
}

func TestClearEnrollment_RemovesBothFiles(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"device-token", "thing-id"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0600); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	if err := ClearEnrollment(dir); err != nil {
		t.Fatalf("ClearEnrollment: %v", err)
	}
	for _, name := range []string{"device-token", "thing-id"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("%s should be removed (err=%v)", name, err)
		}
	}
}

func TestClearEnrollment_MissingFilesNotError(t *testing.T) {
	dir := t.TempDir()
	// device-token + thing-id don't exist; should return nil (idempotent).
	if err := ClearEnrollment(dir); err != nil {
		t.Fatalf("ClearEnrollment on empty dir: %v", err)
	}
}

func TestClearEnrollment_PermissionDeniedWrapsError(t *testing.T) {
	// Cover the non-ENOENT branch of os.Remove → fmt.Errorf wrap.
	// Strategy: make a parent dir read-only so Remove on a child fails
	// with EACCES (not ENOENT).
	parent := t.TempDir()
	tokenPath := filepath.Join(parent, "device-token")
	if err := os.WriteFile(tokenPath, []byte("x"), 0600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Strip write permission from parent so Remove can't unlink.
	if err := os.Chmod(parent, 0500); err != nil {
		t.Fatalf("chmod parent: %v", err)
	}
	// Restore for cleanup.
	t.Cleanup(func() { _ = os.Chmod(parent, 0700) })

	err := ClearEnrollment(parent)
	if err == nil {
		t.Skip("platform allowed unlink with read-only parent — skip non-ENOENT branch coverage")
	}
	// Don't assert the exact message; the wrap format is implementation
	// detail. Just confirm the wrap fired and surfaced the file name.
	if !strings.Contains(err.Error(), "device-token") {
		t.Fatalf("expected error to mention device-token, got: %v", err)
	}
}
