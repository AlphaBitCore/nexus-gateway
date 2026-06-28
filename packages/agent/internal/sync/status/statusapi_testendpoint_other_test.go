//go:build !windows

package status

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

// testEndpoint returns a unique Unix-socket path for a test listener. It uses a
// short os.TempDir path (not the deeply-nested t.TempDir) to stay within macOS's
// 104-char sun_path limit.
func testEndpoint(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "sa-")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "test.sock")
}

// dialEndpoint connects to a test status endpoint — a Unix socket on non-Windows
// platforms, matching platformListen in statusapi_listen_other.go.
func dialEndpoint(path string) (net.Conn, error) {
	return net.Dial("unix", path)
}
