//go:build darwin

// Package ne contains the Network Extension Unix-socket JSON protocol types
// and the socket path helper used by the macOS DarwinPlatform.
package ne

import (
	"os"
	"path/filepath"
)

// neScannerMaxBytes caps the per-line buffer used to receive NE IPC
// messages. The Network Extension wraps each captured HTTP body in a
// base64-encoded JSON line, which inflates the wire size to ~4/3 the raw
// body plus envelope overhead. The default 64 KiB Scanner cap silently
// dropped any flow_inspect / flow_inspect_response carrying a body larger
// than ~46 KiB and killed the entire NE connection, which is squarely in
// the size range typical AI requests/responses hit. We size the buffer
// large enough to cover the agent's configured maxBodyBytes (1 MiB
// default) plus base64 + JSON inflation.
const ScannerMaxBytes = 16 * 1024 * 1024

// FlowMsg is a JSON message from the Network Extension.
type FlowMsg struct {
	Type       string `json:"type"`                 // "flow_new" | "flow_closed" | "flow_inspect" | "flow_inspect_response" | "flow_update_host"
	FlowID     string `json:"flowId"`               // unique flow identifier
	Hostname   string `json:"hostname,omitempty"`   // SNI hostname for flow_update_host
	RemoteHost string `json:"remoteHost,omitempty"` // SNI / resolved hostname
	RemoteIP   string `json:"remoteIp,omitempty"`   // destination IP
	RemotePort int    `json:"remotePort,omitempty"` // destination port
	LocalPort  int    `json:"localPort,omitempty"`  // source port
	PID        int    `json:"pid,omitempty"`        // source process PID
	Protocol   string `json:"protocol,omitempty"`   // "tcp" | "udp"
	BytesIn    int64  `json:"bytesIn,omitempty"`    // set on flow_closed
	BytesOut   int64  `json:"bytesOut,omitempty"`   // set on flow_closed
	DurationMs int    `json:"durationMs,omitempty"` // set on flow_closed
	BumpStatus string `json:"bumpStatus,omitempty"` // set on flow_closed
	// Latency phase fields populated by the Swift NE proxy when
	// URLSessionTaskMetrics surface for this flow. Nil/absent when the
	// flow used a non-URLSession path (raw TCP relay / passthrough).
	UpstreamTtfbMs  *int                `json:"upstreamTtfbMs,omitempty"`
	UpstreamTotalMs *int                `json:"upstreamTotalMs,omitempty"`
	InterceptMs     *int                `json:"interceptMs,omitempty"`
	Body            string              `json:"body,omitempty"`         // base64-encoded request/response body
	Method          string              `json:"method,omitempty"`       // HTTP method
	Path            string              `json:"path,omitempty"`         // HTTP request path
	Headers         map[string][]string `json:"headers,omitempty"`      // HTTP request headers (flow_inspect only)
	HookDecision    string              `json:"hookDecision,omitempty"` // from NE on flow_closed
	HookReason      string              `json:"hookReason,omitempty"`
	HookReasonCode  string              `json:"hookReasonCode,omitempty"`
	ComplianceTags  []string            `json:"complianceTags,omitempty"` // from NE on flow_closed
}

// DecisionMsg is the JSON response sent back to the Network Extension.
type DecisionMsg struct {
	FlowID   string `json:"flowId"`
	Decision string `json:"decision"` // "inspect" | "passthrough" | "deny"
}

// InspectResult is the JSON response for flow_inspect / flow_inspect_response.
type InspectResult struct {
	FlowID         string   `json:"flowId"`
	Decision       string   `json:"decision"`
	Reason         string   `json:"reason,omitempty"`
	ReasonCode     string   `json:"reasonCode,omitempty"`
	ComplianceTags []string `json:"complianceTags,omitempty"`
}

// SocketPath returns the path to the NE Unix socket.
// Prefers /var/run for system-level agents, falls back to user-local.
func SocketPath() string {
	if os.Getuid() == 0 {
		return "/var/run/nexus-agent/ne.sock"
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "/tmp/nexus-agent-ne.sock"
	}
	return filepath.Join(home, ".nexus", "ne.sock")
}
