//go:build windows

package status

import (
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
)

var testPipeSeq atomic.Uint64

// testEndpoint returns a unique Windows named-pipe name for a test listener. The
// production status API listens on a named pipe on Windows (see
// statusapi_listen_windows.go), so tests must use a pipe name too — a temp .sock
// path would make winio.ListenPipe fail and dials would hit no listener.
func testEndpoint(t *testing.T) string {
	t.Helper()
	safe := strings.NewReplacer("/", "_", `\`, "_", " ", "_").Replace(t.Name())
	return fmt.Sprintf(`\\.\pipe\nexus-status-test-%s-%d`, safe, testPipeSeq.Add(1))
}

// dialEndpoint connects to a test status endpoint — a named pipe on Windows,
// matching platformListen in statusapi_listen_windows.go. winio.DialPipe waits
// for the pipe to appear (up to the timeout), so it tolerates the listener
// coming up slightly after the dial.
func dialEndpoint(path string) (net.Conn, error) {
	timeout := 2 * time.Second
	return winio.DialPipe(path, &timeout)
}
