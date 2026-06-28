//go:build darwin

package ne

import (
	"errors"
	"github.com/goccy/go-json"
	"testing"
)

// TestFlowMsg_DecodesBundleID locks the NE→daemon wire contract for the
// bundle-attribution fix: the Swift extension encodes the source app's
// signing identifier under the JSON key "bundleId" (NEFlowNewMessage),
// and the daemon must decode it into FlowMsg.BundleID. A drift in either
// the key name or the struct tag silently drops attribution back to the
// racy PID lookup, so assert the exact wire shape here.
func TestFlowMsg_DecodesBundleID(t *testing.T) {
	// Exactly the frame the Swift NEFlowNewMessage.encode produces.
	raw := `{"type":"flow_new","flowId":"f1","remoteHost":"chatgpt.com","remoteIp":"1.2.3.4","remotePort":443,"localPort":51000,"pid":4242,"bundleId":"com.github.Electron.helper","protocol":"tcp"}`
	var m FlowMsg
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("unmarshal flow_new: %v", err)
	}
	if m.BundleID != "com.github.Electron.helper" {
		t.Fatalf("BundleID = %q, want com.github.Electron.helper (wire key drift?)", m.BundleID)
	}
	if m.PID != 4242 || m.RemoteHost != "chatgpt.com" {
		t.Fatalf("sibling fields mis-decoded: %+v", m)
	}

	// Absent bundleId (unsigned binary path) must decode to empty, not error.
	var m2 FlowMsg
	if err := json.Unmarshal([]byte(`{"type":"flow_new","flowId":"f2","pid":7}`), &m2); err != nil {
		t.Fatalf("unmarshal without bundleId: %v", err)
	}
	if m2.BundleID != "" {
		t.Fatalf("absent bundleId must be empty, got %q", m2.BundleID)
	}
}

func TestSocketPath(t *testing.T) {
	origUID, origHome := osGetuid, osUserHomeDir
	t.Cleanup(func() { osGetuid, osUserHomeDir = origUID, origHome })

	// Root → system-wide /var/run socket.
	osGetuid = func() int { return 0 }
	if got := SocketPath(); got != "/var/run/nexus-agent/ne.sock" {
		t.Fatalf("root: got %q", got)
	}

	// Non-root with a home dir → user-local ~/.nexus/ne.sock.
	osGetuid = func() int { return 501 }
	osUserHomeDir = func() (string, error) { return "/Users/alice", nil }
	if got := SocketPath(); got != "/Users/alice/.nexus/ne.sock" {
		t.Fatalf("user: got %q", got)
	}

	// Non-root, no resolvable home → /tmp fallback.
	osUserHomeDir = func() (string, error) { return "", errors.New("no home") }
	if got := SocketPath(); got != "/tmp/nexus-agent-ne.sock" {
		t.Fatalf("no-home fallback: got %q", got)
	}
}
