package skills

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestStdHTTPGetErrors covers the real getter's two failure modes: an unparseable
// request URL and an unreachable host. Both must surface as errors, not panics, so
// Preview/Install report a clean download failure.
func TestStdHTTPGetErrors(t *testing.T) {
	h := newStdHTTP()
	// A control character makes http.NewRequestWithContext fail to build the request.
	if _, _, err := h.Get(context.Background(), "http://exa\x7fmple"); err == nil {
		t.Fatal("a malformed request URL must error")
	}
	// Port 0 on loopback is never listening, so the transport Do fails to connect.
	if _, _, err := h.Get(context.Background(), "http://127.0.0.1:0/skill.md"); err == nil {
		t.Fatal("an unreachable host must surface a transport error")
	}
}

// TestInstallFetchedWriteError: when the destination path cannot be written (a
// directory already occupies the skill's file name), InstallFetched reports it.
func TestInstallFetchedWriteError(t *testing.T) {
	dir := t.TempDir()
	// Occupy "blocked.md" with a directory so WriteFile to that path fails.
	if err := os.Mkdir(filepath.Join(dir, "blocked.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	info := InstallInfo{Name: "blocked", Raw: []byte("---\nname: blocked\ndescription: y\n---\nbody\n")}
	if _, err := InstallFetched(info, dir); err == nil || !strings.Contains(err.Error(), "write skill") {
		t.Fatalf("a non-writable destination must error with 'write skill', got %v", err)
	}
}

// TestLoadSkipsUnreadableLocalFile: an unreadable local .md is skipped (not fatal),
// and the built-ins plus the readable local skills still load.
func TestLoadSkipsUnreadableLocalFile(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("file-permission denial is not reliable as root / on Windows")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "good.md"),
		[]byte("---\nname: good\ndescription: ok\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(dir, "bad.md")
	if err := os.WriteFile(bad, []byte("---\nname: bad\ndescription: x\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(bad, 0o000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(bad, 0o644) //nolint:errcheck // best-effort restore for cleanup

	set, err := Load(dir)
	if err != nil {
		t.Fatalf("an unreadable local file must not fail the load, got %v", err)
	}
	if _, ok := set.Get("good"); !ok {
		t.Fatal("the readable local skill must still load")
	}
	if _, ok := set.Get("incident-triage"); !ok {
		t.Fatal("built-ins must still load alongside")
	}
}
