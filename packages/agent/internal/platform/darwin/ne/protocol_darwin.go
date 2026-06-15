//go:build darwin

// Package ne contains the Network Extension Unix-socket JSON protocol types
// and the socket path helper used by the macOS DarwinPlatform.
package ne

import (
	"os"
	"path/filepath"
)

// ScannerMaxBytes caps the per-line buffer used to receive NE IPC
// messages. The live messages (flow_new / flow_closed / flow_update_host)
// are small JSON metadata frames — no request/response body travels over
// this socket; inspect bodies go through the loopback bump bridge, not
// here. The cap is generous headroom so an unexpectedly large frame is
// buffered rather than silently truncated at the default 64 KiB Scanner
// limit (a truncated line would kill the whole NE connection mid-stream).
const ScannerMaxBytes = 16 * 1024 * 1024

// FlowMsg is a JSON message from the Network Extension.
type FlowMsg struct {
	Type       string `json:"type"`                 // "flow_new" | "flow_closed" | "flow_update_host"
	FlowID     string `json:"flowId"`               // unique flow identifier
	Hostname   string `json:"hostname,omitempty"`   // SNI hostname for flow_update_host
	RemoteHost string `json:"remoteHost,omitempty"` // SNI / resolved hostname
	RemoteIP   string `json:"remoteIp,omitempty"`   // destination IP
	RemotePort int    `json:"remotePort,omitempty"` // destination port
	LocalPort  int    `json:"localPort,omitempty"`  // source port
	PID        int    `json:"pid,omitempty"`        // source process PID
	BundleID   string `json:"bundleId,omitempty"`   // kernel-attested source-app signing identifier (preferred over PID lookup)
	Protocol   string `json:"protocol,omitempty"`   // "tcp" | "udp"
	BytesIn    int64  `json:"bytesIn,omitempty"`    // set on flow_closed
	BytesOut   int64  `json:"bytesOut,omitempty"`   // set on flow_closed
	DurationMs int    `json:"durationMs,omitempty"` // set on flow_closed
	BumpStatus string `json:"bumpStatus,omitempty"` // set on flow_closed
	// Latency phase fields populated by the Swift NE proxy when
	// URLSessionTaskMetrics surface for this flow. Nil/absent when the
	// flow used a non-URLSession path (raw TCP relay / passthrough).
	UpstreamTtfbMs  *int `json:"upstreamTtfbMs,omitempty"`
	UpstreamTotalMs *int `json:"upstreamTotalMs,omitempty"`
	InterceptMs     *int `json:"interceptMs,omitempty"`
}

// DecisionMsg is the JSON response sent back to the Network Extension.
type DecisionMsg struct {
	FlowID   string `json:"flowId"`
	Decision string `json:"decision"` // "inspect" | "passthrough" | "deny"
}

// SocketPath returns the path to the NE Unix socket.
// Prefers /var/run for system-level agents, falls back to user-local.
// Seams over the OS calls SocketPath depends on, so the root / user / no-home
// branches are unit-testable without running as root or unsetting $HOME.
var (
	osGetuid      = os.Getuid
	osUserHomeDir = os.UserHomeDir
)

func SocketPath() string {
	if osGetuid() == 0 {
		return "/var/run/nexus-agent/ne.sock"
	}
	home, err := osUserHomeDir()
	if err != nil {
		return "/tmp/nexus-agent-ne.sock"
	}
	return filepath.Join(home, ".nexus", "ne.sock")
}
